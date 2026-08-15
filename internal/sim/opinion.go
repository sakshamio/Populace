package sim

import (
	"math"

	"github.com/sakshamio/Populace/internal/world"
)

// Opinion implements the Friedkin-Johnsen model of social influence.
//
//	y_i(t+1) = λ_i · Σ_j W_ij y_j(t)  +  (1 − λ_i) · s_i
//
// The second term is the whole reason to use FJ rather than the DeGroot
// averaging model it extends. Under DeGroot (λ = 1 for everyone) opinions
// always converge to a single consensus value on any connected graph, which is
// not a property the real world has. FJ gives each agent a *stubborn prior* s_i
// it never fully abandons, and the equilibrium is a persistent disagreement
// whose shape depends on the network -- which is the thing worth simulating.
//
// λ_i is susceptibility. λ = 0 is an immovable broadcaster; λ = 1 is a pure
// conformist with no views of their own. Both extremes exist and both are rare.
//
// Numerically, the update is a contraction with modulus max λ_i, so it converges
// geometrically and has a unique fixed point whenever any agent is stubborn.
// TestConverges checks that empirically rather than trusting the algebra.
type Opinion struct {
	Y      []float32 // current opinion, in [-1, 1]
	Prior  []float32 // s_i, the anchor an agent never fully leaves
	Lambda []float32 // λ_i, susceptibility to neighbours

	next []float32 // double buffer; see Step
}

// NewOpinion draws priors and susceptibilities from the population.
//
// Susceptibility is not uniform across strata: media personas are modelled as
// near-immovable broadcasters, because an outlet's published position does not
// drift toward its readers within the horizon of a simulated week.
func NewOpinion(w *world.World, topicSeed uint64) *Opinion {
	// Each topic is a random direction in the (openness, security) plane, so
	// different topics divide the population along different seams. A single
	// fixed axis would make every story split the world the same way.
	ax := newRNG(topicSeed, 0xA5A5)
	theta := ax.f64() * 2 * math.Pi
	axisA, axisB := math.Cos(theta), math.Sin(theta)

	o := &Opinion{
		Y:      make([]float32, w.N),
		Prior:  make([]float32, w.N),
		Lambda: make([]float32, w.N),
		next:   make([]float32, w.N),
	}
	parallel(w.N, func(lo, hi int) {
		for i := lo; i < hi; i++ {
			r := newRNG(topicSeed, uint64(i))

			// The prior is a projection of the archetype's values onto the
			// topic, not a random number keyed by archetype.
			//
			// That distinction is load-bearing. With a random per-archetype
			// base, two archetypes that agree on one topic are no more likely
			// to agree on the next, so the graph's homophily connects people
			// who have nothing in common and the clustering means nothing.
			// Projecting a shared value space makes agreement transitive
			// across topics, which is the property that makes a simulated
			// population feel like one rather than like noise with labels.
			open, sec := world.Archetype(w.Arch[i]).Values()
			base := axisA*open + axisB*sec
			s := clampF(base*0.8+r.norm()*0.3, -1, 1)
			o.Prior[i] = float32(s)
			o.Y[i] = float32(s)

			lam := 0.35 + 0.45*r.f64()
			switch world.Stratum(w.Strat[i]) {
			case world.Media:
				lam = 0.02 + 0.06*r.f64()
			case world.HighNetWorth:
				lam = 0.15 + 0.25*r.f64()
			}
			o.Lambda[i] = float32(lam)
		}
	})
	return o
}

// Step advances every opinion one round and returns the maximum absolute
// change, which is the convergence signal worth watching.
//
// Synchronous update into a second buffer, then swap. Updating in place would
// be Gauss-Seidel rather than Jacobi: agent i would see some neighbours' new
// values and some old ones, depending entirely on which goroutine ran first.
// That converges too -- often faster -- but it is not the model, and it makes
// the result depend on the scheduler. Two runs of the same seed would differ.
func (o *Opinion) Step(g *Graph) float64 {
	partial := make([]float64, numWorkers(len(o.Y)))
	parallelIdx(len(o.Y), func(c, lo, hi int) {
		local := 0.0
		for i := lo; i < hi; i++ {
			nb := g.Neighbours(i)
			y := o.Y[i]
			if len(nb) == 0 {
				o.next[i] = y
				continue
			}
			// Uniform weights: W is row-stochastic by construction, which is
			// what FJ requires. Non-uniform tie strengths would go here, and
			// would need renormalising per row rather than per edge.
			var sum float32
			for _, j := range nb {
				sum += o.Y[j]
			}
			avg := sum / float32(len(nb))

			lam := o.Lambda[i]
			nv := lam*avg + (1-lam)*o.Prior[i]
			o.next[i] = nv
			if d := math.Abs(float64(nv - y)); d > local {
				local = d
			}
		}
		partial[c] = local
	})
	maxDelta := 0.0
	for _, d := range partial {
		if d > maxDelta {
			maxDelta = d
		}
	}
	o.Y, o.next = o.next, o.Y
	return maxDelta
}

// Settle iterates to a fixed point, or gives up. Returns rounds taken and the
// final residual; a residual above tol means it did not converge, which with a
// contraction mapping means a bug rather than a hard problem.
func (o *Opinion) Settle(g *Graph, maxRounds int, tol float64) (int, float64) {
	d := math.Inf(1)
	r := 0
	for ; r < maxRounds; r++ {
		d = o.Step(g)
		if d < tol {
			return r + 1, d
		}
	}
	return r, d
}

// Broadcast pins a set of agents to a position and makes them immovable, which
// is how an event enters the network: a story does not change everyone's mind
// directly, it changes what the broadcasters are saying, and the network does
// the rest.
//
// Returns the previous λ values so the pin can be lifted later.
func (o *Opinion) Broadcast(ids []int32, stance float32) []float32 {
	prev := make([]float32, len(ids))
	for k, id := range ids {
		prev[k] = o.Lambda[id]
		o.Lambda[id] = 0
		o.Prior[id] = stance
		o.Y[id] = stance
	}
	return prev
}

// Mean is the population-weighted mean opinion.
//
// Weighted, and there is no unweighted variant here for the same reason there
// is none in package world: the strata are deliberately oversampled, so a raw
// mean over personas would report the opinion of a population that does not
// exist -- one that is 0.3% journalists.
func (o *Opinion) Mean(w *world.World) float64 {
	var num, den float64
	for i := 0; i < w.N; i++ {
		wt := float64(w.Weight[i])
		num += wt * float64(o.Y[i])
		den += wt
	}
	if den == 0 {
		return 0
	}
	return num / den
}

// Variance is the weighted variance of opinion: the measure of how divided the
// population is. Under DeGroot this goes to zero; under FJ it does not, and
// that gap is the model's entire contribution.
func (o *Opinion) Variance(w *world.World) float64 {
	m := o.Mean(w)
	var num, den float64
	for i := 0; i < w.N; i++ {
		wt := float64(w.Weight[i])
		d := float64(o.Y[i]) - m
		num += wt * d * d
		den += wt
	}
	if den == 0 {
		return 0
	}
	return num / den
}

// Polarisation is the weighted share of the population holding a position at
// least `edge` away from neutral. More legible than variance for a UI, and it
// distinguishes a split population from a merely uncertain one.
func (o *Opinion) Polarisation(w *world.World, edge float64) (neg, pos float64) {
	var dn, dp, den float64
	for i := 0; i < w.N; i++ {
		wt := float64(w.Weight[i])
		den += wt
		switch y := float64(o.Y[i]); {
		case y <= -edge:
			dn += wt
		case y >= edge:
			dp += wt
		}
	}
	if den == 0 {
		return 0, 0
	}
	return dn / den, dp / den
}

func clampF(v, lo, hi float64) float64 {
	return math.Max(lo, math.Min(hi, v))
}
