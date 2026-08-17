package experiment

import (
	"context"
	"math"
	"testing"

	"github.com/sakshamio/Populace/internal/sim"
)

// The whole point of the package: a difference that is obvious per replicate
// must come out as significant, and the paired design must find it with few
// replicates because it controls for the world.
func TestDifficultySeparatesArmsWithConfidence(t *testing.T) {
	r := NewRunner()
	res, err := r.Run(context.Background(), Config{
		N: 30_000, Replicates: 8, Rounds: 30, BaseSeed: 4242,
		Arms: []Arm{
			{Label: "cheap to pass on", SeedFrac: 0.01, Difficulty: 0.6,
				Seeding: sim.SeedInRegion, Salience: 0.6, Stance: -0.5},
			{Label: "costly to act on", SeedFrac: 0.01, Difficulty: 2.2,
				Seeding: sim.SeedInRegion, Salience: 0.6, Stance: -0.5},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Comparisons) != 1 {
		t.Fatalf("expected one comparison, got %d", len(res.Comparisons))
	}
	c := res.Comparisons[0]
	if !c.Significant || c.MeanDiff >= 0 {
		t.Fatalf("costly arm should reach materially less: diff %.4f [%.4f, %.4f] sig=%v",
			c.MeanDiff, c.Lo, c.Hi, c.Significant)
	}
	for _, a := range res.Arms {
		t.Logf("  %-18s reach %.1f%%  [%.1f, %.1f]  p10 %.1f  p90 %.1f  tipped %.0f%%",
			a.Arm.Label, a.Reach.Mean*100, a.Reach.Lo*100, a.Reach.Hi*100,
			a.Reach.P10*100, a.Reach.P90*100, a.Tipped*100)
	}
	t.Logf("  paired difference %.1fpp, 95%% CI [%.1f, %.1f], %d runs in %.1fs",
		c.MeanDiff*100, c.Lo*100, c.Hi*100, res.Runs, res.ElapsedS)
}

// The paired design applied to the day/night mechanic: the same story,
// breaking at the seed region's local night versus its local peak activity
// hour, should reach less early on -- and Config.DayNight has to actually be
// threaded through to sim.New, or every arm here runs with day/night off and
// "3am" and "2pm" would be identical requests.
func TestHourOfBreakingSeparatesArmsWhenDayNightIsOn(t *testing.T) {
	r := NewRunner()
	res, err := r.Run(context.Background(), Config{
		N: 30_000, Replicates: 8, Rounds: 12, BaseSeed: 777, DayNight: true,
		Arms: []Arm{
			{Label: "breaks at night", SeedFrac: 0.03, Difficulty: 0.8,
				Seeding: sim.SeedInRegion, Salience: 0.6, Stance: -0.5,
				HasHour: true, Hour: 2},
			{Label: "breaks at midday", SeedFrac: 0.03, Difficulty: 0.8,
				Seeding: sim.SeedInRegion, Salience: 0.6, Stance: -0.5,
				HasHour: true, Hour: 14},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := res.Comparisons[0]
	t.Logf("midday minus night: %+.1fpp, 95%% CI [%.1f, %.1f], significant=%v",
		c.MeanDiff*100, c.Lo*100, c.Hi*100, c.Significant)
	// pairedDiff(against, label, ...) reports label-minus-against -- "midday"
	// is the label here, "night" is against, so a positive MeanDiff means
	// midday reached more, which is what breaking into an awake population
	// should do.
	if c.MeanDiff <= 0 || !c.Significant {
		t.Errorf("breaking at midday should reach materially more than breaking at "+
			"night this early, got %+.4f significant=%v", c.MeanDiff, c.Significant)
	}
}

// Confirms the opposite failure mode of the test above: without DayNight set
// on the Config, an hour on an Arm must not silently do anything.
func TestHourIsInertWithoutDayNightOnTheConfig(t *testing.T) {
	r := NewRunner()
	res, err := r.Run(context.Background(), Config{
		N: 30_000, Replicates: 6, Rounds: 12, BaseSeed: 778, // DayNight left false
		Arms: []Arm{
			{Label: "night", SeedFrac: 0.03, Difficulty: 0.8, Seeding: sim.SeedInRegion,
				Salience: 0.6, Stance: -0.5, HasHour: true, Hour: 2},
			{Label: "midday", SeedFrac: 0.03, Difficulty: 0.8, Seeding: sim.SeedInRegion,
				Salience: 0.6, Stance: -0.5, HasHour: true, Hour: 14},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c := res.Comparisons[0]; c.MeanDiff != 0 {
		t.Errorf("Hour moved the result with DayNight off: diff %.6f", c.MeanDiff)
	}
}

// Two identical arms must not look different. This is the test that catches a
// broken reset: if state leaked between arms, the second arm would run on a
// population the first one modified and the "identical" arms would separate.
func TestIdenticalArmsDoNotSeparate(t *testing.T) {
	arm := Arm{SeedFrac: 0.008, Difficulty: 1.2, Seeding: sim.SeedInRegion,
		Salience: 0.6, Stance: -0.4}
	a, b := arm, arm
	a.Label, b.Label = "a", "b"

	r := NewRunner()
	res, err := r.Run(context.Background(), Config{
		N: 30_000, Replicates: 6, Rounds: 25, BaseSeed: 99, Arms: []Arm{a, b},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := res.Comparisons[0]
	if c.MeanDiff != 0 {
		t.Fatalf("two identical arms differed by %.6f -- state is leaking between "+
			"arms, or the run is not deterministic", c.MeanDiff)
	}
	t.Logf("identical arms: difference exactly %v across %d replicates",
		c.MeanDiff, res.Arms[0].Reach.N)
}

// Near a tipping point the same parameters give wildly different outcomes, and
// a single run reported as a measurement would be a sample from a bimodal
// distribution. The percentiles have to show that spread even when the CI on
// the mean is tight -- they answer different questions.
func TestSpreadIsReportedSeparatelyFromUncertainty(t *testing.T) {
	r := NewRunner()
	res, err := r.Run(context.Background(), Config{
		N: 30_000, Replicates: 14, Rounds: 40, BaseSeed: 7,
		Arms: []Arm{{Label: "near the tipping point", SeedFrac: 0.012,
			Difficulty: 1.0, Seeding: sim.SeedInRegion, Salience: 0.6, Stance: -0.5}},
	})
	if err != nil {
		t.Fatal(err)
	}
	a := res.Arms[0]
	ciWidth := a.Reach.Hi - a.Reach.Lo
	spread := a.Reach.P90 - a.Reach.P10
	t.Logf("reach mean %.1f%%  CI width %.1fpp  p10-p90 spread %.1fpp  tipped %.0f%% of runs",
		a.Reach.Mean*100, ciWidth*100, spread*100, a.Tipped*100)
	if spread < ciWidth {
		t.Fatal("p10-p90 spread is narrower than the CI on the mean, which cannot " +
			"happen with more than a couple of replicates")
	}
	if len(a.Samples) != 14 {
		t.Fatalf("expected 14 samples for inspection, got %d", len(a.Samples))
	}
}

// Paired differences must be computed per replicate. Difference-of-means throws
// away the pairing, so this checks the arithmetic directly on known input.
func TestPairedDifferenceUsesPairs(t *testing.T) {
	// b is always exactly 0.1 above a, but the marginals overlap completely.
	a := []float64{0.1, 0.5, 0.9, 0.3}
	b := []float64{0.2, 0.6, 1.0, 0.4}
	c := pairedDiff("a", "b", a, b)
	if math.Abs(c.MeanDiff-0.1) > 1e-12 {
		t.Fatalf("mean paired difference %.6f, want 0.1", c.MeanDiff)
	}
	if !c.Significant {
		t.Fatal("a difference that is identical in every pair should be significant")
	}
	if c.Hi-c.Lo > 1e-9 {
		t.Fatalf("a constant difference should have a zero-width interval, got [%v,%v]",
			c.Lo, c.Hi)
	}
	t.Logf("constant paired difference: %.3f, CI [%.6f, %.6f]", c.MeanDiff, c.Lo, c.Hi)
}

func TestRunnerRefusesConcurrentExperiments(t *testing.T) {
	r := NewRunner()
	r.running.Store(true)
	if _, err := r.Run(context.Background(), Config{Arms: []Arm{{Label: "x"}}}); err == nil {
		t.Fatal("a second concurrent experiment was accepted")
	}
}

// The normal approximation produced a 95% interval of [-3.8%, +19.3%] on real
// output -- a negative lower bound on a reach that cannot be negative. A
// bootstrap cannot leave the support of the data, so its bounds are always
// outcomes that could actually happen.
func TestIntervalsStayInsideThePossible(t *testing.T) {
	// The shape that broke the normal approximation: bimodal, bounded, mostly
	// near zero with a few worlds that tipped.
	xs := []float64{0.012, 0.013, 0.011, 0.014, 0.012, 0.013, 0.011, 0.99,
		0.012, 0.013, 0.012, 0.985, 0.011, 0.013, 0.012, 0.014}
	s := summarise(xs)

	if s.Lo < 0 {
		t.Fatalf("lower bound %.4f is below zero for a quantity that cannot be", s.Lo)
	}
	if s.Hi > 1 {
		t.Fatalf("upper bound %.4f exceeds one", s.Hi)
	}
	// And the normal approximation really would have failed here, which is what
	// makes this test worth having rather than a tautology.
	se := s.SD / math.Sqrt(float64(len(xs)))
	if s.Mean-1.96*se >= 0 {
		t.Fatalf("this sample no longer breaks the normal approximation "+
			"(would give %.4f); pick a sharper one", s.Mean-1.96*se)
	}
	t.Logf("mean %.3f; bootstrap CI [%.3f, %.3f]; normal approx would have said "+
		"[%.3f, %.3f]", s.Mean, s.Lo, s.Hi, s.Mean-1.96*se, s.Mean+1.96*se)
}

// The same finished result must always report the same interval. A bootstrap
// reseeded per call would drift between two views of one run.
func TestIntervalsAreDeterministic(t *testing.T) {
	xs := []float64{0.1, 0.9, 0.2, 0.85, 0.15, 0.95, 0.11, 0.88}
	a, b := summarise(xs), summarise(xs)
	if a.Lo != b.Lo || a.Hi != b.Hi {
		t.Fatalf("interval moved between calls: [%v,%v] then [%v,%v]", a.Lo, a.Hi, b.Lo, b.Hi)
	}
}
