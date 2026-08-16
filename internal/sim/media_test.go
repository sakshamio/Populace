package sim

import (
	"math"
	"testing"

	"github.com/sakshamio/Populace/internal/world"
)

// run drives a story to its conclusion and reports what happened. Shared by
// every test below so that arms differ only in the thing under test.
func run(t testing.TB, w *world.World, cfg Config, ev Event, ticks int) Snapshot {
	t.Helper()
	s := New(w, cfg)
	s.Inject(ev)
	for i := 0; i < ticks; i++ {
		s.Advance()
	}
	return s.Snapshot(false)
}

// The control arm has to be exact, not approximately right. Every claim about
// platforms is a difference against "platforms off", so if that arm is not
// bit-identical to the pre-media model then the difference includes whatever
// the media code does when it thinks it is doing nothing.
func TestMediaOffIsExactlyThePreMediaModel(t *testing.T) {
	w := testWorld(t, testN)
	ev := Event{Headline: "control", Stance: -0.5, Salience: 0.7,
		Seeding: SeedInRegion, SeedSize: 600, Seed: 42, Difficulty: 1.2}

	noPlatforms := DefaultConfig()
	noPlatforms.Platforms = nil
	a := run(t, w, noPlatforms, ev, 40)

	// Same config, but built with platforms and then switched off. These must
	// agree: a disabled layer and an absent layer are the same model.
	withPlatforms := DefaultConfig()
	s := New(w, withPlatforms)
	s.M.SetEnabled(false)
	s.Inject(ev)
	for i := 0; i < 40; i++ {
		s.Advance()
	}
	b := s.Snapshot(false)

	if a.Adopters != b.Adopters {
		t.Errorf("absent vs disabled media differ: %d vs %d adopters", a.Adopters, b.Adopters)
	}
	if math.Abs(a.MeanOpinion-b.MeanOpinion) > 1e-9 {
		t.Errorf("opinion differs: %.12f vs %.12f", a.MeanOpinion, b.MeanOpinion)
	}
	if a.FeedShare != 0 || a.FeedReached != 0 {
		t.Errorf("media-off arm reported feed activity: share %.4f, reached %d",
			a.FeedShare, a.FeedReached)
	}
}

// The central claim of the whole media layer, stated so it can fail.
//
// Amplification does not make everything spread -- that was the first version
// of this file, and a model in which every story reaches everybody has stopped
// discriminating between stories. What it does is *move the critical mass*: a
// seed too small to tip the population on peer ties alone becomes large enough
// once a ranked feed is manufacturing confirmations.
//
// So the shape to test for is a threshold that moved, which means checking
// three points rather than one: below the new critical mass nothing much
// happens either way, between the two thresholds the platforms decide the
// outcome, and above the old one both tip and the platforms are irrelevant.
// A single comparison could be satisfied by a layer that merely adds a
// constant, and that is a different claim.
func TestAmplificationLowersTheCriticalMass(t *testing.T) {
	w := testWorld(t, testN)
	seedAt := func(k int) (off, on Snapshot) {
		ev := Event{Headline: "scattered", Stance: 0.3, Salience: 0.9,
			Seeding: SeedEverywhere, SeedSize: k, Seed: 7, Difficulty: 1.0}
		cfgOff := DefaultConfig()
		cfgOff.Platforms = nil
		return run(t, w, cfgOff, ev, 40), run(t, w, DefaultConfig(), ev, 40)
	}

	// Below both thresholds: platforms help a little and tip nothing.
	lowOff, lowOn := seedAt(240)
	// Between the two: this is where the layer earns its place.
	midOff, midOn := seedAt(720)
	// Above the old threshold: it was always going to tip.
	hiOff, hiOn := seedAt(960)

	t.Logf("0.4%% seeded   off %7.4f%%   on %7.4f%%", lowOff.Reach*100, lowOn.Reach*100)
	t.Logf("1.2%% seeded   off %7.4f%%   on %7.4f%%   <- the interesting one", midOff.Reach*100, midOn.Reach*100)
	t.Logf("1.6%% seeded   off %7.4f%%   on %7.4f%%", hiOff.Reach*100, hiOn.Reach*100)

	if lowOn.Reach > 0.5 {
		t.Errorf("a 0.4%% seed tipped the world with platforms on (%.2f%%) -- "+
			"amplification has removed the critical mass rather than lowering it",
			lowOn.Reach*100)
	}
	if midOff.Reach > 0.5 {
		t.Fatalf("the 1.2%% seed already tips without platforms (%.2f%%), so this "+
			"test is not measuring what it claims", midOff.Reach*100)
	}
	if midOn.Reach < 0.5 {
		t.Errorf("platforms failed to tip a 1.2%% seed (%.2f%%) that dies without "+
			"them (%.2f%%) -- amplification is not moving the threshold",
			midOn.Reach*100, midOff.Reach*100)
	}
	if hiOff.Reach < 0.5 {
		t.Errorf("the 1.6%% control should tip on peer ties alone, got %.2f%%", hiOff.Reach*100)
	}
}

// Attribution has to agree with the outcome. In the regime where platforms
// decide it, a non-trivial share of adopters must be ones that could not have
// cleared threshold on peers -- otherwise the extra reach came from somewhere
// the model is not accounting for.
//
// The share is deliberately not asserted to be large. Once the feed pushes a
// population past critical mass, ordinary peer contagion does most of the
// remaining work, so the algorithm's fingerprint is on the *start* of the
// cascade rather than on most of its volume. Expecting a majority here was my
// first guess and it was wrong for an interesting reason.
func TestFeedAttributionAgreesWithTheOutcome(t *testing.T) {
	w := testWorld(t, testN)
	ev := Event{Headline: "scattered", Stance: 0.3, Salience: 0.9,
		Seeding: SeedEverywhere, SeedSize: 720, Seed: 7, Difficulty: 1.0}
	got := run(t, w, DefaultConfig(), ev, 40)

	t.Logf("reach %.3f%%, of which feed-driven %.1f%% of adopters", got.Reach*100, got.FeedShare*100)
	if got.FeedShare <= 0.01 {
		t.Errorf("platforms tipped the cascade but only %.2f%% of adopters are "+
			"attributed to the feed -- the attribution is not tracking the mechanism",
			got.FeedShare*100)
	}
	if got.FeedShare > 0.9 {
		t.Errorf("%.1f%% of adopters attributed to the feed -- peer contagion "+
			"has stopped contributing, which means the feed is acting as a "+
			"broadcast channel rather than an amplifier", got.FeedShare*100)
	}
}

// A closed channel has no ranking function, so it must not rescue anything.
// This is the control that separates "platforms" from "amplification": if a
// group chat spread the scattered seed too, the mechanism being demonstrated
// would just be "more edges", not algorithmic amplification.
func TestClosedChannelsDoNotAmplify(t *testing.T) {
	w := testWorld(t, testN)
	ev := Event{Headline: "scattered", Stance: 0.3, Salience: 0.9,
		Seeding: SeedEverywhere, SeedSize: 720, Seed: 7, Difficulty: 1.0}

	off := DefaultConfig()
	off.Platforms = nil
	dead := run(t, w, off, ev, 40)

	// Only the closed channel, with the same reach as the real one.
	closedOnly := DefaultConfig()
	for _, p := range DefaultPlatforms() {
		if p.Closed {
			closedOnly.Platforms = []Platform{p}
		}
	}
	if closedOnly.Platforms == nil {
		t.Fatal("no closed platform in the default set")
	}
	quiet := run(t, w, closedOnly, ev, 40)

	t.Logf("scattered seed — none: %.4f%%   closed channel only: %.4f%%",
		dead.Reach*100, quiet.Reach*100)

	// Measured: it adds exactly nothing, and the reason is worth knowing rather
	// than treating as a rounding artefact. A closed channel takes a fair sample
	// of what is circulating, so at 1.2% engagement it puts the story in front
	// of ~1% of members per tick -- one post. One post, after the correlation
	// discount in effectiveConfirmations, is worth less than one independent
	// confirmation and therefore counts as zero. A channel with no ranking
	// cannot lift anyone over a threshold on its own; it can only reinforce
	// what the peer graph is already doing, which is precisely the claim.
	if quiet.Reach > dead.Reach*10+0.01 {
		t.Errorf("a channel with no ranking function produced a cascade: "+
			"%.4f%% vs %.4f%% -- amplification is not what is being measured",
			quiet.Reach*100, dead.Reach*100)
	}
}

// Sorting is the echo-chamber knob, and the honest observable is the *peak* of
// opinion variance, not its endpoint.
//
// That distinction is a real property of the model rather than a measurement
// convenience, and it took a failed test to notice. Friedkin-Johnsen has a
// unique fixed point determined by the priors and the graph. The feed is not in
// that fixed point: once a story burns out, engagement goes to zero, presence
// goes to zero, and the population relaxes back to exactly where it would have
// been. So a sorted feed cannot move the equilibrium at all -- it holds people
// apart *while the story is live*, and the divergence is unwound afterwards.
//
// Both halves are asserted here. The transient must grow with sorting, and the
// endpoint must not, because a version of this model where feeds permanently
// repositioned a population would be claiming something FJ does not support.
func TestSortedFeedsHoldPeopleApartWhileAStoryIsLive(t *testing.T) {
	w := testWorld(t, testN)
	ev := Event{Headline: "contested", Stance: -0.7, Salience: 0.85,
		Seeding: SeedInRegion, SeedSize: 900, Seed: 3, Difficulty: 0.9}

	peakAndFinal := func(sorting float64) (peak, final float64) {
		cfg := DefaultConfig()
		plats := DefaultPlatforms()
		for i := range plats {
			plats[i].Sorting = sorting
		}
		cfg.Platforms = plats
		s := New(w, cfg)
		s.Inject(ev)
		for i := 0; i < 60; i++ {
			s.Advance()
			if v := s.Snapshot(false).Variance; v > peak {
				peak = v
			}
		}
		return peak, s.Snapshot(false).Variance
	}

	openPeak, openFinal := peakAndFinal(0.0)
	midPeak, _ := peakAndFinal(0.5)
	echoPeak, echoFinal := peakAndFinal(0.95)

	t.Logf("peak variance — chronological %.6f   half-sorted %.6f   echo chamber %.6f (%.2f×)",
		openPeak, midPeak, echoPeak, echoPeak/math.Max(openPeak, 1e-12))
	t.Logf("final variance — chronological %.6f   echo chamber %.6f", openFinal, echoFinal)

	if !(echoPeak > midPeak && midPeak > openPeak) {
		t.Errorf("peak disagreement did not rise monotonically with sorting: "+
			"%.6f / %.6f / %.6f", openPeak, midPeak, echoPeak)
	}
	if math.Abs(echoFinal-openFinal) > 1e-6 {
		t.Errorf("sorting moved the equilibrium (%.9f vs %.9f). FJ has a unique "+
			"fixed point and a quiet feed contributes nothing to it, so this "+
			"means the feed is leaking into the priors", echoFinal, openFinal)
	}
}

// Determinism, for the same reason the graph has one: the media layer draws
// per person per platform in parallel, and an RNG keyed by anything other than
// (seed, step, person, platform) would make the result depend on how the work
// was chunked across goroutines.
func TestMediaIsDeterministic(t *testing.T) {
	w := testWorld(t, 20_000)
	ev := Event{Headline: "same twice", Stance: 0.2, Salience: 0.6,
		Seeding: SeedInRegion, SeedSize: 300, Seed: 99, Difficulty: 1.0}

	a := run(t, w, DefaultConfig(), ev, 25)
	b := run(t, w, DefaultConfig(), ev, 25)

	if a.Adopters != b.Adopters || a.FeedReached != b.FeedReached {
		t.Errorf("two identical runs diverged: adopters %d vs %d, feed-reached %d vs %d",
			a.Adopters, b.Adopters, a.FeedReached, b.FeedReached)
	}
	if math.Abs(a.MeanOpinion-b.MeanOpinion) > 1e-12 {
		t.Errorf("opinion diverged: %.15f vs %.15f", a.MeanOpinion, b.MeanOpinion)
	}
}

// Platforms must draw different userbases, or the six of them are one platform
// with six parameter sets and every per-platform result is really about the
// parameters rather than about who is in the room.
func TestPlatformsDrawDifferentPopulations(t *testing.T) {
	w := testWorld(t, testN)
	m := NewMedia(w, DefaultPlatforms(), 0xFEED5)
	stats := m.Stats(w)

	for _, st := range stats {
		t.Logf("%-16s members %5.1f%%  amplify %.1f  sorting %.2f", st.Name, st.Members*100, st.Amplify, st.Sorting)
		if st.Members <= 0.001 || st.Members >= 0.995 {
			t.Errorf("%s has %.3f%% of the population -- a platform nobody or "+
				"everybody is on cannot differ from any other", st.Name, st.Members*100)
		}
	}

	// The userbases must actually differ in composition, not just in size.
	// Compare mean archetype openness across two platforms picked for opposite
	// skew: the short-video feed tilts open, broadcast tilts the other way.
	meanOpen := func(p int) float64 {
		var sum, n float64
		for i := 0; i < w.N; i++ {
			if m.Member[i]&(1<<uint(p)) == 0 {
				continue
			}
			o, _ := world.Archetype(w.Arch[i]).Values()
			sum += o
			n++
		}
		return sum / math.Max(n, 1)
	}
	var short, broadcast int
	for i, p := range m.Platforms {
		switch p.Slug {
		case "shortvideo":
			short = i
		case "broadcast":
			broadcast = i
		}
	}
	so, bo := meanOpen(short), meanOpen(broadcast)
	t.Logf("mean openness — short video %.3f, broadcast %.3f", so, bo)
	if so <= bo {
		t.Errorf("platform userbases are not sorted by openness as configured: "+
			"short video %.3f, broadcast %.3f", so, bo)
	}
}

// Amplification has to be a dial rather than a switch, or it cannot be swept
// and no claim about "how much amplification" means anything.
func TestReachRisesWithAmplification(t *testing.T) {
	w := testWorld(t, testN)
	ev := Event{Headline: "sweep", Stance: 0.1, Salience: 0.8,
		Seeding: SeedEverywhere, SeedSize: 600, Seed: 11, Difficulty: 1.0}

	var last float64 = -1
	for _, amp := range []float64{0, 1, 2, 4} {
		cfg := DefaultConfig()
		plats := DefaultPlatforms()
		for i := range plats {
			if !plats[i].Closed {
				plats[i].Amplify = amp
			}
		}
		cfg.Platforms = plats
		got := run(t, w, cfg, ev, 40)
		t.Logf("amplify %.0f → reach %.3f%%  feed-driven %.1f%%", amp, got.Reach*100, got.FeedShare*100)
		if got.Reach < last-1e-9 {
			t.Errorf("reach fell as amplification rose: %.5f at %.0f after %.5f",
				got.Reach, amp, last)
		}
		last = got.Reach
	}
}

// Poisson is used in the exposure draw; a wrong mean would bias every
// amplification result in the same direction and look like a modelling choice.
func TestPoissonHasTheRightMean(t *testing.T) {
	for _, lambda := range []float64{0.25, 1, 3} {
		r := newRNG(0xABCDEF, 7)
		sum, n := 0, 200_000
		for i := 0; i < n; i++ {
			sum += poisson(&r, lambda)
		}
		got := float64(sum) / float64(n)
		if math.Abs(got-lambda) > lambda*0.05+0.02 {
			t.Errorf("poisson(%.2f) mean %.4f, want %.2f", lambda, got, lambda)
		}
	}
}
