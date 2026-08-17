package sim

import (
	"math"
	"testing"
)

func TestDayNightIsOffByDefault(t *testing.T) {
	// Every existing test and every other caller of DefaultConfig has to keep
	// getting the old always-awake behaviour -- this is the guard for that.
	cfg := DefaultConfig()
	if cfg.DayNight.enabled() {
		t.Fatal("DefaultConfig must not enable day/night; the server opts in explicitly")
	}
	w := testWorld(t, testN)
	s := New(w, cfg)
	if s.DayNight.enabled() {
		t.Fatal("Sim.DayNight should be disabled when Config.DayNight is the zero value")
	}
}

func TestActivityCurvePeaksAtDayAndTroughsAtNight(t *testing.T) {
	d := DefaultDayNightConfig()
	peak := d.activityAt(14)
	trough := d.activityAt(2)
	if peak <= trough {
		t.Fatalf("expected 14:00 (%.3f) busier than 02:00 (%.3f)", peak, trough)
	}
	if math.Abs(peak-d.Ceiling) > 1e-9 {
		t.Errorf("14:00 should be exactly the ceiling, got %.4f want %.4f", peak, d.Ceiling)
	}
	if math.Abs(trough-d.Floor) > 1e-9 {
		t.Errorf("02:00 should be exactly the floor, got %.4f want %.4f", trough, d.Floor)
	}
	// Never truly zero -- someone is always awake somewhere.
	for h := 0.0; h < 24; h += 0.5 {
		if a := d.activityAt(h); a < d.Floor-1e-9 || a > d.Ceiling+1e-9 {
			t.Fatalf("activityAt(%.1f) = %.4f, outside [floor, ceiling]", h, a)
		}
	}
}

func TestLocalHourFollowsLongitude(t *testing.T) {
	// East is later in the day; 180 degrees is exactly 12 hours offset.
	cases := []struct {
		lonRad float32
		want   float64
	}{
		{0, 12},
		{float32(math.Pi / 2), 18}, // 90E, +6h
		{float32(-math.Pi / 2), 6}, // 90W, -6h
		{float32(math.Pi), 0},      // 180E wraps to midnight
		{float32(-math.Pi), 0},     // 180W, same instant as 180E
		{float32(2 * math.Pi), 12}, // a full turn changes nothing
	}
	for _, c := range cases {
		got := localHour(12, c.lonRad)
		// Circular distance: 0 and 24 are the same instant, which is exactly
		// what the +/-180 degree case below is testing -- east and west both
		// reach midnight, and float32(-Pi) landing a hair either side of that
		// boundary is not a bug in localHour, it's the ambiguity the
		// antimeridian always has.
		d := math.Mod(got-c.want, 24)
		if d < -12 {
			d += 24
		}
		if d > 12 {
			d -= 24
		}
		if math.Abs(d) > 1e-4 {
			t.Errorf("localHour(12, %.4f rad) = %.4f, want %.4f", c.lonRad, got, c.want)
		}
	}
}

func TestEventHourSetsTheClockOnlyWhenDayNightIsOn(t *testing.T) {
	w := testWorld(t, testN)

	// Disabled: HasHour must be a no-op, since it means nothing without a clock.
	sOff := New(w, DefaultConfig())
	before := sOff.HourUTC
	sOff.Inject(Event{Stance: -0.5, Salience: 0.5, Seeding: SeedInRegion,
		SeedSize: 300, Seed: 1, HasHour: true, Hour: 3})
	if sOff.HourUTC != before {
		t.Errorf("HourUTC changed with day/night disabled: %.2f -> %.2f", before, sOff.HourUTC)
	}

	// Enabled: the clock must snap to exactly the requested hour.
	cfg := DefaultConfig()
	cfg.DayNight = DefaultDayNightConfig()
	sOn := New(w, cfg)
	sOn.Inject(Event{Stance: -0.5, Salience: 0.5, Seeding: SeedInRegion,
		SeedSize: 300, Seed: 1, HasHour: true, Hour: 3})
	if sOn.HourUTC != 3 {
		t.Errorf("HourUTC = %.2f after HasHour:true, Hour:3, want 3", sOn.HourUTC)
	}

	// A negative or >24 hour wraps rather than producing a nonsense clock.
	sOn.Inject(Event{Stance: -0.5, Salience: 0.5, Seeding: SeedInRegion,
		SeedSize: 300, Seed: 2, HasHour: true, Hour: 27})
	if sOn.HourUTC != 3 {
		t.Errorf("Hour:27 should wrap to 3, got %.2f", sOn.HourUTC)
	}
}

func TestResetRestoresTheClockToStartHour(t *testing.T) {
	w := testWorld(t, testN)
	cfg := DefaultConfig()
	cfg.DayNight = DefaultDayNightConfig()
	s := New(w, cfg)

	s.Inject(Event{Stance: -0.5, Salience: 0.5, Seeding: SeedInRegion,
		SeedSize: 300, Seed: 1, HasHour: true, Hour: 22})
	for i := 0; i < 10; i++ {
		s.Advance()
	}
	if s.HourUTC == cfg.DayNight.StartHourUTC {
		t.Skip("clock happened to land back on the start hour, not a useful check")
	}
	s.Reset(cfg)
	if s.HourUTC != cfg.DayNight.StartHourUTC {
		t.Errorf("HourUTC after Reset = %.2f, want StartHourUTC %.2f",
			s.HourUTC, cfg.DayNight.StartHourUTC)
	}
}

func TestDayNightGateIsDeterministic(t *testing.T) {
	run := func() *Snapshot {
		w := testWorld(t, testN)
		cfg := DefaultConfig()
		cfg.DayNight = DefaultDayNightConfig()
		s := New(w, cfg)
		s.Inject(Event{Stance: -0.6, Salience: 0.7, Seeding: SeedInRegion,
			SeedSize: testN / 20, Seed: 7, HasHour: true, Hour: 2, Difficulty: 0.8})
		for i := 0; i < 40; i++ {
			s.Advance()
		}
		sn := s.Snapshot(false)
		return &sn
	}
	a, b := run(), run()
	if a.Reach != b.Reach || a.MeanOpinion != b.MeanOpinion {
		t.Fatalf("same seed gave different runs: reach %.6f vs %.6f, opinion %.6f vs %.6f",
			a.Reach, b.Reach, a.MeanOpinion, b.MeanOpinion)
	}
}

// This is the actual claim the feature exists to test: the same story,
// seeded identically, spreads slower when it breaks into its own region's
// deep night than when it breaks at that region's peak activity hour --
// because a large share of the seed set's own neighbours are asleep and do
// not evaluate their exposure for several ticks after the story lands.
func TestBreakingAtNightSlowsTheEarlyCascade(t *testing.T) {
	w := testWorld(t, testN)
	cfg := DefaultConfig()
	cfg.DayNight = DefaultDayNightConfig()

	reachAfter := func(hour float64, ticks int) float64 {
		s := New(w, cfg)
		// Seed at the world's own prime meridian region so hour 2/14 UTC is
		// close to true local time there, and hold everything else fixed.
		s.Inject(Event{Stance: -0.6, Salience: 0.7, Seeding: SeedInRegion,
			SeedSize: testN / 20, Seed: 11, HasHour: true, Hour: hour, Difficulty: 0.8})
		for i := 0; i < ticks; i++ {
			s.Advance()
		}
		return s.Snapshot(false).Reach
	}

	const earlyTicks = 12 // early enough that the seed region has not woken up yet at night
	night := reachAfter(2, earlyTicks)
	day := reachAfter(14, earlyTicks)
	t.Logf("reach after %d ticks: breaking at 02:00 UTC = %.4f, at 14:00 UTC = %.4f",
		earlyTicks, night, day)
	if night > day {
		t.Errorf("breaking at night reached more people early than breaking at day "+
			"(%.4f > %.4f) -- the activity gate is not doing what it claims to", night, day)
	}

	// And it is not a permanent block: given enough ticks for the world to
	// turn, the night-break case still gets somewhere.
	s := New(w, cfg)
	s.Inject(Event{Stance: -0.6, Salience: 0.7, Seeding: SeedInRegion,
		SeedSize: testN / 20, Seed: 11, HasHour: true, Hour: 2, Difficulty: 0.8})
	for i := 0; i < 200; i++ {
		s.Advance()
	}
	if r := s.Snapshot(false).Reach; r <= 0 {
		t.Errorf("a story broken at night never went anywhere even after 200 ticks (reach %.4f)", r)
	}
}
