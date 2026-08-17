package sim

import (
	"math"

	"github.com/sakshamio/Populace/internal/world"
)

// Adoption states. Kept to a byte because at ten million nodes this array is
// re-read every tick and the difference between 1 and 8 bytes is the
// difference between fitting in cache and not.
const (
	Unaware uint8 = iota
	Adopted
	Fatigued // adopted and no longer transmitting; see Decay
)

// Contagion spreads a behaviour, belief, or piece of news across the graph.
//
// Two modes, and the choice between them changes the answer by an order of
// magnitude:
//
// SIMPLE contagion is the epidemic model. One exposure can infect you, with
// probability Beta per contact. Reach is limited only by connectivity, so on
// any small-world graph it saturates -- long ties are highways.
//
// COMPLEX contagion (Centola & Macy) requires *reinforcement*: several
// independent sources before you act. Costly, risky, or socially contested
// behaviours work this way -- and so does most of what people actually mean by
// "going viral" in the sense of changing behaviour rather than being seen.
// The consequence is not just slower spread but a qualitatively different
// topology of spread: a long tie carries one exposure, which is never enough,
// so complex contagion cannot cross bridges that simple contagion crosses for
// free. It advances only through locally dense neighbourhoods.
//
// This is why the phase-1 renderer's expanding-circle cascade was labelled a
// placeholder. A single-exposure front does not merely run fast, it reaches
// places a real behavioural cascade never gets to, and reports a number that
// is wrong in the optimistic direction.
type Contagion struct {
	State     []uint8
	Threshold []float32 // fraction of neighbours required to adopt
	AdoptedAt []int32   // round each node adopted, -1 if it never did
	Step      int

	// MinReinforce is the absolute number of distinct adopted neighbours
	// required regardless of degree. Setting it to 1 with Threshold 0 makes
	// this simple contagion; 2 or more is what makes it complex. A pure
	// fractional threshold is not enough on its own -- a degree-1 node would
	// adopt from its single neighbour at any threshold up to 1.0.
	MinReinforce int

	// TransmitFor is how many rounds an adopter keeps transmitting before
	// attention moves on. Zero means forever, which is only ever right for a
	// permanent behaviour change. Attention is finite, and a model without
	// this bound reports that everything eventually reaches everyone -- which
	// is a property of the model, not of the world.
	TransmitFor int

	// ThresholdScale multiplies every threshold for the current story. It is
	// how one contagion differs from another on the same population: sharing a
	// link is nearly free and needs almost no corroboration, changing how you
	// vote is expensive and needs a lot. Without it every story on a given
	// graph either tips or fizzles identically, which says more about the
	// parameters than about the story.
	ThresholdScale float64

	// Simple switches to per-contact probabilistic transmission. Present so
	// the two models can be run on the same graph with the same seeds, which
	// is the only way to make a claim about the difference between them.
	Simple bool
	Beta   float64

	// FeedDriven marks adopters who would not have cleared their threshold on
	// peer confirmations alone. This is the attribution that makes the media
	// layer falsifiable: without it, a run with platforms on and a run with
	// them off differ by a number nobody can decompose.
	FeedDriven []bool

	Seed uint64
	next []uint8

	// baseThreshold is the population's own thresholds, before any story
	// modified them. ApplyReactions lowers Threshold per archetype, and
	// without a pristine copy a second run silently inherits the first run's
	// model output -- which contaminates any comparison between two stories
	// and is invisible in every aggregate.
	baseThreshold []float32
}

// ContagionConfig describes how hard a behaviour is to adopt.
type ContagionConfig struct {
	// MeanThreshold is the population's average adoption threshold as a
	// fraction of a person's neighbours. 0.10-0.25 covers most socially
	// contested behaviours in the empirical literature; higher values model
	// things people only do when nearly everyone around them already does.
	MeanThreshold float64
	SpreadLog     float64
	MinReinforce  int

	// TransmitFor bounds how long an adopter stays contagious, in rounds.
	TransmitFor int
	Seed        uint64
}

func DefaultContagionConfig() ContagionConfig {
	return ContagionConfig{
		MeanThreshold: 0.18,
		SpreadLog:     0.55,
		MinReinforce:  2,
		TransmitFor:   3,
		Seed:          0x5EED,
	}
}

// NewContagion draws per-person thresholds.
//
// Lognormal, not normal: thresholds are bounded below by zero and have a long
// upper tail -- there is no such thing as a negative threshold, but there are
// plenty of people who will not do a thing until almost everyone has. The
// low-threshold tail is the innovator population, and whether a cascade starts
// at all is mostly a question of whether the seed lands near enough of them.
func NewContagion(w *world.World, cfg ContagionConfig) *Contagion {
	c := &Contagion{
		State:          make([]uint8, w.N),
		Threshold:      make([]float32, w.N),
		AdoptedAt:      make([]int32, w.N),
		MinReinforce:   cfg.MinReinforce,
		TransmitFor:    cfg.TransmitFor,
		ThresholdScale: 1,
		Seed:           cfg.Seed,
		next:           make([]uint8, w.N),
	}
	for i := range c.AdoptedAt {
		c.AdoptedAt[i] = -1
	}
	mu := math.Log(cfg.MeanThreshold) - cfg.SpreadLog*cfg.SpreadLog/2
	parallel(w.N, func(lo, hi int) {
		for i := lo; i < hi; i++ {
			r := newRNG(cfg.Seed, uint64(i))
			t := math.Exp(mu + cfg.SpreadLog*r.norm())

			// Media personas relay on much weaker evidence; that is the job.
			if world.Stratum(w.Strat[i]) == world.Media {
				t *= 0.35
			}
			c.Threshold[i] = float32(clampF(t, 0.01, 1.0))
		}
	})
	c.baseThreshold = make([]float32, w.N)
	copy(c.baseThreshold, c.Threshold)
	return c
}

// Advance runs one round over peer ties alone and reports how many newly
// adopted. This is the pre-media model and remains the definition of it.
func (c *Contagion) Advance(g *Graph) int { return c.AdvanceWith(g, nil, nil) }

// AdvanceWith runs one round, optionally counting confirmations the media layer
// delivered and gating evaluation by a day/night activity curve. shown[i] is
// how many apparent adopters person i's feeds put in front of them this
// tick; nil means peer ties only. activity[i] is this tick's [0,1] chance
// person i is actually paying attention (see daynight.go); nil means always
// -- exactly the old behaviour, and every existing caller that passes nil
// here gets it unchanged.
//
// Feed confirmations are added to peer confirmations rather than evaluated
// against a separate rule, and that is the modelling claim: a threshold is
// about how much corroboration a person has seen, not about which channel
// carried it. The claim is contestable -- a stranger on a feed is plainly not a
// neighbour -- which is exactly why the two are counted separately in
// FeedDriven and rendered differently, so the contribution stays visible
// instead of disappearing into a single number.
//
// Synchronous, like the opinion update and for the same reason: a node that
// adopts this round must not transmit until the next one. Updating in place
// would let a cascade travel an unbounded distance in a single tick, at a
// speed set by memory layout.
func (c *Contagion) AdvanceWith(g *Graph, shown []uint8, activity []float64) int {
	scale := c.ThresholdScale
	if scale <= 0 {
		scale = 1
	}
	if c.FeedDriven == nil || len(c.FeedDriven) != len(c.State) {
		c.FeedDriven = make([]bool, len(c.State))
	}
	copy(c.next, c.State)
	counts := make([]int, numWorkers(len(c.State)))

	parallelIdx(len(c.State), func(chunk, lo, hi int) {
		n := 0
		for i := lo; i < hi; i++ {
			if c.State[i] != Unaware {
				continue
			}
			if activity != nil {
				// Asleep: whatever this tick would have shown them waits
				// until they actually check, the same as a feed confirmation
				// they have not opened the app to see yet. A fresh RNG draw
				// per (seed, step, node), so a run replays exactly regardless
				// of how the parallel chunks were scheduled -- same reasoning
				// as the Simple branch below.
				r := newRNG(c.Seed^uint64(c.Step)*0xA57A57A5, uint64(i))
				if r.f64() >= activity[i] {
					continue
				}
			}
			nb := g.Neighbours(i)
			peer := 0
			for _, j := range nb {
				if c.State[j] == Adopted {
					peer++
				}
			}
			feed := 0
			if shown != nil {
				feed = int(shown[i])
			}
			exposed := peer + feed
			if exposed == 0 {
				continue
			}
			// A person with no ties at all was previously unreachable by
			// construction. That is right for a peer channel and wrong for a
			// broadcast one -- being isolated does not keep a feed off your
			// phone -- so the degree-0 early return is gone and the threshold
			// rule below handles it instead.
			if len(nb) == 0 && feed == 0 {
				continue
			}

			if c.Simple {
				// One draw standing in for `exposed` independent contacts:
				// P(at least one succeeds) = 1 − (1−β)^exposed. Exact, and it
				// keeps the RNG stream a function of (seed, step, node) only,
				// so the run replays regardless of how work was scheduled.
				r := newRNG(c.Seed^uint64(c.Step)*0x9E3779B1, uint64(i))
				if r.f64() < 1-math.Pow(1-c.Beta, float64(exposed)) {
					c.next[i] = Adopted
					c.AdoptedAt[i] = int32(c.Step + 1)
					c.FeedDriven[i] = peer == 0
					n++
				}
				continue
			}

			// The denominator stays the peer degree. A feed does not enlarge
			// the circle a person measures consensus against -- it changes how
			// many of that circle appear to have acted. Dividing by degree plus
			// feed volume would make a heavy feed user *harder* to convince the
			// more they were shown, which is backwards.
			need := int(math.Ceil(float64(c.Threshold[i]) * scale * float64(len(nb))))
			if need < c.MinReinforce {
				need = c.MinReinforce
			}
			if exposed >= need {
				c.next[i] = Adopted
				c.AdoptedAt[i] = int32(c.Step + 1)
				// Attribution: would this person have adopted on peer ties
				// alone? If not, the feed is what did it. This is the number
				// the whole media layer has to justify itself with.
				c.FeedDriven[i] = peer < need
				n++
			}
		}
		counts[chunk] = n
	})

	c.State, c.next = c.next, c.State
	c.Step++
	if c.TransmitFor > 0 {
		c.Decay(c.TransmitFor)
	}
	total := 0
	for _, n := range counts {
		total += n
	}
	return total
}

// Run advances until nothing changes or the round budget is spent. A complex
// contagion that has stalled will never restart on its own, so the fixed point
// is genuinely final.
func (c *Contagion) Run(g *Graph, maxRounds int) (rounds, adopted int) {
	for rounds = 0; rounds < maxRounds; rounds++ {
		n := c.Advance(g)
		adopted += n
		if n == 0 {
			rounds++
			break
		}
	}
	return rounds, adopted
}

// Reach is the weighted share of the population that has adopted -- the number
// worth reporting. The unweighted count is a property of the sample, not of
// the world it stands for.
func (c *Contagion) Reach(w *world.World) float64 {
	var num, den float64
	for i := 0; i < w.N; i++ {
		wt := float64(w.Weight[i])
		den += wt
		if c.State[i] != Unaware {
			num += wt
		}
	}
	if den == 0 {
		return 0
	}
	return num / den
}

// FeedShare is the weighted share of *adopters* who cleared their threshold
// only because the feed made up the difference. It is the honest answer to
// "what did the algorithm actually do here", and it is reported as a share of
// adopters rather than of the population because that is the question being
// asked: of the people who acted, how many needed the amplification.
func (c *Contagion) FeedShare(w *world.World) float64 {
	if c.FeedDriven == nil {
		return 0
	}
	var num, den float64
	for i := 0; i < w.N; i++ {
		if c.State[i] == Unaware {
			continue
		}
		wt := float64(w.Weight[i])
		den += wt
		if c.FeedDriven[i] {
			num += wt
		}
	}
	if den == 0 {
		return 0
	}
	return num / den
}

// Count is the raw number of adopters, for tests and for the renderer.
func (c *Contagion) Count() int {
	n := 0
	for _, s := range c.State {
		if s != Unaware {
			n++
		}
	}
	return n
}

func (c *Contagion) Reset() {
	for i := range c.State {
		c.State[i] = Unaware
		c.AdoptedAt[i] = -1
	}
	for i := range c.FeedDriven {
		c.FeedDriven[i] = false
	}
	// Thresholds too: they are population state, not story state, and
	// ApplyReactions has almost certainly moved them.
	if c.baseThreshold != nil {
		copy(c.Threshold, c.baseThreshold)
	}
	c.Step = 0
	c.ThresholdScale = 1
}

// SeedScattered picks k adopters uniformly at random across the whole world.
// This is what a broadcast launch looks like: wide, shallow, and -- for a
// complex contagion -- usually inert, because no one gets a second exposure.
func (c *Contagion) SeedScattered(k int, seed uint64) []int32 {
	n := len(c.State)
	ids := make([]int32, 0, k)
	if n == 0 {
		return ids
	}
	r := newRNG(seed, 0x11)
	// Bounded: rejection sampling against an almost-full population would
	// otherwise spin forever rather than returning a short list.
	for tries := 0; len(ids) < k && tries < 20*k+1000; tries++ {
		i := int32(r.u32() % uint32(n))
		if c.State[i] == Unaware {
			c.State[i] = Adopted
			c.AdoptedAt[i] = int32(c.Step)
			ids = append(ids, i)
		}
	}
	return ids
}

// SeedCluster picks k adopters inside one connected neighbourhood, grown
// breadth-first from a starting node. This is what a targeted launch looks
// like, and the reason it works is that it manufactures the reinforcement a
// complex contagion needs to get started at all.
func (c *Contagion) SeedCluster(g *Graph, start int32, k int) []int32 {
	seen := map[int32]bool{start: true}
	queue := []int32{start}
	ids := make([]int32, 0, k)
	for len(queue) > 0 && len(ids) < k {
		v := queue[0]
		queue = queue[1:]
		if c.State[v] == Unaware {
			c.State[v] = Adopted
			c.AdoptedAt[v] = int32(c.Step)
			ids = append(ids, v)
		}
		for _, u := range g.Neighbours(int(v)) {
			if !seen[u] {
				seen[u] = true
				queue = append(queue, u)
			}
		}
	}
	return ids
}

// SeedRegion seeds a contiguous run of the spatial order: a place, not a
// social neighbourhood.
//
// This is how stories actually start, and for a complex contagion it is also
// the only seeding that reliably works at scale. A social-neighbourhood seed
// grown breadth-first from one person fans out into a tree, and a tree gives
// each new node exactly one adopted neighbour -- never the second exposure the
// threshold rule demands. People who live in the same place are connected to
// *each other*, so the seed set is dense rather than merely connected, and the
// reinforcement is there from the first round.
func (c *Contagion) SeedRegion(g *Graph, centre int32, k int) []int32 {
	if g.N == 0 || len(g.Order) == 0 {
		return nil
	}
	if k > g.N {
		k = g.N
	}
	start := int(g.Rank[centre]) - k/2
	ids := make([]int32, 0, k)
	for off := 0; off < k; off++ {
		p := (start + off) % g.N
		if p < 0 {
			p += g.N
		}
		i := g.Order[p]
		if c.State[i] == Unaware {
			c.State[i] = Adopted
			c.AdoptedAt[i] = int32(c.Step)
			ids = append(ids, i)
		}
	}
	return ids
}

// Decay retires adopters into Fatigued after they have transmitted, so a story
// stops propagating without the population forgetting it happened. Attention
// is finite; a cascade that transmits forever is a bug wearing a model's
// clothes.
func (c *Contagion) Decay(after int) {
	for i := range c.State {
		if c.State[i] == Adopted && c.AdoptedAt[i] >= 0 &&
			int(int32(c.Step)-c.AdoptedAt[i]) >= after {
			c.State[i] = Fatigued
		}
	}
}
