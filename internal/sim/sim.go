package sim

import (
	"encoding/binary"
	"io"
	"time"

	"github.com/sakshamio/Populace/internal/world"
)

// Sim ties the population, the network, and the two dynamics together.
//
// The division of labour that makes this affordable: the language model writes
// content for ~400 archetypes, and everything in this file is cheap
// deterministic arithmetic applied per capita. Nothing here scales with the
// number of model calls, so a tick over ten million people costs the same
// whether the model is fast, slow, or offline.
type Sim struct {
	W *world.World
	G *Graph
	O *Opinion
	C *Contagion
	M *Media

	// Chat is six named people whose messages come out of their own state. It
	// is the readable cross-check on every aggregate above it.
	Chat *Cohort

	// ReactionSource, when set, lets Chat's first-reaction messages use a
	// model-authored line instead of the canned fallback. Left nil by New;
	// the caller wires this in only if it has a model gateway, and package
	// sim never imports the package that would define a concrete one -- see
	// ReactionSource in cohort.go for why.
	ReactionSource ReactionSource

	Tick int

	// dirty tracks which personas changed since the last snapshot.
	//
	// A delta is not automatically cheaper, and measuring it was a surprise: a
	// tick during a live event dirties ~99% of the population, because the
	// opinion field moves for almost everyone at once. Six bytes per record
	// against ten million people is 59 MB, three times worse than resending
	// the whole 20 MB flags array. Encode therefore picks per frame; see there.
	dirty   []int32
	inDirty []bool
	lastFl  []uint16

	// Opinion level at the moment the current story broke. Reported as a shift
	// because the level is mostly the population's stubborn priors, which the
	// story did not cause -- quoting it would credit every event with the
	// entire prior distribution of the topic.
	baseOpinion float64
	baseNeg     float64
	basePos     float64
	base        baseline

	history  []Sample
	histHead int

	// TickMS is an exponentially weighted mean of tick cost. Shown in the UI
	// because a tick budget quietly overrunning the tick interval is the first
	// symptom of a world too big for the box, and it is invisible otherwise.
	TickMS float64
}

type Config struct {
	Graph     GraphConfig
	Contagion ContagionConfig
	TopicSeed uint64

	// Platforms is the media layer. Empty means no platforms at all, which is
	// the pre-media model and the control arm for every claim about them.
	Platforms []Platform
	MediaSeed uint64

	// ChatKind picks which rule chooses the six people in Chat. Zero value is
	// KindChildhood, so a caller that never sets this gets the original chat.
	ChatKind CohortKind
}

func DefaultConfig() Config {
	return Config{
		Graph:     DefaultGraphConfig(),
		Contagion: DefaultContagionConfig(),
		TopicSeed: 0x70B1C5EED,
		Platforms: DefaultPlatforms(),
		MediaSeed: 0xFEED5,
	}
}

func New(w *world.World, cfg Config) *Sim {
	s := &Sim{
		W:       w,
		G:       BuildGraph(w, cfg.Graph),
		O:       NewOpinion(w, cfg.TopicSeed),
		C:       NewContagion(w, cfg.Contagion),
		inDirty: make([]bool, w.N),
		lastFl:  make([]uint16, w.N),
	}
	if len(cfg.Platforms) > 0 {
		s.M = NewMedia(w, cfg.Platforms, cfg.MediaSeed)
	}
	s.Chat = NewCohort(w, s.G, s.M, cfg.TopicSeed^0xC0FFEE, cfg.ChatKind)
	s.settle()
	s.packAll()
	return s
}

// settle runs the opinion field to its fixed point before anything happens.
//
// Without this the population starts away from equilibrium and simply relaxing
// toward it moves every aggregate, in the same direction, by more than any
// story does. Measured before this was added: three events with different
// stances and difficulties all reported roughly -8pp against and -20pp for,
// because none of that was the event -- it was the field converging. A
// "before" state has to be a population already at rest on the topic, or the
// shift is not attributable to anything.
func (s *Sim) settle() {
	s.O.Settle(s.G, 300, 1e-5)
}

// Event is a piece of news entering the world.
//
// It does not touch the population directly. It changes what broadcasters are
// saying and hands the story to a seed set; the network decides the rest. That
// indirection is the point -- an event whose reach is assigned rather than
// derived tells you only what you already assumed.
type Event struct {
	Headline string

	// Stance is where the *coverage* sits, not whether the news is good for
	// anyone. Those come apart more often than they look like they would:
	// coverage hostile to a company reads as good news to its customers, and a
	// run of this exact case moved the population +6pp *in favour* off a
	// stance of -0.8. Once model reactions are applied they override this per
	// archetype, which is the right resolution -- the model knows who benefits
	// and a single scalar cannot.
	Stance   float32 // in [-1, 1]
	Salience float64 // share of media personas that cover it, in [0, 1]

	// Seeding decides where the story first takes hold, and it changes the
	// outcome more than anything else on this struct.
	Seeding  Seeding
	SeedSize int
	Seed     uint64

	// Difficulty scales adoption thresholds for this story: below 1 for
	// things people pass on without thinking, above 1 for things that cost
	// them something to act on. 1 leaves the population's own thresholds
	// untouched.
	Difficulty float64
}

// Seeding is where a story starts.
type Seeding uint8

const (
	// SeedInRegion: everyone in one place. The seed set is dense -- these
	// people are tied to each other -- so reinforcement exists immediately.
	// This is the realistic default and the only one that reliably starts a
	// complex contagion at scale.
	SeedInRegion Seeding = iota

	// SeedInNetwork: outward through the social graph from one well-connected
	// ordinary person. Connected but tree-shaped, so most of the seed set gets
	// a single exposure and the cascade usually stalls.
	SeedInNetwork

	// SeedEverywhere: scattered worldwide, which is what a broadcast launch
	// looks like. Maximum coverage, minimum reinforcement, near-zero adoption.
	SeedEverywhere
)

// Inject applies an event and returns the seeded adopters.
func (s *Sim) Inject(ev Event) []int32 {
	// Broadcasters first: the story exists because they carried it.
	var carriers []int32
	r := newRNG(ev.Seed^0xE7E7, 0)
	for i := 0; i < s.W.N; i++ {
		if world.Stratum(s.W.Strat[i]) != world.Media {
			continue
		}
		if r.f64() < ev.Salience {
			carriers = append(carriers, int32(i))
		}
	}
	s.O.Broadcast(carriers, ev.Stance)

	if ev.Difficulty > 0 {
		s.C.ThresholdScale = ev.Difficulty
	}

	size := ev.SeedSize
	if size <= 0 {
		size = 64
	}
	// None of these seed from a broadcaster, which was the first thing tried
	// and produced nothing. Growing a seed set outward from a media node walks
	// its ~300 neighbours, and those neighbours are scattered worldwide and
	// mostly unconnected to each other -- a star, not a cluster. Every one of
	// them gets exactly one exposure, which is precisely what a complex
	// contagion ignores. Coverage is not adoption, and the model reproduces
	// that without being told to.
	var seeded []int32
	switch ev.Seeding {
	case SeedEverywhere:
		seeded = s.C.SeedScattered(size, ev.Seed)
	case SeedInNetwork:
		seeded = s.C.SeedCluster(s.G, s.denseStart(ev.Seed), size)
	default:
		r := newRNG(ev.Seed^0x9111, 1)
		seeded = s.C.SeedRegion(s.G, int32(r.u32()%uint32(s.W.N)), size)
	}
	// Platforms pick the story up at the same moment the broadcasters do.
	if s.M.Enabled() {
		s.M.Ignite(ev.Salience)
	}

	// A fresh script for the chat: the previous one, canned or model-authored,
	// is about whatever story broke last and has nothing to do with this one.
	if s.Chat != nil {
		lean := make([]float64, len(s.Chat.Friends))
		for i, f := range s.Chat.Friends {
			lean[i] = float64(s.O.Prior[f.ID]) * float64(ev.Stance)
		}
		s.Chat.NewScript(ev.Seed, lean)
	}

	s.baseOpinion = s.O.Mean(s.W)
	s.baseNeg, s.basePos = s.O.Polarisation(s.W, 0.35)
	s.captureBaseline()
	s.ResetHistory()
	s.resync()
	return seeded
}

// denseStart picks an ordinary persona sitting in a well-connected local
// neighbourhood: best-of-k by degree among non-broadcasters. Best-of-k rather
// than the global maximum because the global maximum is always the same node,
// and a simulator where every story starts in the same town is not measuring
// anything about the story.
func (s *Sim) denseStart(seed uint64) int32 {
	r := newRNG(seed^0xD3D3, 0)
	best, bestDeg := int32(0), -1
	for k := 0; k < 64; k++ {
		i := int32(r.u32() % uint32(s.W.N))
		if world.Stratum(s.W.Strat[i]) == world.Media {
			continue
		}
		if d := s.G.Degree(int(i)); d > bestDeg {
			best, bestDeg = i, d
		}
	}
	return best
}

// Advance runs one simulation tick: one contagion round and one opinion round.
//
// Deliberately one of each rather than settling the opinion field to its fixed
// point every tick. Settling would make opinion instantaneous relative to
// news, which is backwards: the interesting regime is the one where a story
// spreads faster than people finish arguing about it.
// Order within the tick is deliberate: the media layer reads the state left by
// the previous tick, contagion consumes that exposure, and opinion moves last
// on the result. Running media after contagion would let a story be amplified
// by adoptions it had not caused yet, which reads as the algorithm predicting
// the future.
func (s *Sim) Advance() (newAdopters int, opinionDelta float64) {
	t0 := time.Now()
	var shown []uint8
	if s.M.Enabled() {
		shown = s.M.Advance(s.W, s.C, s.O)
	}
	newAdopters = s.C.AdvanceWith(s.G, shown)
	opinionDelta = s.O.StepWith(s.G, s.M, s.W)
	s.Tick++
	s.Chat.Observe(s, s.ReactionSource)
	s.repack()

	ms := float64(time.Since(t0).Microseconds()) / 1000
	if s.TickMS == 0 {
		s.TickMS = ms
	} else {
		s.TickMS = s.TickMS*0.9 + ms*0.1
	}
	s.record(newAdopters, ms)
	return newAdopters, opinionDelta
}

// Flags packing. The renderer reads these 16 bits per persona and nothing else:
//
//	bits 0-1  adoption state
//	bits 2-7  opinion, quantised to 6 bits over [-1, 1]
//	bit  8    broadcaster
//	bit  9    adopted because the feed made up the difference
//	bit 10    the feed reached this person this tick
//
// Six bits of opinion is 64 levels, which is finer than the eye can read off a
// four-pixel sprite. Spending more of the word on precision nobody can see
// would cost bandwidth on the delta stream, which is the actual constraint.
//
// Bits 9 and 10 exist so algorithmic reach is *visible* rather than merely
// counted. Peer spread and feed spread produce the same adoption state, and
// rendering them identically would hide the one thing the media layer is
// supposed to demonstrate.
func packFlags(state uint8, y float32, media, feedDriven, feedNow bool) uint16 {
	q := uint16((clampF(float64(y), -1, 1) + 1) * 0.5 * 63)
	f := uint16(state&0x3) | q<<2
	if media {
		f |= 1 << 8
	}
	if feedDriven {
		f |= 1 << 9
	}
	if feedNow {
		f |= 1 << 10
	}
	return f
}

// flagsFor is the single place the packing inputs are gathered, so packAll and
// repack cannot drift apart -- they did once, and a flag set in one path and
// not the other is invisible until the delta stream disagrees with a full frame.
func (s *Sim) flagsFor(i int) uint16 {
	feedDriven := s.C.FeedDriven != nil && s.C.FeedDriven[i]
	feedNow := s.M.Enabled() && s.M.Exposed[i]
	return packFlags(s.C.State[i], s.O.Y[i],
		world.Stratum(s.W.Strat[i]) == world.Media, feedDriven, feedNow)
}

func (s *Sim) packAll() {
	parallel(s.W.N, func(lo, hi int) {
		for i := lo; i < hi; i++ {
			f := s.flagsFor(i)
			s.W.Flags[i] = f
			s.lastFl[i] = f
		}
	})
}

// resync rebuilds the dirty list from scratch against the last-sent flags.
// Correct to call at any time and idempotent: the pending set is defined by
// the difference from what the client last saw, not by an incremental log.
func (s *Sim) resync() {
	s.dirty = s.dirty[:0]
	for i := range s.inDirty {
		s.inDirty[i] = false
	}
	s.repack()
}

// repack recomputes flags and records what changed. Serial on purpose: the
// dirty list must be in a stable order for the delta stream to replay
// identically, and a parallel append would order it by whichever goroutine got
// the lock. The scan itself is memory-bound and cheap.
func (s *Sim) repack() {
	for i := 0; i < s.W.N; i++ {
		f := s.flagsFor(i)
		s.W.Flags[i] = f
		if f != s.lastFl[i] && !s.inDirty[i] {
			s.inDirty[i] = true
			s.dirty = append(s.dirty, int32(i))
		}
	}
}

// Frame magics. Both carry the same information; they differ only in which is
// smaller, which is a property of the tick and not of the design.
const (
	MagicDelta = "PDL1" // u32 count, u32 tick, count × (u32 index, u16 flags)
	MagicFull  = "PFL1" // u32 count, u32 tick, count × u16 flags, index implicit
)

// Encode writes the smallest correct representation of the state change since
// the last call, and reports which form it chose.
//
// A delta record costs 6 bytes; a full record costs 2. So a delta is only
// worth sending while fewer than a third of the population has changed, and
// during an active event far more than a third does. Choosing per frame rather
// than committing to either scheme is what keeps the worst case at 20 MB
// instead of 59 MB, and it costs one comparison.
func (s *Sim) Encode(out io.Writer) (records int, full bool, err error) {
	full = 6*len(s.dirty) >= 2*s.W.N

	hdr := make([]byte, 12)
	if full {
		copy(hdr[0:4], MagicFull)
		binary.LittleEndian.PutUint32(hdr[4:8], uint32(s.W.N))
	} else {
		copy(hdr[0:4], MagicDelta)
		binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(s.dirty)))
	}
	binary.LittleEndian.PutUint32(hdr[8:12], uint32(s.Tick))
	if _, err := out.Write(hdr); err != nil {
		return 0, full, err
	}

	buf := make([]byte, 0, 6*4096)
	flush := func(force bool) error {
		if force || len(buf) >= 6*4096 {
			if _, err := out.Write(buf); err != nil {
				return err
			}
			buf = buf[:0]
		}
		return nil
	}

	if full {
		records = s.W.N
		for i := 0; i < s.W.N; i++ {
			buf = binary.LittleEndian.AppendUint16(buf, s.W.Flags[i])
			if err := flush(false); err != nil {
				return 0, full, err
			}
		}
	} else {
		records = len(s.dirty)
		for _, i := range s.dirty {
			buf = binary.LittleEndian.AppendUint32(buf, uint32(i))
			buf = binary.LittleEndian.AppendUint16(buf, s.W.Flags[i])
			if err := flush(false); err != nil {
				return 0, full, err
			}
		}
	}
	if err := flush(true); err != nil {
		return 0, full, err
	}

	// A full frame acknowledges everything, not just what was dirty.
	if full {
		copy(s.lastFl, s.W.Flags)
		for i := range s.inDirty {
			s.inDirty[i] = false
		}
	} else {
		for _, i := range s.dirty {
			s.lastFl[i] = s.W.Flags[i]
			s.inDirty[i] = false
		}
	}
	s.dirty = s.dirty[:0]
	return records, full, nil
}

// DirtyCount reports pending changes without consuming them.
func (s *Sim) DirtyCount() int { return len(s.dirty) }

// Snapshot is the set of numbers a UI or a report should quote. Every share
// here is population-weighted.
type Snapshot struct {
	Tick        int     `json:"tick"`
	N           int     `json:"n"`
	Edges       int64   `json:"edges"`
	Reach       float64 `json:"reach"`
	Adopters    int     `json:"adopters"`
	MeanOpinion float64 `json:"mean_opinion"`

	// Shift is what this story did, as opposed to what the population already
	// thought. It is the number a report should quote.
	OpinionShift float64 `json:"opinion_shift"`
	NegShift     float64 `json:"share_against_shift"`
	PosShift     float64 `json:"share_for_shift"`

	Variance float64     `json:"opinion_variance"`
	NegShare float64     `json:"share_against"`
	PosShare float64     `json:"share_for"`
	Degree   DegreeStats `json:"degree"`

	// FeedShare is the share of adopters who needed the feed to get there, and
	// FeedReached how many the feed touched this tick. Together they are the
	// media layer's claim about itself, stated in a form that can be false.
	MediaOn     bool    `json:"media_on"`
	FeedShare   float64 `json:"feed_share"`
	FeedReached int     `json:"feed_reached"`
}

func (s *Sim) Snapshot(withDegree bool) Snapshot {
	neg, pos := s.O.Polarisation(s.W, 0.35)
	sn := Snapshot{
		Tick:        s.Tick,
		N:           s.W.N,
		Edges:       s.G.Edges,
		Reach:       s.C.Reach(s.W),
		Adopters:    s.C.Count(),
		MeanOpinion: s.O.Mean(s.W),
		Variance:    s.O.Variance(s.W),
		NegShare:    neg,
		PosShare:    pos,

		OpinionShift: s.O.Mean(s.W) - s.baseOpinion,
		NegShift:     neg - s.baseNeg,
		PosShift:     pos - s.basePos,

		MediaOn:     s.M.Enabled(),
		FeedShare:   s.C.FeedShare(s.W),
		FeedReached: s.M.ExposedCount(),
	}
	if withDegree {
		sn.Degree = s.G.DegreeStats(s.W)
	}
	return sn
}

// Reset returns the world to its pre-event state without rebuilding the graph,
// which is the expensive part. Same graph, same thresholds, fresh cascade --
// the right shape for comparing two events or two seeding strategies.
func (s *Sim) Reset(cfg Config) {
	s.C.Reset()
	s.M.Reset()
	s.Chat.Reset()
	s.O = NewOpinion(s.W, cfg.TopicSeed)
	s.settle()
	s.Tick = 0
	s.baseOpinion, s.baseNeg, s.basePos = 0, 0, 0
	s.base = baseline{}
	s.ResetHistory()
	s.resync()
}

// RerollChat rebuilds Chat with a fresh draw of the given kind, and reports
// whether the world could actually produce one -- KindOnline needs the media
// layer, and every kind needs enough population matching its rule, neither of
// which is guaranteed at small world sizes.
//
// The graph and population are untouched, and the current six are not carried
// forward on purpose: a reroll is a request for a *different* chat, not a
// nudge to the one already running. Same reasoning as why Reset keeps Chat's
// people but clears its messages -- these are the two operations a user
// actually wants, and neither is "keep everything, sort of".
func (s *Sim) RerollChat(kind CohortKind, seed uint64) bool {
	c := NewCohort(s.W, s.G, s.M, seed, kind)
	if c == nil {
		return false
	}
	s.Chat = c
	return true
}

// InstallChatScript hands the chat a model-authored exchange, reported back to
// the caller so it can log or discard a stale result instead of assuming it
// landed. See Cohort.InstallModelScript for why it can refuse: the
// conversation has to already be un-started for this to apply.
func (s *Sim) InstallChatScript(turns []ScriptedTurn) bool {
	if s.Chat == nil {
		return false
	}
	return s.Chat.InstallModelScript(turns)
}
