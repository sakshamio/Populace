package sim

import (
	"math"

	"github.com/sakshamio/Populace/internal/world"
)

// The media layer: platforms as a second channel alongside interpersonal ties.
//
// Everything up to this point spreads a story person to person. That is the
// right model for how behaviour spreads, and it is only half of how news does.
// A platform is not a fast social graph -- it is a structurally different
// channel, and two properties make it different in ways that change outcomes
// rather than just speed:
//
// AMPLIFICATION. A feed ranked by engagement does not show you a sample of what
// your contacts are doing, it shows you a biased draw from the tail. That
// matters specifically because of how complex contagion works. A behaviour that
// needs several independent confirmations cannot cross a long tie, which is why
// a scattered seed goes nowhere: everybody gets exactly one exposure. An
// amplifying feed manufactures the appearance of many, so it can push a
// contagion over a threshold that peer ties alone would never reach. The claim
// worth testing is not "platforms are faster" but "platforms let things spread
// that otherwise structurally could not".
//
// SORTING. A feed that ranks by predicted engagement shows people more of what
// they already agree with. Under Friedkin-Johnsen the social input term is what
// pulls an agent away from their prior; if that input is filtered to resemble
// the agent, the pull weakens and the population's disagreement persists or
// widens instead of relaxing. Polarisation here is an outcome of the channel,
// not a parameter of the population.
//
// Both are cheap: per tick this is a handful of flops per persona and a scalar
// per platform. Nothing scales with the number of model calls.
type Media struct {
	Platforms []Platform

	// Member is a per-persona bitmask of platform indices. One byte caps the
	// design at 8 platforms, which is deliberate -- this array is read every
	// tick over the whole population, and the cache cost of widening it is real
	// while the ninth platform would tell nobody anything new.
	Member []uint8

	// Score is per-platform attention on the current story, in [0, 1]. This is
	// the state the feedback loop lives in: engagement raises it, decay lowers
	// it, and amplification decides how hard it pushes back on the population.
	Score []float64

	// Presence is the share of each platform's feed slots the story currently
	// occupies. It is the number to look at when asking what a platform is
	// doing: Score is how much room the topic has, Presence is how much of that
	// room ranking actually gave it.
	Presence []float64

	// Peak is the high-water mark of Presence for the current story. Presence
	// itself is zero whenever nothing is circulating, which is most of the time
	// -- a cascade burns out in a handful of ticks and the live number goes
	// quiet with it. The peak is what answers "how much of that feed did this
	// story take over", which is the question anyone reading the panel is
	// actually asking, and it survives the burnout.
	Peak []float64

	// PeakAt is the step at which Peak was reached.
	//
	// Recorded on the theory that amplification would show up as peaking
	// earlier. Measured, it does not: in a story that tips, all six platforms
	// peak on the same tick, because engagement is a property of the global
	// cascade and every platform is reading the same curve. What amplification
	// changes is the *level* -- on one measured run the closed channel topped
	// out at 58% of its feed while the 3.4x amplifier reached 75%. Kept because
	// it is still informative for stories that never tip, where platforms come
	// apart in time as well, but it is not the discriminator it was added to be.
	PeakAt []int

	// Exposed marks personas the feed reached this tick, so the renderer can
	// distinguish algorithmic reach from peer reach. They are the same colour
	// under every previous version of this model, and they are not the same
	// thing.
	Exposed []bool

	// FeedMean is the mean opinion each platform is showing, before per-person
	// sorting. Kept per platform rather than globally because a story can be
	// celebrated on one platform and buried on another, and collapsing that to
	// one number is exactly the detail worth having.
	FeedMean []float64

	Seed uint64
	Step int

	// on counts platform memberships per persona, cached because the opinion
	// blend needs it every tick and popcount over a bitmask is not free at ten
	// million.
	on []uint8

	// enabled allows the whole layer to be switched off for a controlled
	// comparison. An experiment arm that turns platforms off must produce
	// exactly the pre-media behaviour, or the comparison measures the
	// implementation rather than the mechanism.
	enabled bool
}

// feedSlots is how many pieces of content a person meaningfully takes in from
// one platform in one tick. Three is a judgement, not a measurement, and it is
// a single global constant rather than a per-platform parameter so that
// differences between platforms come from ranking and userbase rather than from
// an unexamined knob. It sets the ceiling on how fast any feed can deliver
// confirmations, so it is worth knowing where it lives.
const feedSlots = 3.0

// Platform is one channel. The parameters are deliberately mechanism-level
// rather than brand-level: a researcher should be able to argue with "reach
// 0.31, amplification 2.6" in a way they cannot argue with a logo.
type Platform struct {
	Name string
	Slug string

	// Reach is the share of the population holding an account, before
	// archetype skew is applied.
	Reach float64

	// Amplify is how many apparent adopters the feed can show for each unit of
	// platform attention. 0 makes the platform a pure conduit that shows you
	// what is actually there; above 1 it can manufacture the reinforcement a
	// complex contagion needs. This is the single most consequential number in
	// the file and the one most worth sweeping.
	Amplify float64

	// Sorting is how strongly the feed resembles the person reading it, in
	// [0, 1]. 0 is chronological; 1 is a perfect echo chamber that shows an
	// agent nothing but their own position back.
	Sorting float64

	// Velocity scales how quickly attention builds; Decay how quickly it dies.
	// A platform with high velocity and high decay spikes and vanishes, which
	// is a different shape of event from a slow forum burning for weeks.
	Velocity float64
	Decay    float64

	// OpenSkew and ReachSkew bias membership by archetype: who is actually on
	// this platform. Without them every platform draws the same userbase and
	// the differences between platforms collapse to their parameters.
	OpenSkew  float64 // positive favours high-openness archetypes
	ReachSkew float64 // positive favours high-reach roles: media, celebrities

	// Closed marks a channel with no ranking function at all -- group messaging,
	// email. These have no amplification by construction, and it is worth being
	// able to see that a story spreading only through closed channels behaves
	// like the pre-media model.
	Closed bool

	Sketch string
}

// DefaultPlatforms is a spread of channel *shapes* rather than a roster of real
// products. The point of the set is that the extremes are covered: one channel
// that amplifies hard and sorts little, one that sorts hard and amplifies
// little, and one with no ranking at all as the control.
func DefaultPlatforms() []Platform {
	return []Platform{
		{
			Name: "Short video", Slug: "shortvideo",
			Reach: 0.42, Amplify: 3.4, Sorting: 0.30,
			Velocity: 1.35, Decay: 0.26,
			OpenSkew: 0.45, ReachSkew: 0.30,
			Sketch: "Ranked almost purely by watch time. Enormous amplification, " +
				"weak ideological sorting -- it will show anyone anything that holds attention.",
		},
		{
			Name: "Microblog", Slug: "microblog",
			Reach: 0.19, Amplify: 2.6, Sorting: 0.72,
			Velocity: 1.55, Decay: 0.34,
			OpenSkew: 0.25, ReachSkew: 0.75,
			Sketch: "Fast, quotable, and heavily sorted. Where a story becomes " +
				"a position rather than a fact.",
		},
		{
			Name: "Group chat", Slug: "groupchat",
			Reach: 0.61, Amplify: 0, Sorting: 0.55, Closed: true,
			Velocity: 0.55, Decay: 0.08,
			OpenSkew: -0.10, ReachSkew: -0.30,
			Sketch: "No ranking function at all. Slow, sticky, and the closest " +
				"thing here to the peer channel -- which is why it is the control.",
		},
		{
			Name: "Link aggregator", Slug: "aggregator",
			Reach: 0.08, Amplify: 2.1, Sorting: 0.62,
			Velocity: 1.10, Decay: 0.30,
			OpenSkew: 0.55, ReachSkew: 0.10,
			Sketch: "Small, self-selected, and ranked by votes -- a community that " +
				"amplifies within itself and rarely leaks outward.",
		},
		{
			Name: "Video platform", Slug: "video",
			Reach: 0.47, Amplify: 1.9, Sorting: 0.48,
			Velocity: 0.80, Decay: 0.14,
			OpenSkew: 0.10, ReachSkew: 0.40,
			Sketch: "Long-form and recommendation-driven. Slower to build than a " +
				"short-video feed and far slower to let go.",
		},
		{
			Name: "Broadcast", Slug: "broadcast",
			Reach: 0.34, Amplify: 0.6, Sorting: 0.12, Closed: false,
			Velocity: 0.95, Decay: 0.22,
			OpenSkew: -0.35, ReachSkew: 0.90,
			Sketch: "Editor-ranked rather than engagement-ranked: one story for " +
				"everybody, which is what makes its sorting so low.",
		},
	}
}

// NewMedia assigns platform membership across the population.
//
// Membership is drawn from the archetype rather than at random, because who is
// on which platform is most of what makes platforms differ. A world where every
// channel draws the same userbase would show six platforms behaving like one
// platform with six sets of parameters.
func NewMedia(w *world.World, plats []Platform, seed uint64) *Media {
	if len(plats) > 8 {
		panic("media: Member is a uint8 bitmask, so at most 8 platforms")
	}
	m := &Media{
		Platforms: plats,
		Member:    make([]uint8, w.N),
		Score:     make([]float64, len(plats)),
		Presence:  make([]float64, len(plats)),
		Peak:      make([]float64, len(plats)),
		PeakAt:    make([]int, len(plats)),
		Exposed:   make([]bool, w.N),
		FeedMean:  make([]float64, len(plats)),
		on:        make([]uint8, w.N),
		Seed:      seed,
		enabled:   true,
	}

	parallel(w.N, func(lo, hi int) {
		for i := lo; i < hi; i++ {
			a := world.Archetype(w.Arch[i])
			open, _ := a.Values()
			reach := a.Reach()
			// Connectivity tracks urbanity: a platform's userbase is a city
			// population before it is anything else.
			urb := float64(w.Urbanity[i])

			var mask, cnt uint8
			for p := range plats {
				pl := &plats[p]
				r := newRNG(seed^uint64(p)*0x9E3779B97F4A7C15, uint64(i))

				// A logit-style tilt keeps the result a probability for any
				// skew, which a linear multiplier does not: a large negative
				// skew on a high-reach platform would otherwise go negative and
				// silently clamp to "nobody", which reads as a broken platform.
				z := math.Log(clampF(pl.Reach, 1e-4, 0.9999)/(1-clampF(pl.Reach, 1e-4, 0.9999))) +
					pl.OpenSkew*open*1.8 +
					pl.ReachSkew*(reach-0.5)*2.2 +
					(urb-0.5)*0.9
				if r.f64() < 1/(1+math.Exp(-z)) {
					mask |= 1 << uint(p)
					cnt++
				}
			}
			m.Member[i] = mask
			m.on[i] = cnt
		}
	})
	return m
}

// Enabled reports whether the layer is contributing anything.
func (m *Media) Enabled() bool { return m != nil && m.enabled }

// SetEnabled switches the whole layer on or off. Off must be exactly the
// pre-media model, not an approximation of it.
func (m *Media) SetEnabled(v bool) {
	if m == nil {
		return
	}
	m.enabled = v
	if !v {
		for i := range m.Exposed {
			m.Exposed[i] = false
		}
		for p := range m.Score {
			m.Score[p] = 0
			m.Presence[p] = 0
			m.Peak[p] = 0
		}
	}
}

// Ignite gives the story its initial purchase on each platform.
//
// Score is an *attention envelope*, not exposure. It says how much room the
// topic has on the platform right now; how many people actually see it still
// depends on how many are posting about it, which at ignition is only the seed
// set. Keeping those two separate is what stopped the first version of this
// file from turning every story into a global cascade: it computed
// `salience · (0.35 + 0.65 · velocity)`, which for an ordinary story clamped to
// 1.0, and a platform pinned at maximum attention from tick zero sprays
// confirmations at its entire userbase regardless of whether anybody is talking.
// That is not amplification, it is broadcast, and it erased the tipping point
// the rest of the model exists to show.
func (m *Media) Ignite(salience float64) {
	if !m.Enabled() {
		return
	}
	for p := range m.Platforms {
		pl := &m.Platforms[p]
		s := 0.25 * salience * (0.6 + 0.4*pl.Velocity)
		if pl.Closed {
			s *= 0.35 // nothing "breaks" in a group chat; it arrives forwarded
		}
		m.Score[p] = clampF(s, 0, 1)
		m.Presence[p] = 0
		m.Peak[p] = 0
		m.PeakAt[p] = 0
	}
	m.Step = 0
}

// Reset clears story-specific state but keeps membership, which is a property
// of the population rather than of the story.
func (m *Media) Reset() {
	if m == nil {
		return
	}
	for p := range m.Score {
		m.Score[p] = 0
		m.Presence[p] = 0
		m.Peak[p] = 0
		m.PeakAt[p] = 0
		m.FeedMean[p] = 0
	}
	for i := range m.Exposed {
		m.Exposed[i] = false
	}
	m.Step = 0
}

// Advance updates platform attention from what the population is doing, then
// reports, per persona, how many *apparent* adopters their feeds showed them.
//
// The return slice is additive with the peer-exposure count in Contagion: the
// threshold rule does not care where a confirmation came from, which is the
// modelling claim being made. Whether that is right is arguable and worth
// arguing about -- what is not arguable is that it must be visible, which is
// why feed-driven exposure is tracked separately and rendered differently.
//
// The mechanism, per platform:
//
//	engagement e = weighted share of this platform's members transmitting
//	ranked     q = e ^ (1 / (1 + Amplify))          // what ranking does
//	presence     = q · score                        // share of feed slots
//	shown_i      = Poisson(Slots · presence)
//
// The middle line is the whole model of an engagement-ranked feed, and it is
// worth being explicit about why it takes that shape. A chronological feed
// shows a fair sample of what is circulating, so the chance any given slot is
// about this story is just e. Ranking by predicted engagement over-samples the
// tail, and a power below 1 is exactly that: at Amplify = 0 the exponent is 1
// and the feed is fair; at Amplify = 3.4 the exponent is 0.23, so a story
// circulating among 0.1% of users occupies 21% of feed slots.
//
// That 200-fold gap at low engagement is the entire claim. It also has the
// right limits: as e approaches 1 all curves meet, because a feed cannot
// over-represent something everybody is already posting. Amplification helps
// the obscure, not the universal, which is the interesting direction.
//
// score is the attention envelope over the top, growing with engagement and
// decaying on its own. The saturating (1 − score) factor keeps a popular story
// from growing without bound.
func (m *Media) Advance(w *world.World, c *Contagion, o *Opinion) []uint8 {
	shown := make([]uint8, len(c.State))
	if !m.Enabled() {
		return shown
	}

	// 1. Measure engagement and feed composition per platform. One pass over
	//    the population, accumulating into small per-platform arrays.
	type acc struct {
		members, active float64
		opSum, opN      float64
	}
	np := len(m.Platforms)
	workers := numWorkers(w.N)
	part := make([][]acc, workers)
	parallelIdx(w.N, func(chunk, lo, hi int) {
		local := make([]acc, np)
		for i := lo; i < hi; i++ {
			mask := m.Member[i]
			if mask == 0 {
				continue
			}
			active := c.State[i] == Adopted
			y := float64(o.Y[i])
			wt := float64(w.Weight[i])
			for p := 0; p < np; p++ {
				if mask&(1<<uint(p)) == 0 {
					continue
				}
				local[p].members += wt
				if active {
					local[p].active += wt
					// Only people currently transmitting contribute to what the
					// feed shows. A feed reflects what is being posted now, not
					// the accumulated opinion of everyone who ever saw it.
					local[p].opSum += y * wt
					local[p].opN += wt
				}
			}
		}
		part[chunk] = local
	})

	for p := 0; p < np; p++ {
		var a acc
		for _, l := range part {
			if l == nil {
				continue
			}
			a.members += l[p].members
			a.active += l[p].active
			a.opSum += l[p].opSum
			a.opN += l[p].opN
		}
		pl := &m.Platforms[p]

		engagement := 0.0
		if a.members > 0 {
			engagement = a.active / a.members
		}
		s := m.Score[p]
		s += pl.Velocity * engagement * (1 - s)
		s -= pl.Decay * s
		m.Score[p] = clampF(s, 0, 1)

		// Presence: the share of this platform's feed slots the story occupies.
		// Computed once here rather than per persona -- it is a property of the
		// platform, and doing it inside the population loop would be ten million
		// identical pow() calls.
		q := engagement
		if !pl.Closed && pl.Amplify > 0 && engagement > 0 {
			q = math.Pow(engagement, 1/(1+pl.Amplify))
		}
		m.Presence[p] = clampF(q*m.Score[p], 0, 1)
		if m.Presence[p] > m.Peak[p] {
			m.Peak[p] = m.Presence[p]
			m.PeakAt[p] = m.Step
		}

		if a.opN > 0 {
			m.FeedMean[p] = a.opSum / a.opN
		} else {
			// Nothing being posted: the feed drifts back toward neutral rather
			// than freezing on the last thing anyone said.
			m.FeedMean[p] *= 0.85
		}
	}

	// 2. Deliver exposure. Per persona, per platform they are on, the feed
	//    shows some number of apparent adopters.
	parallel(w.N, func(lo, hi int) {
		for i := lo; i < hi; i++ {
			m.Exposed[i] = false
			mask := m.Member[i]
			if mask == 0 || c.State[i] != Unaware {
				continue
			}
			total := 0
			for p := 0; p < np; p++ {
				if mask&(1<<uint(p)) == 0 {
					continue
				}
				pres := m.Presence[p]
				if pres <= 1e-6 {
					continue
				}
				pl := &m.Platforms[p]
				// The RNG stream is a pure function of (seed, step, person,
				// platform), so a run replays identically no matter how the
				// work was scheduled across goroutines.
				r := newRNG(m.Seed^uint64(m.Step)*0x9E3779B1^uint64(p)*0xC2B2AE35, uint64(i))

				// A closed channel has no ranking, so what it shows is what is
				// actually there: at most one confirmation, exactly like an
				// ordinary tie. This is the control that separates "platforms"
				// from "amplification".
				if pl.Closed {
					if r.f64() < pres {
						total++
					}
					continue
				}
				if n := poisson(&r, feedSlots*pres); n > 0 {
					total += n
				}
			}
			if total > 0 {
				shown[i] = uint8(effectiveConfirmations(total))
				m.Exposed[i] = true
			}
		}
	})

	m.Step++
	return shown
}

// FeedPull returns, per persona, the opinion their feeds are showing them and
// how much weight that input should carry against their peer ties.
//
// The sorting term is the echo chamber, and it is written as a blend rather
// than a filter for a reason: a filtered feed is not a feed with less content,
// it is a feed whose *composition* has moved toward the reader. So what a
// person sees is `Sorting·(their own position) + (1−Sorting)·(what the platform
// is actually saying)`. At Sorting = 0 the platform pulls them toward the
// population; at Sorting = 1 it pulls them toward themselves, which is to say
// it does not pull them at all and their prior wins by default.
func (m *Media) FeedPull(w *world.World, o *Opinion, i int) (mean float64, weight float64) {
	if !m.Enabled() {
		return 0, 0
	}
	mask := m.Member[i]
	if mask == 0 {
		return 0, 0
	}
	y := float64(o.Y[i])
	var sum, wsum float64
	for p := range m.Platforms {
		if mask&(1<<uint(p)) == 0 {
			continue
		}
		// Presence, not Score: influence follows how much of the feed the story
		// actually occupies. Weighting by Score alone made a platform maximally
		// influential the moment a story existed, whether or not anyone was
		// posting about it -- which left sorting with almost nothing to bite on.
		pres := m.Presence[p]
		if pres <= 1e-6 {
			continue
		}
		pl := &m.Platforms[p]
		seen := pl.Sorting*y + (1-pl.Sorting)*m.FeedMean[p]
		wt := pres * (0.4 + 0.6*pl.Velocity)
		sum += seen * wt
		wsum += wt
	}
	if wsum == 0 {
		return 0, 0
	}
	return sum / wsum, wsum
}

// effectiveConfirmations converts posts seen into independent confirmations.
//
// The two are not the same, and treating them as the same is what made the
// first version of this layer turn every story into a global cascade. A
// threshold rule counts *independent* sources; content selected for you by one
// ranking function, out of one pool, is heavily correlated. Twenty posts about
// a thing are nowhere near twenty independent people deciding it independently.
//
// Two adjustments, both in the same direction:
//
//   - sublinear: sqrt, so returns diminish as the feed floods. Seeing a story
//     four times is roughly twice the evidence of seeing it once, not four
//     times. This is the correlation correction.
//   - discounted: feedTrust < 1, because a stranger's post is weaker evidence
//     than a friend acting. This is the part Centola's reinforcement is really
//     about, and the first version asserted it in a comment while implementing
//     the opposite.
//
// The result still rises with exposure -- amplification still works, and a feed
// can still carry a contagion that peer ties structurally cannot -- but it can
// no longer clear an arbitrary threshold in one tick, so the threshold rule
// goes on doing its job.
func effectiveConfirmations(shown int) int {
	if shown <= 0 {
		return 0
	}
	n := int(feedTrust * math.Sqrt(float64(shown)))
	if n > maxFeedConfirm {
		n = maxFeedConfirm
	}
	return n
}

const (
	// feedTrust is how much a post from a stranger is worth against a friend
	// acting. Below 1 by construction; the exact value is a judgement.
	feedTrust = 0.9

	// maxFeedConfirm caps what any feed can contribute in one tick. Without it
	// a very hot platform hands somebody enough confirmations to clear any
	// threshold, which switches the threshold rule off rather than pushing
	// against it.
	maxFeedConfirm = 4
)

// poisson draws a small Poisson count by Knuth's method.
//
// Exact rather than approximate, and bounded: lambda here is at most a few, so
// the loop terminates almost immediately, but the cap keeps a pathological
// parameter from turning one persona's update into an unbounded loop.
func poisson(r *rng, lambda float64) int {
	if lambda <= 0 {
		return 0
	}
	if lambda > 12 {
		lambda = 12
	}
	l := math.Exp(-lambda)
	k, p := 0, 1.0
	for k < 32 {
		p *= r.f64()
		if p <= l {
			break
		}
		k++
	}
	return k
}

// PlatformStat is the per-platform view the UI reports.
type PlatformStat struct {
	Name     string  `json:"name"`
	Slug     string  `json:"slug"`
	Sketch   string  `json:"sketch"`
	Members  float64 `json:"members"`  // weighted share of the population
	Score    float64 `json:"score"`    // room the topic has on the platform, 0-1
	Presence float64 `json:"presence"` // share of feed slots ranking gave it
	Peak     float64 `json:"peak"`     // high-water mark of Presence this story
	PeakAt   int     `json:"peak_at"`  // step it peaked; earlier = more amplified
	FeedMean float64 `json:"feed_mean"`
	Amplify  float64 `json:"amplify"`
	Sorting  float64 `json:"sorting"`
	Closed   bool    `json:"closed"`
	Reached  float64 `json:"reached"` // weighted share this platform exposed
}

// Stats reports each platform's state. Shares are population-weighted, like
// every other aggregate here.
func (m *Media) Stats(w *world.World) []PlatformStat {
	if m == nil {
		return nil
	}
	np := len(m.Platforms)
	members := make([]float64, np)
	reached := make([]float64, np)
	var den float64
	for i := 0; i < w.N; i++ {
		wt := float64(w.Weight[i])
		den += wt
		mask := m.Member[i]
		if mask == 0 {
			continue
		}
		ex := m.Exposed[i]
		for p := 0; p < np; p++ {
			if mask&(1<<uint(p)) == 0 {
				continue
			}
			members[p] += wt
			if ex {
				reached[p] += wt
			}
		}
	}
	out := make([]PlatformStat, np)
	for p := range m.Platforms {
		pl := &m.Platforms[p]
		out[p] = PlatformStat{
			Name: pl.Name, Slug: pl.Slug, Sketch: pl.Sketch,
			Score: m.Score[p], Presence: m.Presence[p], Peak: m.Peak[p],
			PeakAt: m.PeakAt[p], FeedMean: m.FeedMean[p],
			Amplify: pl.Amplify, Sorting: pl.Sorting, Closed: pl.Closed,
		}
		if den > 0 {
			out[p].Members = members[p] / den
			out[p].Reached = reached[p] / den
		}
	}
	return out
}

// ExposedCount is the number of personas the feed reached this tick.
func (m *Media) ExposedCount() int {
	if !m.Enabled() {
		return 0
	}
	n := 0
	for _, e := range m.Exposed {
		if e {
			n++
		}
	}
	return n
}
