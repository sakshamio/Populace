package sim

import "math"

// dayNightActivity computes this tick's per-persona activity multiplier,
// read by AdvanceWith to decide who evaluates their exposure this tick. O(N)
// and parallel, the same cost class as packFlags -- called once per tick,
// only when DayNight.enabled().
func (s *Sim) dayNightActivity() []float64 {
	out := make([]float64, s.W.N)
	hourUTC := s.HourUTC
	dn := s.DayNight
	parallel(s.W.N, func(lo, hi int) {
		for i := lo; i < hi; i++ {
			out[i] = dn.activityAt(localHour(hourUTC, s.W.Lon[i]))
		}
	})
	return out
}

// A day/night rhythm: whether a person can notice and act on what their
// feed or their friends are showing them depends on their own local time,
// derived from longitude the same way a real timezone does.
//
// Nothing about *exposure* changes -- an adopted neighbour is still visible
// at any hour, the same way a friend's post from yesterday is still sitting
// in a feed whenever it gets checked. What changes is whether a given
// person's threshold check runs on a given tick at all: someone asleep at
// 3am local does not evaluate what their friends are doing until they wake
// up, even if they would have cleared their threshold instantly. This is
// what makes "the same story, broken at a different hour" a different
// cascade rather than the same cascade shifted in time -- a story breaking
// into a region's night has to survive on a much smaller awake fraction of
// its own seed set until other timezones wake into it.
//
// Disabled by default (MinutesPerTick == 0) is exactly the old behaviour:
// everyone evaluates every tick, at any hour. See AdvanceWith's activity
// parameter for where this plugs into contagion, and DefaultConfig for why
// it is on by default at the Sim level.
type DayNightConfig struct {
	// MinutesPerTick converts one simulation tick into simulated wall-clock
	// time. Zero disables the whole mechanic -- every call site that reads
	// this treats a zero config as "uniform activity, ignore the clock".
	MinutesPerTick float64

	// StartHourUTC is the clock's reading at Tick 0, in [0, 24).
	StartHourUTC float64

	// Floor is the activity multiplier at the dead of night (local ~2am) --
	// never zero, because this model has no evidence anyone's probability of
	// noticing news is truly zero at any hour, only lower. Ceiling is the
	// multiplier at peak daytime activity.
	Floor, Ceiling float64
}

func DefaultDayNightConfig() DayNightConfig {
	return DayNightConfig{
		MinutesPerTick: 10, // a simulated day is 144 ticks
		StartHourUTC:   9,  // a workday morning in Europe, evening in East Asia
		Floor:          0.12,
		Ceiling:        1.0,
	}
}

func (d DayNightConfig) enabled() bool { return d.MinutesPerTick > 0 }

func wrapHour(h float64) float64 { return math.Mod(math.Mod(h, 24)+24, 24) }

// localHour converts a UTC hour to this longitude's local hour. lonRad
// follows World.Lon's own convention: radians, east positive. 24 hours per
// full turn, the same relationship that puts 15 degrees to a timezone.
func localHour(hourUTC float64, lonRad float32) float64 {
	return wrapHour(hourUTC + float64(lonRad)*12/math.Pi)
}

// activity is a single-peaked diurnal curve, not the real bimodal
// morning/evening-commute shape -- a smooth stand-in in the same spirit as
// place.go's Gaussian centres: calibrated for "clearly quieter at 3am than
// at 3pm", not for matching any specific city's real traffic. Peaks at
// 14:00 local, troughs at 02:00.
func (d DayNightConfig) activityAt(localHr float64) float64 {
	t := 0.5 + 0.5*math.Cos(2*math.Pi*(localHr-14)/24)
	return d.Floor + (d.Ceiling-d.Floor)*t
}
