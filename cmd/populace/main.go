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
	mu        sync.Mutex
	w         *world.World
	s         *sim.Sim
	cfg       sim.Config
	running   bool
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
}

// startedAt is process start, reported so an operator can tell a server that
// has been up for a week from one that restarted a minute ago -- which are
// indistinguishable from any other number on the dashboard.
var startedAt = time.Now()

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
		exp: experiment.NewRunner()}
	if *gwURL != "" {
		srv.relay = reaction.New(llm.New(*gwURL, *gwTok), *model, *rxCache)
		log.Printf("model gateway %s (model %q, cache %s)", *gwURL, *model, *rxCache)
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
	})
	// Where it broke, taken from the seed set rather than chosen separately --
	// two sources for the same fact is two chances to disagree about it.
	where := "nowhere"
	if len(seeded) > 0 {
		where = srv.w.PlaceName(int(seeded[0]))
	}
	srv.lastEv = req.Headline
	srv.lastWhere = where
	snap := srv.s.Snapshot(false)
	srv.mu.Unlock()

	log.Printf("event %q stance %+.2f salience %.2f: seeded %d in %s (%s)",
		req.Headline, req.Stance, req.Salience, len(seeded), where, req.Seeding)

	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(map[string]any{
		"seeded": len(seeded), "where": where, "snapshot": snap})
}

func (srv *server) stats(rw http.ResponseWriter, r *http.Request) {
	srv.mu.Lock()
	snap := srv.s.Snapshot(r.URL.Query().Get("degree") == "1")
	running, last, where := srv.running, srv.lastEv, srv.lastWhere
	tickMS, dirty, n := srv.s.TickMS, srv.s.DirtyCount(), srv.w.N
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
		"wire": map[string]any{
			"delta_bytes": 12 + 6*dirty,
			"full_bytes":  12 + 2*n,
			"form":        map[bool]string{true: "full", false: "delta"}[6*dirty >= 2*n],
		},
	})
}

func (srv *server) control(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		Running *bool `json:"running"`
		Reset   bool  `json:"reset"`
	}
	json.NewDecoder(http.MaxBytesReader(rw, r.Body, 4096)).Decode(&req)

	srv.mu.Lock()
	if req.Running != nil {
		srv.running = *req.Running
	}
	if req.Reset {
		// Same graph, fresh cascade and opinions. Rebuilding the network would
		// cost seconds and change the thing under study.
		srv.s.Reset(srv.cfg)
		srv.lastEv = ""
	}
	running := srv.running
	srv.mu.Unlock()

	rw.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(rw, `{"running":%v}`, running)
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
		"id": best, "place": srv.w.PlaceName(best),
		"role": a.Role().Name, "region": world.Regions[a.Region()].Name,
		"stratum":   world.Strata[a.Role().Stratum].Name,
		"archetype": int(a), "describe": a.Describe(),
		"weight":             srv.w.Weight[best],
		"urbanity":           srv.w.Urbanity[best],
		"ties":               deg,
		"opinion":            srv.s.O.Y[best],
		"prior":              srv.s.O.Prior[best],
		"openness":           srv.s.O.Lambda[best],
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
