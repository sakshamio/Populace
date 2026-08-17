// Command populace serves the world, the dynamics, and the renderer.
//
// The simulation runs on a single goroutine behind a mutex, and that is a
// deliberate choice rather than a shortcut. A tick reads and writes every
// parallel array in the population; letting HTTP handlers touch it concurrently
// would mean either locking per persona or accepting torn state, and both are
// worse than serialising a 23 ms operation. The parallelism that matters is
// inside the tick, where it is over disjoint index ranges.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sakshamio/Populace/internal/experiment"
	"github.com/sakshamio/Populace/internal/llm"
	"github.com/sakshamio/Populace/internal/reaction"
	"github.com/sakshamio/Populace/internal/sim"
	"github.com/sakshamio/Populace/internal/world"
)

type server struct {
	mu      sync.Mutex
	w       *world.World
	s       *sim.Sim
	cfg     sim.Config
	running bool

	// speed multiplies the base tick interval; steps is a one-shot budget that
	// lets a paused clock advance exactly n ticks. Both live under mu with the
	// simulation itself, because a control that raced the tick loop could
	// advance twice for one click and there would be no way to tell from the
	// outside.
	speed float64
	steps int

	lastEv    string
	lastWhere string
	story     reaction.Story

	relay *reaction.Relay
	// gateway is recorded so the UI can show whether the model is reachable
	// at all -- "no reactions yet" and "the Spark is off" look identical
	// otherwise, and only one of them is worth doing something about.
	gatewayURL string
	relayConc  int
	applyW     float64

	exp *experiment.Runner

	// placeIdx is built once at startup so /api/street never scans the whole
	// population -- see world.BuildPlaceIndex.
	placeIdx *world.PlaceIndex
	// street is the one roster currently on screen, if any. Kept fixed across
	// polls (only Refresh runs) so a viewer watches the same named people's
	// state change rather than a different draw shuffling in underneath them;
	// a new place or an explicit reroll replaces it outright.
	street    *sim.Street
	streetGen uint64
}

// startedAt is process start, reported so an operator can tell a server that
// has been up for a week from one that restarted a minute ago -- which are
// indistinguishable from any other number on the dashboard.
var startedAt = time.Now()

// reactionSource bridges the chat's decoupled sim.ReactionSource interface to
// this server's model relay, so the group chat's first-reaction messages can
// use a real model line when one is cached for a friend's archetype.
//
// It reads srv.relay and srv.story directly rather than through storyFP(),
// which takes srv.mu -- and must not, here. Reaction is only ever called from
// inside Sim.Advance(), which the tick loop always runs with srv.mu already
// held (see tickOnce in resilience.go); a second Lock() from the same
// goroutine would deadlock the tick loop permanently, with no panic and
// nothing in the log, since sync.Mutex is not reentrant. If this type is ever
// wired in anywhere Advance() runs without the lock held, this comment is
// wrong and the read below is a race.
type reactionSource struct{ srv *server }

func (rs reactionSource) Reaction(archetype int) (string, bool) {
	if rs.srv.relay == nil {
		return "", false
	}
	rx, ok := rs.srv.relay.Get(reaction.Key{Archetype: archetype, Story: rs.srv.story.Fingerprint()})
	if !ok {
		return "", false
	}
	return rx.Line, true
}

func main() {
	var (
		addr = flag.String("addr", ":"+envOr("PORT", "8080"), "listen address")
		// Env fallbacks on the sizing knobs, because a platform deploy
		// configures with environment variables and cannot easily pass flags.
		// A container stuck at whatever default was compiled in is a container
		// that OOMs on a small instance with no way to turn it down.
		n       = flag.Int("n", envInt("POPULACE_N", 1_000_000), "personas to generate at startup")
		seed    = flag.Uint64("seed", uint64(envInt("POPULACE_SEED", 20260815)), "world seed")
		webDir  = flag.String("web", "web", "static assets directory")
		maxSend = flag.Int("max", 10_000_000, "cap on personas served per request")
		tickMS  = flag.Int("tick", envInt("POPULACE_TICK_MS", 400), "milliseconds between simulation ticks")
		run     = flag.Bool("run", true, "start ticking immediately")

		daynight      = flag.Bool("daynight", true, "gate contagion by a diurnal activity rhythm")
		daynightMin   = flag.Float64("daynight-minutes", 10, "simulated minutes per tick, for the day/night clock")
		daynightHour0 = flag.Float64("daynight-start", 9, "UTC hour the day/night clock reads at tick 0")

		gwURL   = flag.String("gateway", envOr("LLM_GATEWAY_URL", ""), "model gateway base URL; empty disables generation")
		gwTok   = flag.String("token", os.Getenv("LLM_TOKEN"), "bearer token for the gateway")
		model   = flag.String("model", envOr("LLM_MODEL", "default"), "model name to request")
		rxCache = flag.String("reactions", "reactions.json", "path to the reaction cache")
		rxConc  = flag.Int("relay-conc", 6, "concurrent generations; the gateway reserves 6 of 8 for batch")
		applyW  = flag.Float64("apply", 1.0, "how far model output moves priors, 0..1")
	)
	flag.Parse()

	log.Printf("generating %s personas (seed %d)...", commas(*n), *seed)
	t0 := time.Now()
	w := world.Generate(*n, *seed)
	log.Printf("population in %s", time.Since(t0).Round(time.Millisecond))

	counts := w.StratumCounts()
	for i := 0; i < int(world.NumStrata); i++ {
		log.Printf("  %-15s %9s  weight %.4f",
			world.Strata[i].Name, commas(counts[i]), world.Stratum(i).Weight())
	}

	cfg := sim.DefaultConfig()
	if *daynight {
		cfg.DayNight = sim.DefaultDayNightConfig()
		cfg.DayNight.MinutesPerTick = *daynightMin
		cfg.DayNight.StartHourUTC = *daynightHour0
	}
	t0 = time.Now()
	s := sim.New(w, cfg)
	log.Printf("social graph in %s — %s edges, mean degree %.1f",
		time.Since(t0).Round(time.Millisecond), commas(int(s.G.Edges)),
		s.G.DegreeStats(nil).Mean)

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	log.Printf("heap %.2f GB for population and network", float64(ms.HeapAlloc)/1e9)

	srv := &server{w: w, s: s, cfg: cfg, running: *run,
		gatewayURL: *gwURL, relayConc: *rxConc, applyW: *applyW,
		speed: 1, exp: experiment.NewRunner(),
		placeIdx: world.BuildPlaceIndex(w)}
	// Wired unconditionally: reactionSource.Reaction checks srv.relay itself
	// and returns false with no model configured, which is exactly the
	// fallback-to-canned-lines behaviour the chat already had.
	s.ReactionSource = reactionSource{srv}
	if *gwURL != "" {
		client := llm.New(*gwURL, *gwTok)
		srv.relay = reaction.New(client, *model, *rxCache)
		log.Printf("model gateway %s (model %q, cache %s)", *gwURL, *model, *rxCache)
		// Probe once, in the background, and only log the result. The gateway
		// being unreachable is not a reason to refuse to boot -- the simulation
		// is the product and it runs without the model. But finding out at
		// startup beats finding out when a user clicks Generate and waits.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := client.Check(ctx); err != nil {
				log.Printf("model gateway unreachable at startup: %v", err)
				log.Printf("the simulation runs; generation will fail until this clears")
				return
			}
			log.Printf("model gateway reachable")
		}()
	} else {
		log.Printf("no -gateway set: the simulation runs, but nobody has an opinion " +
			"the model wrote")
	}
	go srv.loop(time.Duration(*tickMS) * time.Millisecond)

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir(*webDir)))
	mux.HandleFunc("/api/instances", srv.instances(*maxSend))
	mux.HandleFunc("/api/state", srv.state)
	mux.HandleFunc("/api/event", srv.event)
	mux.HandleFunc("/api/stats", srv.stats)
	mux.HandleFunc("/api/control", srv.control)
	mux.HandleFunc("/api/archetypes", srv.archetypes)
	mux.HandleFunc("/api/breakdown", srv.breakdown)
	mux.HandleFunc("/api/history", srv.history)
	mux.HandleFunc("/api/relay", srv.relayStatus)
	mux.HandleFunc("/api/generate", srv.generate)
	mux.HandleFunc("/api/reactions", srv.reactions)
	mux.HandleFunc("/api/persona", srv.persona)
	mux.HandleFunc("/api/media", srv.mediaStatus)
	mux.HandleFunc("/api/chat", srv.chat)
	mux.HandleFunc("/api/cohort", srv.cohort)
	mux.HandleFunc("/api/street", srv.streetView)
	mux.HandleFunc("/api/experiment", srv.experimentStatus)
	mux.HandleFunc("/api/experiment/run", srv.experimentRun)
	mux.HandleFunc("/healthz", func(rw http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(rw, "ok")
	})

	log.Printf("listening on %s (tick %dms, running=%v)", *addr, *tickMS, *run)
	log.Fatal((&http.Server{
		Addr: *addr, Handler: recoverHTTP(mux), ReadHeaderTimeout: 10 * time.Second,
	}).ListenAndServe())
}

func (srv *server) instances(maxSend int) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		want := srv.w.N
		if q := r.URL.Query().Get("n"); q != "" {
			if v, err := strconv.Atoi(q); err == nil && v > 0 {
				want = v
			}
		}
		want = min(min(want, srv.w.N), maxSend)

		// Serving a prefix is a valid subsample precisely because generation is
		// per-persona deterministic and independent -- index order carries no
		// structure. The social graph is built over the whole population, so a
		// prefix view renders a sample of a world that is simulated in full.
		srv.mu.Lock()
		view := *srv.w
		view.N = want
		rw.Header().Set("Content-Type", "application/octet-stream")
		rw.Header().Set("Content-Length", strconv.Itoa(8+want*world.InstanceBytes))
		start := time.Now()
		err := view.EncodeInstances(rw)
		srv.mu.Unlock()

		if err != nil {
			log.Printf("encode: %v", err)
			return
		}
		log.Printf("served %s instances (%.1f MB) in %s", commas(want),
			float64(want*world.InstanceBytes)/1e6, time.Since(start).Round(time.Millisecond))
	}
}

// state returns whichever frame is smaller; the client dispatches on the magic.
func (srv *server) state(rw http.ResponseWriter, r *http.Request) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	rw.Header().Set("Content-Type", "application/octet-stream")
	rw.Header().Set("Cache-Control", "no-store")
	if _, _, err := srv.s.Encode(rw); err != nil {
		log.Printf("state: %v", err)
	}
}

type eventReq struct {
	Headline   string  `json:"headline"`
	Stance     float64 `json:"stance"`
	Salience   float64 `json:"salience"`
	Seeding    string  `json:"seeding"` // region | network | scattered
	SeedSize   int     `json:"seed_size"`
	Seed       uint64  `json:"seed"`
	Difficulty float64 `json:"difficulty"` // <1 cheap to act on, >1 costly

	// Hour sets the simulated UTC clock at the moment this story breaks --
	// "introduce the news at 3am" rather than whatever the clock already
	// reads. A pointer so "not sent" (leave the clock alone) is
	// distinguishable from "midnight". Ignored when day/night is off.
	Hour *float64 `json:"hour"`
}

func (srv *server) event(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req eventReq
	if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(rw, "bad request body", http.StatusBadRequest)
		return
	}
	if req.Seed == 0 {
		req.Seed = uint64(time.Now().UnixNano())
	}
	if req.Salience == 0 {
		req.Salience = 0.6
	}
	if req.SeedSize == 0 {
		req.SeedSize = 300
	}

	srv.mu.Lock()
	seeded := srv.s.Inject(sim.Event{
		Headline: req.Headline, Stance: float32(clamp1(req.Stance)),
		Salience: req.Salience, Seeding: seedingOf(req.Seeding),
		SeedSize: req.SeedSize, Seed: req.Seed, Difficulty: req.Difficulty,
		HasHour: req.Hour != nil, Hour: derefHour(req.Hour),
	})
	// Where it broke, taken from the seed set rather than chosen separately --
	// two sources for the same fact is two chances to disagree about it.
	where := "nowhere"
	if len(seeded) > 0 {
		where = srv.w.PlaceName(int(seeded[0]))
	}
	srv.lastEv = req.Headline
	srv.lastWhere = where
	// Set immediately rather than waiting for /api/generate: this is the
	// fingerprint reactionSource looks up under, and the chat's first
	// messages can start arriving within a handful of ticks -- long before a
	// person clicks "Ask the model", if they ever do.
	story := reaction.Story{Headline: req.Headline, Stance: req.Stance}
	srv.story = story
	// Captured under the same lock that guards Chat.Friends, then used after
	// unlocking -- the background generation below must not hold srv.mu for
	// however long the model takes to answer.
	var warmArchetypes []int
	if srv.relay != nil && srv.s.Chat != nil {
		warmArchetypes = srv.s.Chat.Archetypes(srv.w)
	}
	snap := srv.s.Snapshot(false)
	srv.mu.Unlock()

	log.Printf("event %q stance %+.2f salience %.2f: seeded %d in %s (%s)",
		req.Headline, req.Stance, req.Salience, len(seeded), where, req.Seeding)

	// Warm just the six friends' archetypes, not the population. This is what
	// makes the chat's first reactions live by default rather than only after
	// someone clicks "Ask the model": at most 6 archetypes (often fewer, since
	// a small cohort sharing a place or a job usually shares one outright)
	// against the ~450 a full run walks, so the wall-clock cost is seconds,
	// not minutes, and it is well inside "the model authors content per
	// archetype" -- this is that, just triggered by the story breaking instead
	// of by a click. It only populates the cache; it deliberately does not
	// call ApplyReactions, so breaking a story never moves anyone's opinion or
	// threshold on its own -- that stays something only "Ask the model" does.
	//
	// relay.Run refuses a second concurrent call outright, so this can race a
	// population-wide "Ask the model" run that is still in flight and simply
	// fail -- logged, not surfaced to the client, exactly how the same
	// collision already behaves in the generate handler below. Harmless: the
	// full run's own pass covers these archetypes too by the time it finishes.
	if len(warmArchetypes) > 0 {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			err := srv.relay.Run(ctx, story, warmArchetypes, len(warmArchetypes), nil)
			p := srv.relay.Progress()
			log.Printf("chat reactions warmed: %d archetypes, %d from cache, %.1fs, err=%v",
				len(warmArchetypes), p.FromCache, p.ElapsedS, err)

			// The actual conversation, not six independent opinions -- see
			// reaction.Dialogue. Grounded in whatever just landed above: each
			// member carries their own archetype's stance if the warm-up found
			// one, so the exchange is consistent with what the model already
			// said this kind of person thinks rather than a second,
			// disconnected read of them.
			//
			// story is a snapshot from before this goroutine started; srv.story
			// may have moved on to a newer headline by the time we get here, in
			// which case none of this should be applied. Checked twice -- once
			// before spending the tokens, once before installing the result --
			// because a second story can break in the gap either way.
			srv.mu.Lock()
			current := srv.story.Fingerprint() == story.Fingerprint()
			var members []reaction.DialogueMember
			var relationship string
			if current && srv.s.Chat != nil {
				relationship = srv.s.Chat.Sketch()
				for _, f := range srv.s.Chat.Friends {
					arch := int(world.Archetype(srv.w.Arch[f.ID]))
					m := reaction.DialogueMember{Name: f.Name, Role: f.Role, Place: f.Place, Region: f.Region}
					if rx, ok := srv.relay.Get(reaction.Key{Archetype: arch, Story: story.Fingerprint()}); ok {
						m.Stance, m.Salience, m.HaveReaction = rx.Stance, rx.Salience, true
					}
					members = append(members, m)
				}
			}
			srv.mu.Unlock()
			if !current || len(members) == 0 {
				return
			}

			dctx, dcancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer dcancel()
			turns, tokens, err := srv.relay.Dialogue(dctx, members, relationship, story)
			if err != nil {
				log.Printf("chat dialogue failed: %v", err)
				return
			}
			scripted := make([]sim.ScriptedTurn, len(turns))
			for i, t := range turns {
				scripted[i] = sim.ScriptedTurn{Speaker: t.Speaker, Text: t.Text, ReplyTo: t.ReplyTo}
			}

			srv.mu.Lock()
			applied := srv.story.Fingerprint() == story.Fingerprint() && srv.s.InstallChatScript(scripted)
			srv.mu.Unlock()
			log.Printf("chat dialogue: %d turns, %d tokens, applied=%v", len(turns), tokens, applied)
		}()
	}

	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(map[string]any{
		"seeded": len(seeded), "where": where, "snapshot": snap})
}

func (srv *server) stats(rw http.ResponseWriter, r *http.Request) {
	srv.mu.Lock()
	snap := srv.s.Snapshot(r.URL.Query().Get("degree") == "1")
	running, last, where := srv.running, srv.lastEv, srv.lastWhere
	tickMS, dirty, n := srv.s.TickMS, srv.s.DirtyCount(), srv.w.N
	dayNightOn := srv.s.DayNight.MinutesPerTick > 0
	hourUTC := srv.s.HourUTC
	srv.mu.Unlock()

	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(rw).Encode(map[string]any{
		"snapshot": snap, "running": running, "headline": last, "where": where,
		"instance_bytes": world.InstanceBytes,
		"archetypes":     world.NumArchetypes(),
		"tick_ms":        tickMS,
		"dirty":          dirty,
		"uptime_s":       time.Since(startedAt).Seconds(),
		"recovered":      recovered.Load(),
		"daynight":       dayNightOn,
		"hour_utc":       hourUTC,
		"wire": map[string]any{
			"delta_bytes": 12 + 6*dirty,
			"full_bytes":  12 + 2*n,
			"form":        map[bool]string{true: "full", false: "delta"}[6*dirty >= 2*n],
		},
	})
}

func (srv *server) control(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		Running *bool    `json:"running"`
		Reset   bool     `json:"reset"`
		Speed   *float64 `json:"speed"`
		Step    int      `json:"step"`
		Media   *bool    `json:"media"`
	}
	json.NewDecoder(http.MaxBytesReader(rw, r.Body, 4096)).Decode(&req)

	srv.mu.Lock()
	if req.Running != nil {
		srv.running = *req.Running
	}
	if req.Speed != nil {
		srv.speed = math.Min(math.Max(*req.Speed, minSpeed), maxSpeed)
	}
	if req.Step > 0 {
		// Bounded: an unbounded step count from a client is a way to ask the
		// server to spend an hour inside the tick lock and stop answering.
		srv.steps += min(req.Step, 100)
	}
	if req.Media != nil {
		// Switching the media layer mid-run is deliberate and is the fastest
		// honest A/B available in the UI: same world, same graph, same story,
		// platforms on or off from this tick forward.
		srv.s.M.SetEnabled(*req.Media)
	}
	if req.Reset {
		// Same graph, fresh cascade and opinions. Rebuilding the network would
		// cost seconds and change the thing under study.
		srv.s.Reset(srv.cfg)
		srv.lastEv = ""
		srv.steps = 0
	}
	running, speed, steps := srv.running, srv.speed, srv.steps
	media := srv.s.M.Enabled()
	srv.mu.Unlock()

	writeJSON(rw, map[string]any{
		"running": running, "speed": speed, "pending_steps": steps, "media": media,
	})
}

// mediaStatus reports what each platform is doing right now.
func (srv *server) mediaStatus(rw http.ResponseWriter, r *http.Request) {
	srv.mu.Lock()
	on := srv.s.M.Enabled()
	var stats []sim.PlatformStat
	if srv.s.M != nil {
		stats = srv.s.M.Stats(srv.w)
	}
	srv.mu.Unlock()
	writeJSON(rw, map[string]any{"enabled": on, "platforms": stats})
}

// chat serves the six-person cohort: who they are and what they have said.
func (srv *server) chat(rw http.ResponseWriter, r *http.Request) {
	srv.mu.Lock()
	c := srv.s.Chat
	if c == nil {
		srv.mu.Unlock()
		writeJSON(rw, map[string]any{"available": false})
		return
	}
	c.Refresh(srv.w, srv.s)
	// Copy under the lock: these slices are mutated by the tick loop, and
	// encoding them directly would race the writer for the whole of the JSON
	// serialisation.
	// Non-nil slices: an empty conversation must serialise as [] rather than
	// null, or every client has to special-case "no messages yet".
	friends := append(make([]sim.Friend, 0, len(c.Friends)), c.Friends...)
	msgs := append(make([]sim.Message, 0, len(c.Messages)), c.Messages...)
	origin, platform, sketch := c.Origin, c.Platform, c.Sketch()
	kind, label := c.Kind.String(), c.Kind.Label()
	srv.mu.Unlock()

	writeJSON(rw, map[string]any{
		"available": true, "origin": origin, "platform": platform, "sketch": sketch,
		"kind": kind, "kind_label": label,
		"friends": friends, "messages": msgs,
	})
}

// cohort rerolls the chat to a fresh draw, optionally of a different kind.
// Distinct from /api/control{reset:true}: an ordinary reset keeps the same six
// people and clears only the conversation, because a researcher comparing two
// stories usually wants the same group for both. This endpoint is the other
// operation -- "I want a different chat" -- and it is not something reset
// should also quietly do.
func (srv *server) cohort(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Kind string `json:"kind"`
	}
	json.NewDecoder(http.MaxBytesReader(rw, r.Body, 1024)).Decode(&req)
	kind, ok := sim.ParseCohortKind(req.Kind)
	if !ok {
		http.Error(rw, fmt.Sprintf("unknown cohort kind %q", req.Kind), http.StatusBadRequest)
		return
	}

	srv.mu.Lock()
	formed := srv.s.RerollChat(kind, uint64(time.Now().UnixNano()))
	srv.mu.Unlock()

	if !formed {
		// Kind-specific enough to actually help: KindOnline is the one that
		// needs something beyond population, and it is worth naming rather
		// than leaving the reader to guess which constraint failed.
		reason := "this world does not have enough people matching that pattern"
		if kind == sim.KindOnline && srv.s.M == nil {
			reason = "online friends needs the media layer, which is off in this run"
		}
		http.Error(rw, "could not form that chat: "+reason, http.StatusUnprocessableEntity)
		return
	}
	writeJSON(rw, map[string]any{"ok": true, "kind": kind.String()})
}

// streetRosterSize bounds how many named residents a street-level view shows
// at once. Large enough to see real local structure, small enough that
// uniqueName's ~10-names-per-region lists don't collide constantly and that
// drawing every tie stays legible rather than becoming a hairball.
const streetRosterSize = 48

// street returns a bounded, named roster of real people at one place -- the
// zoom rung between the population-scale globe and a single persona's
// inspector card. See internal/sim/street.go for the mechanism.
//
// The roster is cached server-side and only rebuilt on a place change or an
// explicit reroll (?reroll=1); every other poll just re-reads live state for
// the same fixed ids via Street.Refresh, which is why this doesn't cost
// anywhere near what /api/persona's linear scan does -- see main.go's own
// comment on that handler for why that scan is fine only for an occasional
// click and would not be for something polled repeatedly.
func (srv *server) streetView(rw http.ResponseWriter, r *http.Request) {
	pid, err := strconv.Atoi(r.URL.Query().Get("place"))
	if err != nil || pid < 0 || pid >= world.NumPlaces() {
		http.Error(rw, "place is required, a valid place id", http.StatusBadRequest)
		return
	}
	reroll := r.URL.Query().Get("reroll") == "1"

	srv.mu.Lock()
	if srv.street == nil || srv.street.PlaceID != uint16(pid) || reroll {
		srv.streetGen++
		st := sim.NewStreet(srv.w, srv.s.G, srv.placeIdx, uint16(pid), srv.streetGen, streetRosterSize)
		if st == nil {
			srv.mu.Unlock()
			http.Error(rw, "nobody lives there in this population", http.StatusUnprocessableEntity)
			return
		}
		srv.street = st
	}
	srv.street.Refresh(srv.s)
	// Copy under the lock, same reasoning as /api/chat: Residents is mutated
	// in place by Refresh, and JSON-encoding it directly would race the tick
	// loop for the whole of the serialisation.
	residents := append(make([]sim.StreetResident, 0, len(srv.street.Residents)), srv.street.Residents...)
	out := map[string]any{
		"place_id": srv.street.PlaceID, "place": srv.street.PlaceName, "region": srv.street.Region,
		"lat": srv.street.Lat, "lon": srv.street.Lon,
		"residents": residents,
	}
	srv.mu.Unlock()

	writeJSON(rw, out)
}

func (srv *server) breakdown(rw http.ResponseWriter, r *http.Request) {
	srv.mu.Lock()
	b := srv.s.Breakdown()
	srv.mu.Unlock()
	writeJSON(rw, b)
}

func (srv *server) history(rw http.ResponseWriter, r *http.Request) {
	srv.mu.Lock()
	h := srv.s.History()
	srv.mu.Unlock()
	writeJSON(rw, map[string]any{"samples": h})
}

func (srv *server) relayStatus(rw http.ResponseWriter, r *http.Request) {
	if srv.relay == nil {
		writeJSON(rw, map[string]any{"enabled": false, "gateway": ""})
		return
	}
	writeJSON(rw, map[string]any{
		"enabled": true, "gateway": srv.gatewayURL,
		"progress": srv.relay.Progress(), "stories": srv.relay.Stories(),
	})
}

// reactions returns the generated lines for the current story, most engaged
// first. This is the output a user actually reads, so it is ranked by how much
// the archetype cares rather than by archetype id.
func (srv *server) reactions(rw http.ResponseWriter, r *http.Request) {
	if srv.relay == nil {
		writeJSON(rw, map[string]any{"reactions": []any{}})
		return
	}
	srv.mu.Lock()
	story := srv.story
	counts := make([]int, world.NumArchetypes())
	for i := 0; i < srv.w.N; i++ {
		if a := int(srv.w.Arch[i]); a < len(counts) {
			counts[a]++
		}
	}
	srv.mu.Unlock()

	snap := srv.relay.Snapshot(story)
	type row struct {
		Role     string  `json:"role"`
		Region   string  `json:"region"`
		Stratum  string  `json:"stratum"`
		Stance   float32 `json:"stance"`
		Salience float32 `json:"salience"`
		Share    float32 `json:"share"`
		Line     string  `json:"line"`
		Personas int     `json:"personas"`
	}
	out := make([]row, 0, len(snap))
	for id, rx := range snap {
		a := world.Archetype(id)
		n := 0
		if id < len(counts) {
			n = counts[id]
		}
		out = append(out, row{
			Role: a.Role().Name, Region: world.Regions[a.Region()].Name,
			Stratum: world.Strata[a.Role().Stratum].Name,
			Stance:  rx.Stance, Salience: rx.Salience, Share: rx.Share,
			Line: rx.Line, Personas: n,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Salience > out[j].Salience })
	writeJSON(rw, map[string]any{"story": story.Headline, "n": len(out), "reactions": out})
}

// generate walks the archetype manifest through the gateway and folds the
// result back into the population.
//
// Runs in the background and returns immediately: a full pass is minutes, and
// an HTTP handler that blocks for minutes is a handler that times out behind
// every proxy between here and the browser. Progress is polled from /api/relay.
func (srv *server) generate(rw http.ResponseWriter, r *http.Request) {
	if srv.relay == nil {
		http.Error(rw, "no model gateway configured (-gateway)", http.StatusPreconditionFailed)
		return
	}
	var req struct {
		Headline string  `json:"headline"`
		Stance   float64 `json:"stance"`
		Detail   string  `json:"detail"`
		Limit    int     `json:"limit"` // 0 = every archetype carrying population
	}
	json.NewDecoder(http.MaxBytesReader(rw, r.Body, 1<<16)).Decode(&req)
	if req.Headline == "" {
		srv.mu.Lock()
		req.Headline = srv.lastEv
		srv.mu.Unlock()
	}
	if req.Headline == "" {
		http.Error(rw, "no story to react to; break one first", http.StatusBadRequest)
		return
	}

	// Only archetypes that carry population. The empty cells are real cells --
	// they just have nobody in them at this sample size, and generating for
	// them would spend the budget on people who do not exist.
	srv.mu.Lock()
	counts := make([]int, world.NumArchetypes())
	for i := 0; i < srv.w.N; i++ {
		if a := int(srv.w.Arch[i]); a < len(counts) {
			counts[a]++
		}
	}
	srv.mu.Unlock()

	ids := make([]int, 0, len(counts))
	for id, n := range counts {
		if n > 0 {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return counts[ids[i]] > counts[ids[j]] })
	if req.Limit > 0 && req.Limit < len(ids) {
		ids = ids[:req.Limit] // largest archetypes first, so a partial run covers most people
	}

	story := reaction.Story{Headline: req.Headline, Stance: req.Stance, Detail: req.Detail}
	srv.mu.Lock()
	srv.story = story
	srv.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()

		// Fold results in as they arrive rather than at the end, so the globe
		// starts moving within seconds of a run beginning instead of after
		// every archetype has been waited for.
		var pending = map[int]sim.ArchetypeReaction{}
		var pmu sync.Mutex
		flush := func() {
			pmu.Lock()
			if len(pending) == 0 {
				pmu.Unlock()
				return
			}
			batch := pending
			pending = map[int]sim.ArchetypeReaction{}
			pmu.Unlock()

			srv.mu.Lock()
			srv.s.ApplyReactions(batch, srv.applyW)
			srv.mu.Unlock()
		}

		done := make(chan struct{})
		go func() {
			t := time.NewTicker(2 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					flush()
				case <-done:
					return
				}
			}
		}()

		err := srv.relay.Run(ctx, story, ids, srv.relayConc, func(rx reaction.Reaction) {
			pmu.Lock()
			pending[rx.Archetype] = sim.ArchetypeReaction{
				Stance: rx.Stance, Salience: rx.Salience, Share: rx.Share,
			}
			pmu.Unlock()
		})
		close(done)
		flush()

		p := srv.relay.Progress()
		log.Printf("generation finished: %d/%d done, %d from cache, %d failed, "+
			"%d tokens in %.0fs (%.1f tok/s) err=%v",
			p.Done, p.Total, p.FromCache, p.Failed, p.Tokens, p.ElapsedS, p.TokPerSec, err)
	}()

	writeJSON(rw, map[string]any{"started": true, "archetypes": len(ids)})
}

// persona answers "who is that dot". The globe sends a point on the sphere and
// gets back the nearest person to it, with everything the simulation knows.
//
// Nearest by great-circle distance over a linear scan, which is 10M distance
// calculations and ~15 ms. A spatial index would make it microseconds and would
// also be a second copy of the spatial ordering the graph already maintains --
// not worth the divergence risk for a click a human makes once a second.
func (srv *server) persona(rw http.ResponseWriter, r *http.Request) {
	lon, err1 := strconv.ParseFloat(r.URL.Query().Get("lon"), 64)
	lat, err2 := strconv.ParseFloat(r.URL.Query().Get("lat"), 64)
	if err1 != nil || err2 != nil {
		http.Error(rw, "lon and lat are required, in radians", http.StatusBadRequest)
		return
	}
	tx, ty, tz := unit(lon, lat)

	srv.mu.Lock()
	best, bestDot := -1, -2.0
	for i := 0; i < srv.w.N; i++ {
		x, y, z := unit(float64(srv.w.Lon[i]), float64(srv.w.Lat[i]))
		if d := x*tx + y*ty + z*tz; d > bestDot {
			best, bestDot = i, d
		}
	}
	if best < 0 {
		srv.mu.Unlock()
		http.Error(rw, "empty world", http.StatusNotFound)
		return
	}

	a := world.Archetype(srv.w.Arch[best])
	deg := srv.s.G.Degree(best)

	// How many of this person's own neighbours have adopted. This is the number
	// the threshold rule actually reads, so showing it makes an individual's
	// behaviour explicable rather than mysterious.
	adoptedNb := 0
	for _, j := range srv.s.G.Neighbours(best) {
		if srv.s.C.State[j] != sim.Unaware {
			adoptedNb++
		}
	}
	out := map[string]any{
		"id": best, "place": srv.w.PlaceName(best), "place_id": srv.w.Place[best],
		"role": a.Role().Name, "region": world.Regions[a.Region()].Name,
		"stratum":   world.Strata[a.Role().Stratum].Name,
		"archetype": int(a), "describe": a.Describe(),
		"weight":             srv.w.Weight[best],
		"urbanity":           srv.w.Urbanity[best],
		"ties":               deg,
		"opinion":            srv.s.O.Y[best],
		"prior":              srv.s.O.Prior[best],
		"susceptibility":     srv.s.O.Lambda[best],
		"threshold":          srv.s.C.Threshold[best],
		"state":              []string{"has not heard", "carrying it", "moved on"}[srv.s.C.State[best]],
		"neighbours_adopted": adoptedNb,
		"needs":              needsFor(srv.s, best, deg),
	}
	srv.mu.Unlock()

	if srv.relay != nil {
		if rx, ok := srv.relay.Get(reaction.Key{Archetype: int(a), Story: srv.storyFP()}); ok {
			out["line"] = rx.Line
			out["reaction_stance"] = rx.Stance
			out["reaction_salience"] = rx.Salience
		}
	}
	writeJSON(rw, out)
}

// needsFor is how many adopted neighbours this person requires before acting.
func needsFor(s *sim.Sim, i, deg int) int {
	scale := s.C.ThresholdScale
	if scale <= 0 {
		scale = 1
	}
	n := int(float64(s.C.Threshold[i])*scale*float64(deg) + 0.999)
	if n < s.C.MinReinforce {
		n = s.C.MinReinforce
	}
	return n
}

func (srv *server) storyFP() string {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return srv.story.Fingerprint()
}

func unit(lon, lat float64) (x, y, z float64) {
	cl := math.Cos(lat)
	return cl * math.Sin(lon), math.Sin(lat), cl * math.Cos(lon)
}

func (srv *server) experimentStatus(rw http.ResponseWriter, r *http.Request) {
	writeJSON(rw, map[string]any{
		"progress": srv.exp.Progress(), "result": srv.exp.Last(),
	})
}

// experimentRun launches a paired design in the background.
//
// Background because a 12-replicate design at 60k is tens of seconds and a
// 50-replicate one is minutes -- an HTTP handler that blocks that long is one
// that times out behind every proxy between here and the browser.
func (srv *server) experimentRun(rw http.ResponseWriter, r *http.Request) {
	var cfg experiment.Config
	if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 1<<16)).Decode(&cfg); err != nil {
		http.Error(rw, "bad experiment config", http.StatusBadRequest)
		return
	}
	if len(cfg.Arms) == 0 {
		http.Error(rw, "an experiment needs at least one arm", http.StatusBadRequest)
		return
	}
	// Bounded so a stray request cannot pin every core for an hour.
	if cfg.N > 200_000 {
		cfg.N = 200_000
	}
	if cfg.Replicates > 60 {
		cfg.Replicates = 60
	}
	if cfg.BaseSeed == 0 {
		cfg.BaseSeed = uint64(time.Now().UnixNano())
	}

	// Attach cached model output to any arm that asked for it, by label.
	// Resolved here rather than in package experiment so that the experiment
	// runner never needs to know a gateway exists.
	if srv.relay != nil {
		snap := srv.relay.Snapshot(reaction.Story{Headline: srv.lastEv})
		if len(snap) > 0 {
			for i := range cfg.Arms {
				if !strings.Contains(strings.ToLower(cfg.Arms[i].Label), "model") {
					continue
				}
				m := map[int]sim.ArchetypeReaction{}
				for id, rx := range snap {
					m[id] = sim.ArchetypeReaction{
						Stance: rx.Stance, Salience: rx.Salience, Share: rx.Share,
					}
				}
				cfg.Arms[i].Reactions = m
			}
		}
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
		defer cancel()
		res, err := srv.exp.Run(ctx, cfg)
		if err != nil {
			log.Printf("experiment: %v", err)
			return
		}
		log.Printf("experiment done: %d runs in %.1fs", res.Runs, res.ElapsedS)
		for _, c := range res.Comparisons {
			log.Printf("  %q vs %q: %+.1fpp [%.1f, %.1f] significant=%v",
				c.Label, c.Against, c.MeanDiff*100, c.Lo*100, c.Hi*100, c.Significant)
		}
	}()
	writeJSON(rw, map[string]any{"started": true,
		"runs": cfg.Replicates * len(cfg.Arms)})
}

func writeJSON(rw http.ResponseWriter, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(rw).Encode(v)
}

// archetypes is the generation manifest: exactly the list a batch job would
// walk to fill the reaction cache, with live counts so it is obvious which
// cells actually carry population and which are theoretical.
func (srv *server) archetypes(rw http.ResponseWriter, r *http.Request) {
	counts := make([]int, world.NumArchetypes())
	srv.mu.Lock()
	for i := 0; i < srv.w.N; i++ {
		if a := int(srv.w.Arch[i]); a < len(counts) {
			counts[a]++
		}
	}
	srv.mu.Unlock()

	type row struct {
		ID      int     `json:"id"`
		Role    string  `json:"role"`
		Region  string  `json:"region"`
		Stratum string  `json:"stratum"`
		Prompt  string  `json:"prompt"`
		Count   int     `json:"count"`
		Reach   float64 `json:"reach"`
	}
	out := make([]row, 0, len(counts))
	for i := range counts {
		a := world.Archetype(i)
		out = append(out, row{
			ID: i, Role: a.Role().Name, Region: world.Regions[a.Region()].Name,
			Stratum: world.Strata[a.Role().Stratum].Name,
			Prompt:  a.Describe(), Count: counts[i], Reach: a.Reach(),
		})
	}
	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(map[string]any{"n": len(out), "archetypes": out})
}

// seedingOf defaults to a region, because that is both the realistic case and
// the only one that reliably starts a complex contagion; the other two exist
// so the difference can be shown rather than asserted.
func seedingOf(s string) sim.Seeding {
	switch s {
	case "network":
		return sim.SeedInNetwork
	case "scattered":
		return sim.SeedEverywhere
	default:
		return sim.SeedInRegion
	}
}

// derefHour is 0 for "no hour sent" (HasHour will be false alongside it, so
// the zero is never read as a real value) and the requested hour otherwise.
func derefHour(h *float64) float64 {
	if h == nil {
		return 0
	}
	return *h
}

func clamp1(v float64) float64 {
	if v < -1 {
		return -1
	}
	if v > 1 {
		return 1
	}
	return v
}

// envInt reads an integer from the environment, falling back on anything that
// is missing or unparseable. Loudly on unparseable: a typo in a platform
// variable that silently reverts to the default is a deploy that ignores its
// own configuration.
func envInt(k string, d int) int {
	v := os.Getenv(k)
	if v == "" {
		return d
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("WARNING: %s=%q is not a number; using %d", k, v, d)
		return d
	}
	return n
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func commas(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}
