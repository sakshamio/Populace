package sim

import (
	"bytes"
	"math"
	"testing"

	"github.com/sakshamio/Populace/internal/world"
)

const testN = 60_000

func testWorld(tb testing.TB, n int) *world.World {
	tb.Helper()
	return world.Generate(n, 20260815)
}

func testGraph(tb testing.TB, w *world.World) *Graph {
	tb.Helper()
	return BuildGraph(w, DefaultGraphConfig())
}

// The CSR is built with atomic cursors, so within-row order comes out in
// whatever order the goroutines got there. If the sort-and-dedup pass did not
// run, two identical runs would produce different float accumulation orders in
// the opinion update and diverge. This is the test that catches that.
func TestGraphIsDeterministic(t *testing.T) {
	w := testWorld(t, 20_000)
	a := testGraph(t, w)
	b := testGraph(t, w)

	if a.Edges != b.Edges {
		t.Fatalf("edge counts differ: %d vs %d", a.Edges, b.Edges)
	}
	for i := 0; i < a.N; i++ {
		x, y := a.Neighbours(i), b.Neighbours(i)
		if len(x) != len(y) {
			t.Fatalf("node %d degree %d vs %d", i, len(x), len(y))
		}
		for k := range x {
			if x[k] != y[k] {
				t.Fatalf("node %d slot %d: %d vs %d", i, k, x[k], y[k])
			}
		}
	}
	t.Logf("two builds identical across %d nodes, %d edges", a.N, a.Edges)
}

// Symmetry is not decorative. The opinion update reads a node's neighbours to
// decide who influences it; an asymmetric graph would mean influence flowed
// one way along ties that are supposed to be mutual, and the equilibrium would
// be wrong in a way no aggregate would reveal.
func TestGraphIsSymmetricAndSimple(t *testing.T) {
	w := testWorld(t, 20_000)
	g := testGraph(t, w)

	has := func(a, b int32) bool {
		for _, v := range g.Neighbours(int(a)) {
			if v == b {
				return true
			}
		}
		return false
	}
	for i := 0; i < g.N; i++ {
		row := g.Neighbours(i)
		for k, j := range row {
			if int(j) == i {
				t.Fatalf("node %d has a self-loop", i)
			}
			if k > 0 && row[k-1] == j {
				t.Fatalf("node %d has a duplicate edge to %d", i, j)
			}
			if k > 0 && row[k-1] > j {
				t.Fatalf("node %d row is not sorted", i)
			}
			if !has(j, int32(i)) {
				t.Fatalf("edge %d->%d has no reverse", i, j)
			}
		}
	}
	t.Logf("%d edges, all symmetric, no self-loops or duplicates", g.Edges)
}

// A network with the right mean degree and no tail behaves nothing like a real
// one: without broadcast nodes there is no mechanism by which a story reaches
// distant regions at all, and every cascade measurement is a measurement of
// the wrong network.
func TestDegreeDistributionHasATail(t *testing.T) {
	w := testWorld(t, testN)
	g := testGraph(t, w)
	s := g.DegreeStats(w)

	if s.P99 <= s.P50*2 {
		t.Fatalf("degree distribution has no tail: p50=%d p99=%d", s.P50, s.P99)
	}
	if s.MediaMeanDegree < s.Mean*5 {
		t.Fatalf("media nodes are not broadcasters: mean %.1f vs population %.1f",
			s.MediaMeanDegree, s.Mean)
	}
	frac := float64(s.Isolated) / float64(g.N)
	if frac > 0.02 {
		t.Fatalf("%.1f%% of nodes are isolated", frac*100)
	}
	t.Logf("degree mean %.1f  p50 %d  p90 %d  p99 %d  max %d  media mean %.0f  isolated %.2f%%",
		s.Mean, s.P50, s.P90, s.P99, s.Max, s.MediaMeanDegree, frac*100)
}

// If the graph shatters, nothing can spread and every result is an artefact of
// the sampler rather than of the dynamics.
func TestGiantComponentCoversTheWorld(t *testing.T) {
	w := testWorld(t, testN)
	g := testGraph(t, w)

	seen := make([]bool, g.N)
	best := 0
	for start := 0; start < g.N; start++ {
		if seen[start] {
			continue
		}
		size := 0
		queue := []int32{int32(start)}
		seen[start] = true
		for len(queue) > 0 {
			v := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			size++
			for _, u := range g.Neighbours(int(v)) {
				if !seen[u] {
					seen[u] = true
					queue = append(queue, u)
				}
			}
		}
		if size > best {
			best = size
		}
		if best > g.N/2 {
			break // a second component this large cannot exist
		}
	}
	share := float64(best) / float64(g.N)
	if share < 0.90 {
		t.Fatalf("giant component covers only %.1f%% of the population", share*100)
	}
	t.Logf("giant component: %.2f%% of %d nodes", share*100, g.N)
}

// The headline claim, and the reason the phase-1 renderer's expanding circle
// was labelled a placeholder: a single-exposure front overstates reach.
//
// Same graph, same seed set, same RNG streams. Only the transmission rule
// changes.
func TestComplexContagionReachesLessThanSimple(t *testing.T) {
	w := testWorld(t, testN)
	g := testGraph(t, w)

	run := func(simple bool, beta float64) (int, int) {
		c := NewContagion(w, DefaultContagionConfig())
		c.Simple, c.Beta = simple, beta
		c.SeedScattered(200, 99)
		rounds, _ := c.Run(g, 200)
		return rounds, c.Count()
	}

	simpleRounds, simpleN := run(true, 0.12)
	complexRounds, complexN := run(false, 0)

	if complexN >= simpleN {
		t.Fatalf("complex contagion reached %d, simple reached %d -- "+
			"the threshold rule is not binding", complexN, simpleN)
	}
	t.Logf("from 200 scattered seeds in a population of %d:", testN)
	t.Logf("  simple  (β=0.12): %6d adopters in %d rounds", simpleN, simpleRounds)
	t.Logf("  complex (θ~0.18): %6d adopters in %d rounds", complexN, complexRounds)
	t.Logf("  single-exposure overstates reach by %.1fx",
		float64(simpleN)/math.Max(float64(complexN), 1))
}

// Centola's result, and the one with a practical consequence: a complex
// contagion is limited by local density, not by connectivity. The same number
// of seeds placed inside one neighbourhood manufactures the reinforcement the
// threshold rule demands; scattered across the world, each seed is one
// exposure to each of its neighbours and nothing moves.
//
// This is the difference between a broadcast launch and a targeted one, and it
// is why the seeding strategy is a first-class field on Event.
func TestComplexContagionNeedsAClusteredSeed(t *testing.T) {
	w := testWorld(t, testN)
	g := testGraph(t, w)
	cfg := DefaultContagionConfig()

	scattered := NewContagion(w, cfg)
	scattered.SeedScattered(400, 7)
	scattered.Run(g, 200)

	clustered := NewContagion(w, cfg)
	// A well-connected *ordinary* person, not the highest-degree node overall
	// -- that one is a broadcaster, and seeding outward from a broadcaster is
	// a different experiment entirely (see TestBroadcastSeedingIsWideButShallow).
	start, bestDeg := int32(0), 0
	for i := 0; i < g.N; i++ {
		if world.Stratum(w.Strat[i]) == world.Media {
			continue
		}
		if d := g.Degree(i); d > bestDeg {
			start, bestDeg = int32(i), d
		}
	}
	clustered.SeedCluster(g, start, 400)
	clustered.Run(g, 200)

	sN, cN := scattered.Count(), clustered.Count()
	if cN <= sN {
		t.Fatalf("clustered seeding (%d) did not beat scattered (%d); "+
			"the model is not reinforcement-limited", cN, sN)
	}
	t.Logf("400 seeds, complex contagion:")
	t.Logf("  scattered worldwide: %6d adopters", sN)
	t.Logf("  one neighbourhood:   %6d adopters  (%.1fx)", cN,
		float64(cN)/math.Max(float64(sN), 1))
}

// FJ is a contraction with modulus max λ, so this must converge. If it stops
// converging the bug is in the update, not in the problem.
func TestOpinionConverges(t *testing.T) {
	w := testWorld(t, 20_000)
	g := testGraph(t, w)
	o := NewOpinion(w, 1234)

	first := o.Step(g)
	rounds, resid := o.Settle(g, 400, 1e-6)
	if resid > 1e-6 {
		t.Fatalf("did not converge: residual %g after %d rounds", resid, rounds)
	}
	t.Logf("converged in %d rounds; residual %g → %g", rounds+1, first, resid)
}

// The reason for FJ over DeGroot, stated as a test. DeGroot averaging drives
// any connected population to a single shared opinion, which is not a property
// the world has. The stubborn prior is what preserves disagreement.
func TestStubbornPriorsPreserveDisagreement(t *testing.T) {
	w := testWorld(t, 20_000)
	g := testGraph(t, w)

	fj := NewOpinion(w, 1234)
	fj.Settle(g, 400, 1e-7)
	fjVar := fj.Variance(w)

	dg := NewOpinion(w, 1234)
	for i := range dg.Lambda { // λ = 1 everywhere is exactly DeGroot
		dg.Lambda[i] = 1
	}
	dg.Settle(g, 400, 1e-7)
	dgVar := dg.Variance(w)

	if fjVar < dgVar*5 {
		t.Fatalf("FJ variance %.5f is not meaningfully above DeGroot %.5f",
			fjVar, dgVar)
	}
	t.Logf("equilibrium opinion variance: Friedkin-Johnsen %.5f, DeGroot %.3g "+
		"-- DeGroot collapses to consensus", fjVar, dgVar)
}

// Broadcasters are how an event enters the world. Pinning them should move the
// population mean without pinning anyone else.
func TestBroadcastMovesThePopulation(t *testing.T) {
	w := testWorld(t, 20_000)
	g := testGraph(t, w)
	o := NewOpinion(w, 1234)
	o.Settle(g, 300, 1e-6)
	before := o.Mean(w)

	var media []int32
	for i := 0; i < w.N; i++ {
		if world.Stratum(w.Strat[i]) == world.Media {
			media = append(media, int32(i))
		}
	}
	o.Broadcast(media, 1.0)
	o.Settle(g, 300, 1e-6)
	after := o.Mean(w)

	if after <= before {
		t.Fatalf("unanimous positive coverage did not move the mean: %.4f → %.4f",
			before, after)
	}
	t.Logf("%d broadcasters pinned to +1.0 moved the weighted mean %.4f → %.4f",
		len(media), before, after)
}

// The delta stream is what makes ticking at ten million affordable: a full
// flags resend is 20 MB, and a tick that changes a few thousand people should
// cost a few thousand records.
func TestDeltaCarriesOnlyChanges(t *testing.T) {
	w := testWorld(t, 20_000)
	s := New(w, DefaultConfig())

	var buf bytes.Buffer
	if n, full, _ := s.Encode(&buf); n != 0 || full {
		t.Fatalf("a fresh sim reported %d pending changes (full=%v)", n, full)
	}

	s.Inject(Event{Stance: 1, Salience: 1, Seeding: SeedInRegion, SeedSize: 50, Seed: 5})
	buf.Reset()
	n, full, err := s.Encode(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if full {
		t.Fatalf("an injection touching a neighbourhood should not need a full frame")
	}
	if n == 0 || n >= w.N {
		t.Fatalf("delta of %d records is not a delta (population %d)", n, w.N)
	}
	if want := 12 + 6*n; buf.Len() != want {
		t.Fatalf("wire size %d, want %d", buf.Len(), want)
	}

	// Encoding twice in a row must be empty: the second call has nothing new.
	buf.Reset()
	if again, _, _ := s.Encode(&buf); again != 0 {
		t.Fatalf("re-encoding an unchanged sim produced %d records", again)
	}
	t.Logf("event touched %d of %d personas: %d bytes on the wire "+
		"against %d for a full resend", n, w.N, 12+6*n, w.N*2)
}

// The delta is only cheaper while the change set is small, and during an
// active event it is not: the opinion field moves for nearly everyone at once.
// The encoder must notice and switch, or the "optimisation" costs three times
// what it saves.
func TestFullFrameWinsWhenNearlyEverythingMoves(t *testing.T) {
	w := testWorld(t, 20_000)
	s := New(w, DefaultConfig())
	s.Inject(Event{Stance: -1, Salience: 1, Seeding: SeedInRegion, SeedSize: 200, Seed: 3})

	var buf bytes.Buffer
	s.Encode(&buf) // drain the injection

	s.Advance() // one round of opinion movement touches almost everyone
	dirty := s.DirtyCount()
	buf.Reset()
	n, full, err := s.Encode(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !full {
		t.Fatalf("%d of %d dirty and still sent a delta (%d bytes vs %d full)",
			dirty, w.N, 12+6*dirty, 12+2*w.N)
	}
	if n != w.N || buf.Len() != 12+2*w.N {
		t.Fatalf("full frame is %d bytes for %d records, want %d for %d",
			buf.Len(), n, 12+2*w.N, w.N)
	}
	t.Logf("one tick dirtied %d of %d (%.0f%%): full frame %d bytes, "+
		"a delta would have been %d", dirty, w.N,
		float64(dirty)/float64(w.N)*100, buf.Len(), 12+6*dirty)
}

// An end-to-end tick, which is what the server will actually run.
func TestEventPlaysOut(t *testing.T) {
	w := testWorld(t, testN)
	s := New(w, DefaultConfig())

	before := s.Snapshot(false)
	s.Inject(Event{
		Headline: "Regulator opens inquiry into the largest carrier",
		Stance:   -0.8, Salience: 0.7,
		Seeding: SeedInRegion, SeedSize: 300, Seed: 42,
	})
	for i := 0; i < 30; i++ {
		s.Advance()
	}
	after := s.Snapshot(false)

	if after.Reach <= 0 {
		t.Fatal("nothing spread")
	}
	if after.MeanOpinion >= before.MeanOpinion {
		t.Fatalf("negative coverage did not move opinion: %.4f → %.4f",
			before.MeanOpinion, after.MeanOpinion)
	}
	t.Logf("30 ticks: reach %.2f%% of the weighted population, "+
		"mean opinion %.4f → %.4f, against/for %.1f%%/%.1f%%",
		after.Reach*100, before.MeanOpinion, after.MeanOpinion,
		after.NegShare*100, after.PosShare*100)
}

func BenchmarkBuildGraph(b *testing.B) {
	w := testWorld(b, 1_000_000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		BuildGraph(w, DefaultGraphConfig())
	}
}

func BenchmarkTick(b *testing.B) {
	w := testWorld(b, 1_000_000)
	s := New(w, DefaultConfig())
	s.Inject(Event{Stance: -1, Salience: 0.5, Seeding: SeedInRegion, SeedSize: 500, Seed: 1})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Advance()
	}
}

// Seeding a story through broadcasters reaches a lot of people once, and a
// complex contagion ignores being reached once. This is the mechanism behind
// "the campaign got great coverage and changed nobody's behaviour", and it
// falls out of the model rather than being assumed by it.
func TestBroadcastSeedingIsWideButShallow(t *testing.T) {
	w := testWorld(t, testN)
	g := testGraph(t, w)
	cfg := DefaultContagionConfig()

	// Find the biggest broadcaster and seed outward from it.
	star, bestDeg := int32(0), -1
	for i := 0; i < w.N; i++ {
		if world.Stratum(w.Strat[i]) == world.Media && g.Degree(i) > bestDeg {
			star, bestDeg = int32(i), g.Degree(i)
		}
	}
	viaStar := NewContagion(w, cfg)
	viaStar.SeedCluster(g, star, 400)
	viaStar.Run(g, 200)

	s := New(w, DefaultConfig())
	s.Inject(Event{Stance: -0.8, Salience: 0.7, Seeding: SeedInNetwork, SeedSize: 400, Seed: 42})
	s.C.Run(g, 200)

	if s.C.Count() <= viaStar.Count() {
		t.Fatalf("community seeding (%d) did not beat broadcaster seeding (%d)",
			s.C.Count(), viaStar.Count())
	}
	t.Logf("400 seeds, complex contagion:")
	t.Logf("  outward from a broadcaster (degree %d): %6d adopters", bestDeg, viaStar.Count())
	t.Logf("  outward from a dense community:         %6d adopters", s.C.Count())
}

// The three seeding strategies, same population, same graph, same budget.
//
// The assertion here is deliberately narrower than the one first written,
// which claimed region > network > scattered. That ordering is not robust:
// network seeding grows outward from a hub, and whether a hub-mediated cascade
// tips is unstable -- at 60k it took the whole population and at 1M it went
// nowhere. Asserting it would have baked a coin flip into the suite.
//
// What *is* robust is the mechanism: seeding a place produces a seed set whose
// members are tied to each other, and scattering produces one whose members
// are strangers. Reinforcement is the thing the threshold rule needs, so that
// is the thing to measure.
func TestSeedingInAPlaceCreatesReinforcement(t *testing.T) {
	w := testWorld(t, testN)
	cfg := DefaultConfig()
	s := New(w, cfg)
	const budget = 400

	density := func(ids []int32) (internalShare float64, withReinforcement int) {
		in := make(map[int32]bool, len(ids))
		for _, id := range ids {
			in[id] = true
		}
		internal, total := 0, 0
		for _, id := range ids {
			k := 0
			for _, j := range s.G.Neighbours(int(id)) {
				total++
				if in[j] {
					internal++
					k++
				}
			}
			if k >= 2 {
				withReinforcement++
			}
		}
		if total == 0 {
			return 0, 0
		}
		return float64(internal) / float64(total), withReinforcement
	}

	seed := func(mode Seeding) []int32 {
		c := NewContagion(w, cfg.Contagion)
		switch mode {
		case SeedInRegion:
			r := newRNG(2026^0x9111, 1)
			return c.SeedRegion(s.G, int32(r.u32()%uint32(w.N)), budget)
		case SeedInNetwork:
			return c.SeedCluster(s.G, s.denseStart(2026), budget)
		default:
			return c.SeedScattered(budget, 2026)
		}
	}

	rShare, rReady := density(seed(SeedInRegion))
	nShare, nReady := density(seed(SeedInNetwork))
	eShare, eReady := density(seed(SeedEverywhere))

	if rShare <= nShare || nShare <= eShare {
		t.Fatalf("internal tie share should fall region > network > scattered, got %.3f / %.3f / %.3f",
			rShare, nShare, eShare)
	}
	if rReady < budget/2 {
		t.Fatalf("only %d of %d regional seeds start with reinforcement", rReady, budget)
	}
	t.Logf("%d seeds: share of ties that stay inside the seed set, and how many "+
		"seeds begin with the two exposures the threshold rule needs", budget)
	t.Logf("  one region (a place):      %5.1f%%   %3d seeds", rShare*100, rReady)
	t.Logf("  one network neighbourhood: %5.1f%%   %3d seeds", nShare*100, nReady)
	t.Logf("  scattered worldwide:       %5.1f%%   %3d seeds", eShare*100, eReady)
}

// The validation that matters most, and the one that says the dynamics are a
// property of the modelled network rather than of how many personas happen to
// be in the sample: seed the same *fraction* of two populations sixteen times
// apart in size, and the same fraction adopts.
//
// This is what makes a result at one million mean anything about a world of
// eight billion. If reach tracked seed count instead, every number the app
// reported would be an artefact of the sampler.
func TestReachDependsOnSeededFractionNotCount(t *testing.T) {
	fracs := []float64{0.001, 0.004, 0.016}
	got := map[int][]float64{}

	for _, n := range []int{60_000, 1_000_000} {
		w := testWorld(t, n)
		cfg := DefaultConfig()
		s := New(w, cfg)
		for _, f := range fracs {
			c := NewContagion(w, cfg.Contagion)
			r := newRNG(2026^0x9111, 1)
			c.SeedRegion(s.G, int32(r.u32()%uint32(n)), int(float64(n)*f))
			c.Run(s.G, 400)
			got[n] = append(got[n], float64(c.Count())/float64(n))
		}
	}

	for k, f := range fracs {
		small, large := got[60_000][k], got[1_000_000][k]
		if rel := math.Abs(small-large) / math.Max(small, large); rel > 0.15 {
			t.Fatalf("seeding %.1f%%: 60k reached %.3f%% but 1M reached %.3f%% "+
				"(%.0f%% apart) -- reach is tracking sample size, not structure",
				f*100, small*100, large*100, rel*100)
		}
		t.Logf("seed %.1f%%  ->  60k: %.3f%% adopt   1M: %.3f%% adopt   "+
			"(amplification %.2fx)", f*100, small*100, large*100, large/f)
	}
}
