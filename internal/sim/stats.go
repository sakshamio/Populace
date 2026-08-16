package sim

import "github.com/sakshamio/Populace/internal/world"

// Observability.
//
// The aggregates here are computed on demand rather than maintained
// incrementally. That is a deliberate trade: a full weighted scan of ten
// million people costs ~20 ms, which is affordable once a second and is not
// affordable per tick -- but an incremental counter set updated inside the tick
// loop would be a second representation of the same state, and the failure mode
// of a drifting counter is a dashboard that confidently reports a number the
// simulation does not hold. One source of truth, scanned when asked.

// GroupStat is one row of a breakdown: how big the group is, how far the story
// got into it, and how far its opinion moved.
type GroupStat struct {
	Name     string  `json:"name"`
	Share    float64 `json:"share"`     // weighted share of the population
	Reach    float64 `json:"reach"`     // weighted share of the group that adopted
	Opinion  float64 `json:"opinion"`   // weighted mean opinion within the group
	Shift    float64 `json:"shift"`     // change since the story broke
	Personas int     `json:"personas"`  // raw count, for sampling confidence
	Degree   float64 `json:"mean_ties"` // mean degree, which explains most reach differences
}

// Breakdown is the set of cuts the UI shows. Region and stratum rather than
// archetype: 450 archetype rows is a data dump, and the two questions people
// actually ask of a result are "where" and "who".
type Breakdown struct {
	Regions []GroupStat `json:"regions"`
	Strata  []GroupStat `json:"strata"`
}

// baseline holds the per-group opinion at the moment the story broke, so shift
// is attributable. Same reasoning as Sim.baseOpinion: the level is mostly what
// the population already believed.
type baseline struct {
	region  [world.NumRegions]float64
	stratum [world.NumStrata]float64
	set     bool
}

func (s *Sim) Breakdown() Breakdown {
	var (
		regW, regAdopt, regOp, regDeg [world.NumRegions]float64
		regN                          [world.NumRegions]int
		strW, strAdopt, strOp, strDeg [world.NumStrata]float64
		strN                          [world.NumStrata]int
		totalW                        float64
	)

	for i := 0; i < s.W.N; i++ {
		wt := float64(s.W.Weight[i])
		totalW += wt

		reg := world.Archetype(s.W.Arch[i]).Region()
		st := s.W.Strat[i]
		y := float64(s.O.Y[i])
		deg := float64(s.G.Degree(i))
		adopted := 0.0
		if s.C.State[i] != Unaware {
			adopted = wt
		}

		regW[reg] += wt
		regAdopt[reg] += adopted
		regOp[reg] += wt * y
		regDeg[reg] += deg
		regN[reg]++

		strW[st] += wt
		strAdopt[st] += adopted
		strOp[st] += wt * y
		strDeg[st] += deg
		strN[st]++
	}

	b := Breakdown{}
	for r := world.Region(0); r < world.NumRegions; r++ {
		if regW[r] == 0 {
			continue
		}
		op := regOp[r] / regW[r]
		b.Regions = append(b.Regions, GroupStat{
			Name: world.Regions[r].Name, Share: regW[r] / totalW,
			Reach: regAdopt[r] / regW[r], Opinion: op,
			Shift: op - s.base.region[r], Personas: regN[r],
			Degree: regDeg[r] / float64(regN[r]),
		})
	}
	for st := world.Stratum(0); st < world.NumStrata; st++ {
		if strW[st] == 0 {
			continue
		}
		op := strOp[st] / strW[st]
		b.Strata = append(b.Strata, GroupStat{
			Name: world.Strata[st].Name, Share: strW[st] / totalW,
			Reach: strAdopt[st] / strW[st], Opinion: op,
			Shift: op - s.base.stratum[st], Personas: strN[st],
			Degree: strDeg[st] / float64(strN[st]),
		})
	}
	return b
}

// captureBaseline records per-group opinion at the moment a story breaks.
func (s *Sim) captureBaseline() {
	var regW, regOp [world.NumRegions]float64
	var strW, strOp [world.NumStrata]float64
	for i := 0; i < s.W.N; i++ {
		wt := float64(s.W.Weight[i])
		y := float64(s.O.Y[i])
		reg := world.Archetype(s.W.Arch[i]).Region()
		st := s.W.Strat[i]
		regW[reg] += wt
		regOp[reg] += wt * y
		strW[st] += wt
		strOp[st] += wt * y
	}
	for r := range regW {
		if regW[r] > 0 {
			s.base.region[r] = regOp[r] / regW[r]
		}
	}
	for st := range strW {
		if strW[st] > 0 {
			s.base.stratum[st] = strOp[st] / strW[st]
		}
	}
	s.base.set = true
}

// Sample is one tick's worth of history. Small on purpose: this is retained for
// the whole run and sent to the browser on every reload, so it holds the four
// series worth plotting and nothing else.
type Sample struct {
	Tick        int     `json:"tick"`
	Reach       float64 `json:"reach"`
	NewAdopters int     `json:"new"`
	Shift       float64 `json:"shift"`
	TickMS      float64 `json:"tick_ms"`
}

// History is a ring, because a run left open overnight should not grow without
// bound. 1800 samples at the default 400 ms tick is twelve minutes, which is
// far longer than any cascade takes to resolve.
const historyLen = 1800

func (s *Sim) record(newAdopters int, tickMS float64) {
	sm := Sample{
		Tick: s.Tick, NewAdopters: newAdopters,
		Reach: s.C.Reach(s.W), Shift: s.O.Mean(s.W) - s.baseOpinion,
		TickMS: tickMS,
	}
	if len(s.history) < historyLen {
		s.history = append(s.history, sm)
		return
	}
	s.history[s.histHead] = sm
	s.histHead = (s.histHead + 1) % historyLen
}

// History returns the samples oldest-first.
func (s *Sim) History() []Sample {
	if len(s.history) < historyLen {
		out := make([]Sample, len(s.history))
		copy(out, s.history)
		return out
	}
	out := make([]Sample, 0, historyLen)
	out = append(out, s.history[s.histHead:]...)
	out = append(out, s.history[:s.histHead]...)
	return out
}

// ResetHistory clears the series when a new story starts, so the plot is of
// this cascade and not of every cascade that ever ran.
func (s *Sim) ResetHistory() {
	s.history = s.history[:0]
	s.histHead = 0
}
