package sim

import (
	"fmt"
	"sort"

	"github.com/sakshamio/Populace/internal/world"
)

// A cohort is six people who grew up in the same place and still share a group
// chat. It is the bottom rung of the view ladder: world, region, network, chat,
// person.
//
// The point is not decoration. Every aggregate above this -- reach, opinion
// shift, feed share -- is a number about a population, and a number about a
// population is exactly the kind of thing that can look reasonable while the
// mechanism underneath it is wrong. Six named people whose messages are
// generated *from their own simulation state* are a continuous audit: if the
// cascade says 40% reach and nobody in the chat has heard about it, one of the
// two is lying, and the chat is the one you can check by reading.
//
// The chat is also a literal window into the closed channel. Group chat is a
// platform in the media layer with no ranking function, and this is what that
// platform looks like from the inside: no algorithm, no amplification, just six
// people who each bring news in from their own separate networks.
type Cohort struct {
	Kind CohortKind

	Origin   string // the place they grew up, or "" when the kind has none
	OriginID uint16 // place index, for seeding stories where they live

	// Platform is set only for KindOnline, where there is no shared place --
	// the platform is the entire reason these six know each other.
	Platform string

	Friends    []Friend `json:"friends"`
	Messages   []Message
	maxHistory int
	nextMsgID  int

	// Last tick's values for these six only; see Observe.
	prevState []uint8
	prevY     []float32

	// script is a pre-written exchange -- a real back-and-forth rather than six
	// independent reactions to the news -- released one turn at a time as its
	// speaker's own contagion state says they have actually heard the story.
	// See buildCannedScript and releaseScript for the mechanism; ScriptedTurn
	// for the shape.
	script           []ScriptedTurn
	scriptCursor     int   // index of the next turn waiting to be released
	scriptStallSince int   // tick the cursor got stuck on an unheard speaker, -1 if not stuck
	scriptMsgID      []int // Message.ID each released turn landed at, -1 if not yet released
	scriptIsModel    bool  // for the UI tag: canned wit vs the model's actual words
}

// CohortKind is which rule picked these six people. The rule is the
// experiment: each kind holds a different variable fixed (place, job,
// platform) so a difference in how the story moves through the chat can be
// attributed to something specific rather than to "six different people".
type CohortKind uint8

const (
	// KindChildhood: four still live in one place, two moved to a different
	// region. The default -- see newChildhoodCohort for why the split matters.
	KindChildhood CohortKind = iota

	// KindCoworkers: six people doing the same job in the same place. Nothing
	// separates their media diet or their local ties, so a story that splits
	// this group is telling you something about the story, not about them.
	KindCoworkers

	// KindOnline: six people who share a platform and nothing else -- drawn
	// from six different regions on purpose. There is no local tie between
	// them at all; if news reaches all six anywhere near together, the
	// platform is the entire explanation.
	KindOnline

	// KindNeighbours: six people who all still live in one place, nobody
	// moved. The control group for KindChildhood: same construction, minus
	// the geographic split, so any difference between the two chats is the
	// two movers and nothing else.
	KindNeighbours

	NumCohortKinds
)

// String is the wire form used by the API and the UI's <select>.
func (k CohortKind) String() string {
	switch k {
	case KindCoworkers:
		return "coworkers"
	case KindOnline:
		return "online"
	case KindNeighbours:
		return "neighbours"
	default:
		return "childhood"
	}
}

// Label is what a person reads in the UI.
func (k CohortKind) Label() string {
	switch k {
	case KindCoworkers:
		return "Coworkers"
	case KindOnline:
		return "Online friends"
	case KindNeighbours:
		return "The neighbours"
	default:
		return "Childhood friends"
	}
}

// ParseCohortKind maps the API's string back to a CohortKind. "" parses as
// KindChildhood so an omitted field in a request body is not an error.
func ParseCohortKind(s string) (CohortKind, bool) {
	switch s {
	case "coworkers":
		return KindCoworkers, true
	case "online":
		return KindOnline, true
	case "neighbours", "neighbors":
		return KindNeighbours, true
	case "childhood", "":
		return KindChildhood, true
	default:
		return 0, false
	}
}

// Sketch is a one-line, kind-specific explanation of why these six are
// together, mirroring Platform.Sketch: the point is that a reader should be
// able to argue with the mechanism, not just read a label.
func (c *Cohort) Sketch() string {
	switch c.Kind {
	case KindCoworkers:
		return "work the same job in " + c.Origin +
			". Nothing separates their media diet -- a fast, homogeneous consensus is what to expect here."
	case KindOnline:
		return "never shared a hometown -- they know each other from " + c.Platform +
			". Geography does nothing for this group; the platform is the whole channel."
	case KindNeighbours:
		return "all still live in " + c.Origin +
			". No one moved, so there is no geographic split to explain a difference in when they hear things."
	default:
		return "grew up together in " + c.Origin + ". Four stayed, two moved away."
	}
}

// Archetypes lists the distinct archetypes this cohort's six people belong to
// -- rarely six, since a small cohort drawn from a shared place or job often
// shares an archetype outright. Exported so a caller with a model gateway can
// ask for exactly these, rather than the whole population, when a story
// breaks and these six specifically need an answer soon.
func (c *Cohort) Archetypes(w *world.World) []int {
	seen := map[int]bool{}
	var out []int
	for _, f := range c.Friends {
		a := int(world.Archetype(w.Arch[f.ID]))
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}

// Friend is one member, bound to a real persona in the population. Everything
// shown about them is read from the simulation rather than authored: the role,
// the place, the opinion, and whether they have heard about the story are all
// live state.
type Friend struct {
	ID     int32  `json:"id"`
	Name   string `json:"name"`
	Role   string `json:"role"`
	Place  string `json:"place"`
	Region string `json:"region"`
	Away   bool   `json:"away"` // moved away from the origin

	// Platforms they are on, by name. This is what makes the chat legible as a
	// media experiment: the friend on the short-video feed hears about things
	// days before the one who is only on broadcast.
	Platforms []string `json:"platforms"`

	// Live state, refreshed on every read.
	Opinion float64 `json:"opinion"`
	State   string  `json:"state"`
	Degree  int     `json:"degree"`
	Feed    bool    `json:"feed_reached"`
}

// Message is one line in the chat, caused by something that happened in the
// simulation on that tick.
type Message struct {
	// ID is stable across the trim in say(), unlike a slice index. ReplyTo
	// below has to survive that trim to keep pointing at the right message, or
	// old history sliding out of maxHistory would silently reattach replies to
	// whatever happens to be sitting at the same offset afterward.
	ID   int    `json:"id"`
	Tick int    `json:"tick"`
	From string `json:"from"`
	Text string `json:"text"`

	// Kind records *why* this line exists, which is what makes the chat an
	// audit rather than a mood piece. Every message traces to a state
	// transition, and the kind names the transition.
	Kind string `json:"kind"`

	// ReplyTo is another message's ID, or -1. Set only on script turns -- see
	// ScriptedTurn -- because only a script has the reply structure to know
	// this; an independent reaction has nothing to reply to by construction.
	ReplyTo int `json:"reply_to"`

	Opinion float64 `json:"opinion"`
	Via     string  `json:"via,omitempty"` // platform, when the feed delivered it
}

// ScriptedTurn is one line of a pre-written exchange among this cohort's six
// people about the current story -- a real conversation, not six independent
// reactions to the news. Speaker indexes into Cohort.Friends; ReplyTo indexes
// into the script slice itself (not into Messages, and not -- yet -- a stable
// ID: that translation happens in releaseScript, once a turn actually fires).
type ScriptedTurn struct {
	Speaker int
	Text    string
	ReplyTo int // -1 for a turn that opens a new thread rather than answering one
}

// NewCohort picks six people according to kind. See CohortKind for what each
// rule holds fixed and why that makes it a different experiment rather than a
// different coat of paint.
func NewCohort(w *world.World, g *Graph, m *Media, seed uint64, kind CohortKind) *Cohort {
	switch kind {
	case KindCoworkers:
		return newCoworkersCohort(w, g, m, seed)
	case KindOnline:
		return newOnlineCohort(w, g, m, seed)
	case KindNeighbours:
		return newNeighboursCohort(w, g, m, seed)
	default:
		return newChildhoodCohort(w, g, m, seed)
	}
}

// newChildhoodCohort picks six people who grew up in one place.
//
// Four still live there and two have moved away, which is not flavour: the two
// who left sit in different regions with different platform mixes, so they hear
// about things on a different schedule. A chat where everyone shares a media
// environment would show one arrival time and teach nothing about how news
// actually travels between places.
func newChildhoodCohort(w *world.World, g *Graph, m *Media, seed uint64) *Cohort {
	if w.N < 64 {
		return nil
	}
	r := newRNG(seed, 0xC0405)
	origin := pickPopulousPlace(w, &r)

	locals := reservoirK(w, &r, 4, func(i int) bool {
		return w.Place[i] == origin && world.Stratum(w.Strat[i]) != world.Media
	})
	_, originRegion, _, _ := world.PlaceInfo(int(origin))
	movers := sampleDistinctRegions(w, &r, 2, 4000, originRegion, true, func(i int) bool {
		return world.Stratum(w.Strat[i]) != world.Media
	})
	if len(locals) < 4 || len(movers) < 2 {
		return nil
	}

	name, _, _, _ := world.PlaceInfo(int(origin))
	c := &Cohort{Kind: KindChildhood, Origin: name, OriginID: origin, maxHistory: 400}
	taken := map[string]bool{}
	for _, id := range locals {
		addFriend(c, w, g, m, id, false, taken)
	}
	for _, id := range movers {
		addFriend(c, w, g, m, id, true, taken)
	}
	sort.Slice(c.Friends, func(i, j int) bool { return c.Friends[i].ID < c.Friends[j].ID })
	return c
}

// newNeighboursCohort is newChildhoodCohort with the split removed: six people,
// one place, nobody moved. The control group -- see CohortKind.
func newNeighboursCohort(w *world.World, g *Graph, m *Media, seed uint64) *Cohort {
	if w.N < 64 {
		return nil
	}
	r := newRNG(seed, 0xC0405)
	origin := pickPopulousPlace(w, &r)
	ids := reservoirK(w, &r, 6, func(i int) bool {
		return w.Place[i] == origin && world.Stratum(w.Strat[i]) != world.Media
	})
	if len(ids) < 6 {
		return nil
	}
	name, _, _, _ := world.PlaceInfo(int(origin))
	c := &Cohort{Kind: KindNeighbours, Origin: name, OriginID: origin, maxHistory: 400}
	taken := map[string]bool{}
	for _, id := range ids {
		addFriend(c, w, g, m, id, false, taken)
	}
	sort.Slice(c.Friends, func(i, j int) bool { return c.Friends[i].ID < c.Friends[j].ID })
	return c
}

// newCoworkersCohort picks six people sharing one job in one place.
func newCoworkersCohort(w *world.World, g *Graph, m *Media, seed uint64) *Cohort {
	if w.N < 64 {
		return nil
	}
	r := newRNG(seed, 0xC0405)

	// Which (place, role) pair actually has a real team-sized population here,
	// found the same way pickPopulousPlace finds a place: sample rather than
	// scan, so the answer is a real cell in this world and not always the
	// first one a scan would reach.
	type cell struct {
		place uint16
		role  string
	}
	counts := map[cell]int{}
	var best cell
	bestN := -1
	for k := 0; k < 1500; k++ {
		i := int(r.u32()) % w.N
		if world.Stratum(w.Strat[i]) == world.Media {
			continue
		}
		c := cell{w.Place[i], world.Archetype(w.Arch[i]).Role().Name}
		counts[c]++
		if counts[c] > bestN {
			bestN, best = counts[c], c
		}
	}
	if bestN < 6 {
		return nil
	}

	ids := reservoirK(w, &r, 6, func(i int) bool {
		return w.Place[i] == best.place && world.Stratum(w.Strat[i]) != world.Media &&
			world.Archetype(w.Arch[i]).Role().Name == best.role
	})
	if len(ids) < 6 {
		return nil
	}
	pn, _, _, _ := world.PlaceInfo(int(best.place))
	c := &Cohort{Kind: KindCoworkers, Origin: pn, OriginID: best.place, maxHistory: 400}
	taken := map[string]bool{}
	for _, id := range ids {
		addFriend(c, w, g, m, id, false, taken)
	}
	sort.Slice(c.Friends, func(i, j int) bool { return c.Friends[i].ID < c.Friends[j].ID })
	return c
}

// newOnlineCohort picks six people who share a platform and nothing else --
// scattered one-per-region on purpose, so nothing but the platform can explain
// a shared arrival time. Needs the media layer; returns nil without one.
func newOnlineCohort(w *world.World, g *Graph, m *Media, seed uint64) *Cohort {
	if w.N < 64 || m == nil || len(m.Platforms) == 0 {
		return nil
	}
	r := newRNG(seed, 0xC0405)

	// Which platform has a genuinely broad userbase here, found by sampling
	// membership rather than by picking platform 0 or the one with the
	// largest configured Reach -- Reach is what a platform was built for, not
	// necessarily what this particular world drew.
	counts := make([]int, len(m.Platforms))
	for k := 0; k < 1500; k++ {
		mask := m.Member[int(r.u32())%w.N]
		for p := range m.Platforms {
			if mask&(1<<uint(p)) != 0 {
				counts[p]++
			}
		}
	}
	plat := 0
	for p, n := range counts {
		if n > counts[plat] {
			plat = p
		}
	}

	ids := sampleDistinctRegions(w, &r, 6, 8000, 0, false, func(i int) bool {
		return world.Stratum(w.Strat[i]) != world.Media && m.Member[i]&(1<<uint(plat)) != 0
	})
	if len(ids) < 6 {
		return nil
	}
	c := &Cohort{Kind: KindOnline, Platform: m.Platforms[plat].Name, maxHistory: 400}
	taken := map[string]bool{}
	for _, id := range ids {
		// Away has no "origin" to be away from here; it is unused for this
		// kind and left false. The person card shows each one's own place
		// regardless, which is the whole point -- six different places.
		addFriend(c, w, g, m, id, false, taken)
	}
	sort.Slice(c.Friends, func(i, j int) bool { return c.Friends[i].ID < c.Friends[j].ID })
	return c
}

// pickPopulousPlace finds a well-populated place by sampling rather than by
// scanning, because the population is sorted spatially and a scan would
// always land in the same continent.
func pickPopulousPlace(w *world.World, r *rng) uint16 {
	var best uint16
	bestCount := -1
	for try := 0; try < 24; try++ {
		cand := w.Place[int(r.u32())%w.N]
		n := 0
		for k := 0; k < 512; k++ {
			if w.Place[int(r.u32())%w.N] == cand {
				n++
			}
		}
		if n > bestCount {
			bestCount, best = n, cand
		}
	}
	return best
}

// reservoirK draws up to k personas satisfying pred by reservoir sampling, so
// the result is a genuine draw from everyone who matches rather than whoever
// happens to sit at the lowest indices.
//
// Taking the first k looked equivalent to this once and was not. Roles are
// assigned partly from urbanity, and index order is not independent of it, so
// the first four matches in Mumbai came back as three smallholder farmers and
// a pastoral herder -- a real draw from a biased corner of the place rather
// than a representative one.
func reservoirK(w *world.World, r *rng, k int, pred func(i int) bool) []int32 {
	out := make([]int32, 0, k)
	seen := 0
	for i := 0; i < w.N; i++ {
		if !pred(i) {
			continue
		}
		seen++
		if len(out) < k {
			out = append(out, int32(i))
		} else if j := int(r.u32()) % seen; j < k {
			out[j] = int32(i)
		}
	}
	return out
}

// sampleDistinctRegions draws up to k personas satisfying pred via bounded
// random trials, at most one per region. When hasAvoid, avoidRegion is
// additionally excluded from the start -- used by the childhood cohort so its
// movers are not accidentally still in the locals' region.
func sampleDistinctRegions(w *world.World, r *rng, k, tries int,
	avoidRegion world.Region, hasAvoid bool, pred func(i int) bool) []int32 {
	used := map[world.Region]bool{}
	if hasAvoid {
		used[avoidRegion] = true
	}
	out := make([]int32, 0, k)
	for try := 0; try < tries && len(out) < k; try++ {
		i := int(r.u32()) % w.N
		if !pred(i) {
			continue
		}
		_, reg, _, _ := world.PlaceInfo(int(w.Place[i]))
		if used[reg] {
			continue
		}
		used[reg] = true
		out = append(out, int32(i))
	}
	return out
}

// addFriend resolves one persona into a Friend and appends it, drawing a name
// that is unique within this cohort.
//
// Names must be unique inside the chat. Drawing independently per persona
// produced two people called Chen in the same six-person conversation, which
// makes the transcript unreadable at exactly the moment it is supposed to be
// the legible view.
func addFriend(c *Cohort, w *world.World, g *Graph, m *Media, id int32, away bool, taken map[string]bool) {
	a := world.Archetype(w.Arch[id])
	pn, reg, _, _ := world.PlaceInfo(int(w.Place[id]))
	f := Friend{
		ID:     id,
		Name:   uniqueName(reg, uint64(id), taken),
		Role:   a.Role().Name,
		Place:  pn,
		Region: reg.String(),
		Away:   away,
		Degree: g.Degree(int(id)),
	}
	if m != nil {
		for p := range m.Platforms {
			if m.Member[id]&(1<<uint(p)) != 0 {
				f.Platforms = append(f.Platforms, m.Platforms[p].Name)
			}
		}
	}
	c.Friends = append(c.Friends, f)
}

// Refresh reads live state back onto each friend.
func (c *Cohort) Refresh(w *world.World, s *Sim) {
	if c == nil {
		return
	}
	for i := range c.Friends {
		id := c.Friends[i].ID
		c.Friends[i].Opinion = float64(s.O.Y[id])
		c.Friends[i].State = stateName(s.C.State[id])
		c.Friends[i].Feed = s.M.Enabled() && s.M.Exposed[id]
	}
}

func stateName(st uint8) string {
	switch st {
	case Adopted:
		return "talking about it"
	case Fatigued:
		return "moved on"
	default:
		return "hasn't heard"
	}
}

// ReactionSource optionally supplies a model-authored line for an archetype's
// take on the current story, and whether one exists yet.
//
// Defined here rather than importing internal/reaction, for the same reason
// ArchetypeReaction exists in reactions.go: package sim has to work identically
// with no model configured, so it depends on a one-method shape it owns rather
// than on the LLM package. A nil ReactionSource is exactly "no model" -- every
// call site below treats it as the graceful-degradation case, not an error.
type ReactionSource interface {
	Reaction(archetype int) (line string, ok bool)
}

// Observe is called once per tick. It turns state transitions into chat.
//
// Every branch here is a transition, not a mood: somebody just adopted,
// somebody's opinion moved past a threshold, somebody was reached by a feed.
// A message with no transition behind it would make the chat decorative, and a
// decorative chat cannot be used to check the aggregates -- which is the only
// reason it exists.
//
// The previous tick's values are kept here, six of each, rather than by
// snapshotting the population. Copying State and Y every tick to diff six
// people would cost 50 MB of memory traffic per tick at ten million personas,
// to answer a question about 0.00006% of them.
func (c *Cohort) Observe(s *Sim, rs ReactionSource) {
	if c == nil || len(c.Friends) == 0 {
		return
	}
	if c.prevState == nil {
		c.prevState = make([]uint8, len(c.Friends))
		c.prevY = make([]float32, len(c.Friends))
		for i, f := range c.Friends {
			c.prevState[i] = s.C.State[f.ID]
			c.prevY[i] = s.O.Y[f.ID]
		}
		return // nothing to diff against on the first tick
	}

	for i := range c.Friends {
		f := &c.Friends[i]
		id := f.ID
		now, was := s.C.State[id], c.prevState[i]
		y, py := float64(s.O.Y[id]), float64(c.prevY[i])
		c.prevState[i], c.prevY[i] = now, s.O.Y[id]

		switch {
		case was == Unaware && now == Adopted:
			via := ""
			if s.M.Enabled() && s.C.FeedDriven != nil && s.C.FeedDriven[id] {
				via = c.loudestPlatform(s.M, id)
			}
			// The model's line, when there is one, replaces the canned first
			// reaction -- this is the one line per (archetype, story) the
			// model was actually asked to write, in this person's own voice,
			// about this specific headline. The canned line is not a worse
			// version of this; it is what runs when there is no model, or the
			// model has not answered for this archetype yet, which is why the
			// fallback stays instead of a placeholder.
			text, kind := "", "heard"
			if rs != nil {
				if line, ok := rs.Reaction(int(world.Archetype(s.W.Arch[id]))); ok {
					text, kind = line, "heard-live"
				}
			}
			if text == "" {
				text = arrivalLine(f, y, via)
			}
			c.say(s.Tick, f, text, kind, y, via)

		case was == Adopted && now == Fatigued:
			// Only some people announce that they are done with a thing.
			if hash32(uint64(id)^uint64(s.Tick))%3 == 0 {
				c.say(s.Tick, f, tiredLine(f), "moved-on", y, "")
			}

		case now != Unaware && sign(y) != sign(py) && absF(y-py) > 0.04:
			c.say(s.Tick, f, turnedLine(f, y), "changed-mind", y, "")

		case now != Unaware && absF(y-py) > 0.08:
			c.say(s.Tick, f, strengthenLine(f, y, y > py), "dug-in", y, "")
		}
	}

	// The script is a separate, additive channel: a friend's own reaction to
	// the news above is independent of whatever the pre-written exchange has
	// them saying later. The two occasionally sit close together in the
	// transcript -- someone's quick reflexive line, then a moment later a
	// fuller remark once the group has actually started talking -- which is
	// not a bug, it is what texting a group chat is actually like.
	c.releaseScript(s)
}

// loudestPlatform names the platform most likely to have delivered the story to
// this person: the one they are on with the highest presence right now.
func (c *Cohort) loudestPlatform(m *Media, id int32) string {
	best, bestP := "", 0.0
	for p := range m.Platforms {
		if m.Member[id]&(1<<uint(p)) == 0 {
			continue
		}
		if m.Presence[p] > bestP {
			best, bestP = m.Platforms[p].Name, m.Presence[p]
		}
	}
	return best
}

// say appends an independent reaction -- one with no reply structure, because
// nothing produced one. Scripted turns go through sayScript instead.
func (c *Cohort) say(tick int, f *Friend, text, kind string, y float64, via string) int {
	return c.append(Message{
		Tick: tick, From: f.Name, Text: text, Kind: kind,
		ReplyTo: -1, Opinion: y, Via: via,
	})
}

// append assigns a stable ID, stores the message, trims history, and returns
// the ID -- needed by releaseScript to record where a turn landed, so a later
// turn replying to it can resolve the reply after any amount of trimming.
func (c *Cohort) append(m Message) int {
	m.ID = c.nextMsgID
	c.nextMsgID++
	c.Messages = append(c.Messages, m)
	if len(c.Messages) > c.maxHistory {
		c.Messages = c.Messages[len(c.Messages)-c.maxHistory:]
	}
	return m.ID
}

// Reset clears the conversation but keeps the people. They do not stop being
// friends because a new story broke.
func (c *Cohort) Reset() {
	if c != nil {
		c.Messages = c.Messages[:0]
		// Drop the diff baseline too. Keeping it would make the first tick
		// after a reset compare against the old story's state and open the new
		// conversation with six people reacting to news that no longer exists.
		c.prevState, c.prevY = nil, nil
		c.script, c.scriptCursor, c.scriptStallSince, c.scriptMsgID = nil, 0, -1, nil
		c.scriptIsModel = false
		// nextMsgID is NOT reset: IDs only need to stay unique for this
		// Cohort's lifetime, and a reply_to surviving a reset by landing on
		// whatever new message happens to reuse ID 0 is a far worse bug than
		// an ID sequence that does not restart at zero.
	}
}

// NewScript replaces the current script with a freshly written one for the
// story that just broke -- the previous script's text, canned or model-
// authored, is about the old headline and has nothing to do with this one.
// lean[i] is a rough read on whether friend i is inclined to agree with the
// story's own stance (their prior times the stance; same sign means yes),
// used only to flavour reply templates, never the substantive opinion the
// simulation tracks.
func (c *Cohort) NewScript(seed uint64, lean []float64) {
	if c == nil {
		return
	}
	c.script = buildCannedScript(c, seed, lean)
	c.scriptMsgID = make([]int, len(c.script))
	for i := range c.scriptMsgID {
		c.scriptMsgID[i] = -1
	}
	c.scriptCursor = 0
	c.scriptStallSince = -1
	c.scriptIsModel = false
}

// InstallModelScript replaces the canned script with the model's authored
// dialogue, but only if nothing has been released from the current script
// yet. Once the conversation has actually started, swapping its remaining
// turns for a different set written without knowledge of what was already
// said would read as someone rewriting history rather than as the
// conversation getting richer. If the model answers before anyone in the
// cohort has adopted -- which the eager warm-up in cmd/populace is
// specifically timed to make the common case -- this always applies cleanly.
func (c *Cohort) InstallModelScript(turns []ScriptedTurn) bool {
	if c == nil || c.scriptCursor != 0 {
		return false
	}
	c.script = turns
	c.scriptMsgID = make([]int, len(turns))
	for i := range c.scriptMsgID {
		c.scriptMsgID[i] = -1
	}
	c.scriptIsModel = true
	c.scriptStallSince = -1
	return true
}

// scriptStallLimit bounds how long releaseScript waits on one turn before
// giving up on it. Generous relative to how fast a cohort of six usually all
// adopt once the first of them does, so it only ever fires on the genuine
// edge case -- a speaker the cascade never reaches -- rather than on ordinary
// variance in arrival time.
const scriptStallLimit = 120

// releaseScript advances the script by at most one turn per tick, gated on
// the turn's speaker having actually heard the story (their contagion state
// is not Unaware) and, if the turn replies to another, on that earlier turn
// having already landed. Strictly in script order, never by scanning ahead for
// whichever turn happens to be ready first -- a conversation that jumped
// around to whoever adopted soonest would not read as one conversation.
//
// A cursor stuck because its speaker never adopts would otherwise silence
// everything behind it forever; after scriptStallLimit it gives up on that one
// turn and moves to the next, so one unlucky adoption timing does not take the
// whole exchange down with it.
func (c *Cohort) releaseScript(s *Sim) {
	if c == nil {
		return
	}
	for c.scriptCursor < len(c.script) {
		t := c.script[c.scriptCursor]
		if t.Speaker < 0 || t.Speaker >= len(c.Friends) {
			// Cannot happen from buildCannedScript; can happen from a model
			// response with a bad index. Skip rather than crash the thread.
			c.scriptCursor++
			c.scriptStallSince = -1
			continue
		}
		speakerID := c.Friends[t.Speaker].ID
		heard := s.C.State[speakerID] != Unaware
		depReady := t.ReplyTo < 0 ||
			(t.ReplyTo < len(c.scriptMsgID) && c.scriptMsgID[t.ReplyTo] >= 0)

		if heard && depReady {
			replyID := -1
			if t.ReplyTo >= 0 && t.ReplyTo < len(c.scriptMsgID) {
				replyID = c.scriptMsgID[t.ReplyTo]
			}
			kind := "script"
			if c.scriptIsModel {
				kind = "script-model"
			}
			id := c.append(Message{
				Tick: s.Tick, From: c.Friends[t.Speaker].Name, Text: t.Text,
				Kind: kind, ReplyTo: replyID, Opinion: float64(s.O.Y[speakerID]),
			})
			c.scriptMsgID[c.scriptCursor] = id
			c.scriptCursor++
			c.scriptStallSince = -1
			// One release per tick: a chat that unloads eight lines the
			// instant everyone happens to adopt together reads as a dump of
			// text, not as people actually typing.
			return
		}

		if c.scriptStallSince < 0 {
			c.scriptStallSince = s.Tick
		}
		if s.Tick-c.scriptStallSince < scriptStallLimit {
			return
		}
		c.scriptCursor++
		c.scriptStallSince = -1
	}
}

// buildCannedScript writes a deterministic, story-specific exchange: someone
// breaks the news outright, the others reply agreeing or pushing back rather
// than each stating an independent opinion, and it tails off into a tangent --
// the shape of an actual thread rather than six unconnected reactions. This is
// what a cohort with no model configured runs on forever, and what every
// cohort runs on until a model, if configured, answers.
func buildCannedScript(c *Cohort, seed uint64, lean []float64) []ScriptedTurn {
	n := len(c.Friends)
	if n == 0 {
		return nil
	}
	r := newRNG(seed, 0xC5A7)

	// A random speaking order rather than Friends order, which is sorted by
	// persona ID for a stable roster -- a thread that always opened with the
	// same person would read as scripted in the bad sense.
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	for i := n - 1; i > 0; i-- {
		j := int(r.u32()) % (i + 1)
		order[i], order[j] = order[j], order[i]
	}

	script := make([]ScriptedTurn, 0, n+2)
	var openedAt []int // script indices that opened a thread, in the order they did

	first := order[0]
	script = append(script, ScriptedTurn{
		Speaker: first, ReplyTo: -1,
		Text: pick(&c.Friends[first], 14, []string{
			"ok have you all seen this",
			"did anyone catch this",
			"wait, has everyone seen this yet",
		}),
	})
	openedAt = append(openedAt, 0)

	for _, idx := range order[1:] {
		replyTo := openedAt[int(r.u32())%len(openedAt)]
		target := script[replyTo].Speaker
		agree := lean[idx]*lean[target] >= 0
		text := replyLine(&c.Friends[idx], c.Friends[target].Name, agree)
		script = append(script, ScriptedTurn{Speaker: idx, Text: text, ReplyTo: replyTo})
		openedAt = append(openedAt, len(script)-1)
	}

	// One or two people wander off topic, replying to whoever spoke last --
	// this is what closes the thread rather than leaving it hanging.
	for k, tangents := 0, 1+int(r.u32()%2); k < tangents; k++ {
		idx := order[int(r.u32())%n]
		script = append(script, ScriptedTurn{
			Speaker: idx, ReplyTo: len(script) - 1, Text: tiredLine(&c.Friends[idx]),
		})
	}
	return script
}

func replyLine(f *Friend, targetName string, agree bool) string {
	if agree {
		return fmt.Sprintf(pick(f, 12, []string{
			"yeah %s, that's exactly it",
			"%s's right about this one",
			"honestly agree with %s here",
			"same, %s. no notes",
		}), targetName)
	}
	return fmt.Sprintf(pick(f, 13, []string{
		"eh, not sure I'm with you on this one %s",
		"disagree %s, I think it's more complicated",
		"%s I really don't see it that way",
		"hard no from me on this %s",
	}), targetName)
}

// The lines below are deterministic and state-driven. They are intentionally
// plain: this is the fallback that always works, including with no model
// gateway configured, and it is what a friend's "heard" message falls back to
// when a ReactionSource is present but has not answered for their archetype
// yet. See the "heard" case in Observe for where a model-authored line
// replaces one of these instead.

// pick chooses a variant by persona id, so a given friend keeps a consistent
// voice across a run instead of drawing a fresh register every time they speak.
// Six people delivering the same sentence is what the first version did, and it
// read as one narrator with six labels rather than as six people.
func pick(f *Friend, salt uint64, opts []string) string {
	return opts[hash32(uint64(f.ID)*0x9E3779B1^salt)%uint32(len(opts))]
}

func arrivalLine(f *Friend, y float64, via string) string {
	if via != "" {
		switch {
		case y < -0.25:
			return fmt.Sprintf(pick(f, 1, []string{
				"this is all over %s and I hate it",
				"why is %s nothing but this today",
				"%s has completely lost it over this",
			}), via)
		case y > 0.25:
			return fmt.Sprintf(pick(f, 2, []string{
				"ok %s is full of this and honestly? good",
				"finally seeing this on %s",
				"%s is actually right about this one",
			}), via)
		default:
			return fmt.Sprintf(pick(f, 3, []string{
				"seeing this everywhere on %s — is it real",
				"%s keeps pushing this at me. anyone know more",
				"got this off %s, no idea if it's true",
			}), via)
		}
	}
	switch {
	case y < -0.25:
		return pick(f, 4, []string{
			"did you all see this. not great",
			"well this is bleak",
			"saw the news. not thrilled",
		})
	case y > 0.25:
		return pick(f, 5, []string{
			"finally some good news",
			"ok this one I like",
			"about time honestly",
		})
	default:
		return pick(f, 6, []string{
			"just heard about this",
			"anyone else seen this",
			"catching up on this now",
		})
	}
}

func turnedLine(f *Friend, y float64) string {
	if y > 0 {
		return pick(f, 7, []string{
			"ok I've come round on this actually",
			"fine, you were right",
			"rethought this one",
		})
	}
	return pick(f, 8, []string{
		"changed my mind, this is worse than I thought",
		"no, I've gone off this",
		"took it back. not good",
	})
}

func strengthenLine(f *Friend, y float64, up bool) string {
	if up {
		return pick(f, 9, []string{
			"more of you should be paying attention to this",
			"still think this is the right call",
			"genuinely glad about this",
		})
	}
	return pick(f, 10, []string{
		"the more I read the less I like it",
		"this keeps getting worse",
		"really not sitting right with me",
	})
}

func tiredLine(f *Friend) string {
	return pick(f, 11, []string{
		"anyway. what are we doing this weekend",
		"ok I'm done with this topic",
		"changing the subject — anyone free sunday",
		"enough of this. how's your mum",
	})
}

// givenName picks a plausible given name for a region.
//
// Names are drawn per region rather than from one global list because a chat
// where six people who grew up in Lagos are all called Emma reads as a
// simulation of somewhere else. The lists are short and obviously incomplete;
// they exist to avoid a worse default, not to represent anywhere properly.
func givenName(r world.Region, id uint64) string {
	list := givenNames[r]
	if len(list) == 0 {
		list = givenNames[world.Europe]
	}
	return list[hash32(id*0x9E3779B1)%uint32(len(list))]
}

// uniqueName is givenName with collision avoidance inside one cohort. It walks
// the region's list from the drawn position, then falls back across regions, so
// a small name list cannot produce two people with the same name in a chat of
// six. Deterministic: the walk order is fixed by the persona id.
func uniqueName(r world.Region, id uint64, taken map[string]bool) string {
	list := givenNames[r]
	if len(list) == 0 {
		list = givenNames[world.Europe]
	}
	start := int(hash32(id*0x9E3779B1) % uint32(len(list)))
	for k := 0; k < len(list); k++ {
		if n := list[(start+k)%len(list)]; !taken[n] {
			taken[n] = true
			return n
		}
	}
	// Every name in the region is already in use by this cohort, which needs a
	// region with fewer than six names to happen. Fall through the other lists
	// rather than returning a duplicate.
	for reg := world.Region(0); reg < world.NumRegions; reg++ {
		for _, n := range givenNames[reg] {
			if !taken[n] {
				taken[n] = true
				return n
			}
		}
	}
	return "?"
}

var givenNames = map[world.Region][]string{
	world.SubSaharanAfrica: {"Amara", "Kwame", "Chidi", "Thandiwe", "Sipho", "Ngozi", "Kofi", "Zanele", "Obi", "Aisha"},
	world.MENA:             {"Layla", "Omar", "Yasmin", "Karim", "Rania", "Tarek", "Nour", "Hakim", "Salma", "Idris"},
	world.SouthAsia:        {"Priya", "Arjun", "Meera", "Rohan", "Ananya", "Vikram", "Divya", "Imran", "Kavya", "Sanjay"},
	world.EastAsia:         {"Mei", "Haruki", "Jin", "Yuna", "Wei", "Sora", "Ling", "Takeshi", "Ji-woo", "Chen"},
	world.SoutheastAsia:    {"Anh", "Siti", "Rizal", "Mai", "Bayu", "Linh", "Arif", "Dewi", "Thura", "Nadia"},
	world.Europe:           {"Elena", "Mateusz", "Sofia", "Lukas", "Ines", "Niamh", "Andrei", "Freya", "Marco", "Hana"},
	world.Eurasia:          {"Dmitri", "Aigul", "Nadia", "Ruslan", "Zarina", "Timur", "Alina", "Bekzat", "Leyla", "Artem"},
	world.NorthAmerica:     {"Maya", "Dante", "Riley", "Jonah", "Camila", "Ethan", "Nia", "Owen", "Sadie", "Marcus"},
	world.LatinAmerica:     {"Valentina", "Mateo", "Luciana", "Diego", "Camila", "Tomás", "Paula", "Rafael", "Sol", "Nicolás"},
	world.Oceania:          {"Tane", "Ariana", "Jarrah", "Moana", "Kai", "Sione", "Talia", "Lachlan", "Hine", "Ruben"},
}

func hash32(x uint64) uint32 {
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	return uint32(x)
}

func sign(v float64) int {
	if v < 0 {
		return -1
	}
	if v > 0 {
		return 1
	}
	return 0
}

func absF(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
