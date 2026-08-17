// Package experiment runs the simulation the way a result is actually
// defended: many replicates, several arms, and an interval rather than a
// number.
//
// One run of a cascade model tells you almost nothing. Threshold dynamics sit
// near a tipping point, and near a tipping point the same parameters produce
// 1% reach and 99% reach depending on which people happened to be seeded. A
// single number from a single run is a sample from a bimodal distribution
// being reported as a measurement.
//
// Two design choices follow, and both are what make the output quotable:
//
//   - Arms are PAIRED. Every arm runs on the same world and the same social
//     graph within a replicate, so the difference between arms is not
//     contaminated by the difference between worlds. Paired differences have
//     far lower variance than independent ones, which is why a 20-replicate
//     paired design detects effects an unpaired 200-replicate design misses.
//
//   - Replicates vary the WORLD seed, not just the cascade seed. Re-rolling
//     only the seed set answers "how much does this depend on who I seeded";
//     re-rolling the world answers "how much does this depend on the world I
//     invented", which is the question a reader will actually ask.
package experiment

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sakshamio/Populace/internal/sim"
	"github.com/sakshamio/Populace/internal/world"
)

// Arm is one condition being compared. Everything not set here is held fixed
// across arms by construction -- there is no way to vary the population, the
// graph, or the topic between arms, because those are what pairing controls.
type Arm struct {
	Label string `json:"label"`

	// SeedFrac is the seeded share of the population. A fraction rather than a
	// count because reach tracks the fraction; see the phase-3 result.
	SeedFrac float64 `json:"seed_frac"`

	Difficulty float64     `json:"difficulty"`
	Seeding    sim.Seeding `json:"seeding"`
	Salience   float64     `json:"salience"`
	Stance     float64     `json:"stance"`

	// Hour sets the simulated UTC clock the moment this arm's story breaks --
	// "the same story, but at 3am" as an arm rather than a manual one-off.
	// Only meaningful when Config.DayNight is set; ignored otherwise, same as
	// sim.Event.HasHour.
	HasHour bool    `json:"has_hour"`
	Hour    float64 `json:"hour"`

	// Reactions applies cached model output. Nil is the no-model counterfactual,
	// which is the baseline any claim about the model's contribution needs.
	Reactions map[int]sim.ArchetypeReaction `json:"-"`
}

type Config struct {
	N          int    `json:"n"`          // population per replicate
	Replicates int    `json:"replicates"` // independent worlds
	Rounds     int    `json:"rounds"`     // simulation ticks per run
	BaseSeed   uint64 `json:"base_seed"`
	Arms       []Arm  `json:"arms"`

	// DayNight turns on the diurnal activity rhythm for every arm in this
	// experiment. Off by default, like sim.DefaultConfig itself -- an
	// experiment that never mentions timing should not have its numbers
	// quietly depend on what hour the arms happened to run.
	DayNight bool `json:"daynight"`
}

// Stats is a sample summary with a 95% confidence interval on the MEAN.
//
// The interval is on the mean and not on the observations, and the difference
// matters here more than usual: near a tipping point the observations span the
// whole range while the mean is estimated tightly, and quoting the spread of
// observations as uncertainty about the effect would be wrong in both
// directions at once. P10 and P90 report the spread separately so both are
// visible.
type Stats struct {
	Mean float64 `json:"mean"`
	SD   float64 `json:"sd"`
	Lo   float64 `json:"ci_lo"`
	Hi   float64 `json:"ci_hi"`
	P10  float64 `json:"p10"`
	P50  float64 `json:"p50"`
	P90  float64 `json:"p90"`
	N    int     `json:"n"`
}

type ArmResult struct {
	Arm      Arm       `json:"arm"`
	Reach    Stats     `json:"reach"`
	Shift    Stats     `json:"shift"`
	PeakRate Stats     `json:"peak_rate"`
	Tipped   float64   `json:"tipped"` // share of replicates that exceeded 50% reach
	Samples  []float64 `json:"samples"`
}

// Comparison is a paired difference against the first arm. Paired, so it is
// computed per replicate and then summarised, never as a difference of means.
type Comparison struct {
	Against     string  `json:"against"`
	Label       string  `json:"label"`
	MeanDiff    float64 `json:"mean_diff"`
	Lo          float64 `json:"ci_lo"`
	Hi          float64 `json:"ci_hi"`
	Significant bool    `json:"significant"` // CI excludes zero
}

type Result struct {
	Config      Config       `json:"config"`
	Arms        []ArmResult  `json:"arms"`
	Comparisons []Comparison `json:"comparisons"`
	ElapsedS    float64      `json:"elapsed_s"`
	Runs        int          `json:"runs"`
}

// Progress is what a long experiment owes a UI watching it.
type Progress struct {
	Running    bool    `json:"running"`
	Replicate  int     `json:"replicate"`
	Replicates int     `json:"replicates"`
	Runs       int     `json:"runs"`
	TotalRuns  int     `json:"total_runs"`
	ElapsedS   float64 `json:"elapsed_s"`
	ETASec     float64 `json:"eta_s"`
	Note       string  `json:"note"`
}

// Runner owns one experiment at a time. One at a time on purpose: these are
// CPU-bound over every core, and two concurrent experiments would take twice
// as long each while making both sets of timings meaningless.
type Runner struct {
	running   atomic.Bool
	replicate atomic.Int64
	runs      atomic.Int64
	totalRuns atomic.Int64
	reps      atomic.Int64
	startedAt atomic.Int64
	note      atomic.Value // string

	mu   sync.RWMutex
	last *Result
}

func NewRunner() *Runner {
	r := &Runner{}
	r.note.Store("")
	return r
}

func (r *Runner) Progress() Progress {
	p := Progress{
		Running:   r.running.Load(),
		Replicate: int(r.replicate.Load()), Replicates: int(r.reps.Load()),
		Runs: int(r.runs.Load()), TotalRuns: int(r.totalRuns.Load()),
		Note: r.note.Load().(string),
	}
	if s := r.startedAt.Load(); s > 0 && p.Running {
		p.ElapsedS = time.Since(time.Unix(0, s)).Seconds()
		if p.Runs > 0 && p.TotalRuns > p.Runs {
			p.ETASec = p.ElapsedS / float64(p.Runs) * float64(p.TotalRuns-p.Runs)
		}
	}
	return p
}

func (r *Runner) Last() *Result {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.last
}

// Run executes the whole design and returns once every replicate is done.
func (r *Runner) Run(ctx context.Context, cfg Config) (*Result, error) {
	if len(cfg.Arms) == 0 {
		return nil, fmt.Errorf("no arms to compare")
	}
	if cfg.N <= 0 {
		cfg.N = 60_000
	}
	if cfg.Replicates <= 0 {
		cfg.Replicates = 12
	}
	if cfg.Rounds <= 0 {
		cfg.Rounds = 40
	}
	if !r.running.CompareAndSwap(false, true) {
		return nil, fmt.Errorf("an experiment is already running")
	}
	defer r.running.Store(false)

	r.reps.Store(int64(cfg.Replicates))
	r.totalRuns.Store(int64(cfg.Replicates * len(cfg.Arms)))
	r.runs.Store(0)
	r.replicate.Store(0)
	r.startedAt.Store(time.Now().UnixNano())
	r.note.Store("building the first world")

	start := time.Now()
	// reach[arm][replicate] -- laid out arm-major so the paired difference for
	// a replicate is a column, which is the shape the summary needs.
	reach := make([][]float64, len(cfg.Arms))
	shift := make([][]float64, len(cfg.Arms))
	peak := make([][]float64, len(cfg.Arms))
	for i := range reach {
		reach[i] = make([]float64, 0, cfg.Replicates)
		shift[i] = make([]float64, 0, cfg.Replicates)
		peak[i] = make([]float64, 0, cfg.Replicates)
	}

	simCfg := sim.DefaultConfig()
	if cfg.DayNight {
		simCfg.DayNight = sim.DefaultDayNightConfig()
	}
	for rep := 0; rep < cfg.Replicates; rep++ {
		if ctx.Err() != nil {
			break
		}
		r.replicate.Store(int64(rep + 1))
		r.note.Store(fmt.Sprintf("replicate %d of %d: building world and graph",
			rep+1, cfg.Replicates))

		// One world and one graph per replicate, shared by every arm. This is
		// the pairing, and it is also most of the cost -- building the graph
		// dominates, so sharing it makes the design nearly free per extra arm.
		w := world.Generate(cfg.N, cfg.BaseSeed+uint64(rep)*0x9E3779B9)
		s := sim.New(w, simCfg)

		for a := range cfg.Arms {
			if ctx.Err() != nil {
				break
			}
			arm := cfg.Arms[a]
			r.note.Store(fmt.Sprintf("replicate %d/%d, arm %q",
				rep+1, cfg.Replicates, arm.Label))

			// Reset restores the population to its pre-story state, including
			// the thresholds ApplyReactions moves. Without that the arms would
			// be run on a population the previous arm modified.
			s.Reset(simCfg)

			size := int(float64(cfg.N) * arm.SeedFrac)
			if size < 1 {
				size = 1
			}
			s.Inject(sim.Event{
				Stance: float32(arm.Stance), Salience: arm.Salience,
				Seeding: arm.Seeding, SeedSize: size,
				// Seed varies with the replicate so the seed set moves with the
				// world, and with nothing else so the arms stay paired.
				Seed:       cfg.BaseSeed + uint64(rep)*0x9E3779B9,
				Difficulty: arm.Difficulty,
				HasHour:    arm.HasHour, Hour: arm.Hour,
			})
			if arm.Reactions != nil {
				s.ApplyReactions(arm.Reactions, 1)
			}

			maxRate := 0.0
			for t := 0; t < cfg.Rounds; t++ {
				n, _ := s.Advance()
				if rate := float64(n) / float64(cfg.N); rate > maxRate {
					maxRate = rate
				}
			}
			snap := s.Snapshot(false)
			reach[a] = append(reach[a], snap.Reach)
			shift[a] = append(shift[a], snap.OpinionShift)
			peak[a] = append(peak[a], maxRate)
			r.runs.Add(1)
		}
	}

	res := &Result{Config: cfg, ElapsedS: time.Since(start).Seconds(),
		Runs: int(r.runs.Load())}
	for a, arm := range cfg.Arms {
		tipped := 0.0
		for _, v := range reach[a] {
			if v > 0.5 {
				tipped++
			}
		}
		if len(reach[a]) > 0 {
			tipped /= float64(len(reach[a]))
		}
		res.Arms = append(res.Arms, ArmResult{
			Arm: arm, Reach: summarise(reach[a]), Shift: summarise(shift[a]),
			PeakRate: summarise(peak[a]), Tipped: tipped, Samples: reach[a],
		})
	}
	for a := 1; a < len(cfg.Arms); a++ {
		res.Comparisons = append(res.Comparisons,
			pairedDiff(cfg.Arms[0].Label, cfg.Arms[a].Label, reach[0], reach[a]))
	}

	r.mu.Lock()
	r.last = res
	r.mu.Unlock()
	r.note.Store("done")
	return res, ctx.Err()
}

// summarise computes mean, SD, a 95% CI on the mean, and percentiles.
//
// The interval is a PERCENTILE BOOTSTRAP, not mean +/- 1.96 SE. The normal
// approximation was tried first and produced, on real output, a 95% interval
// of [-3.8%, +19.3%] for a reach that cannot be negative. That is not a
// rounding artefact: reach here is bounded in [0,1] and strongly bimodal --
// most worlds either fizzle near 1% or tip near 99% -- so the sampling
// distribution of its mean is skewed and nothing symmetric around the mean can
// describe it. An impossible bound on a dashboard is worse than a wide one,
// because it tells the reader the arithmetic is not to be trusted anywhere.
//
// The bootstrap makes no distributional assumption and cannot leave the
// support of the data, so its bounds are always attainable outcomes.
func summarise(xs []float64) Stats {
	s := Stats{N: len(xs)}
	if len(xs) == 0 {
		return s
	}
	for _, v := range xs {
		s.Mean += v
	}
	s.Mean /= float64(len(xs))

	if len(xs) > 1 {
		for _, v := range xs {
			d := v - s.Mean
			s.SD += d * d
		}
		s.SD = math.Sqrt(s.SD / float64(len(xs)-1))
		s.Lo, s.Hi = bootstrapCI(xs)
	} else {
		s.Lo, s.Hi = s.Mean, s.Mean
	}

	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	s.P10 = quantile(sorted, 0.10)
	s.P50 = quantile(sorted, 0.50)
	s.P90 = quantile(sorted, 0.90)
	return s
}

// bootstrapCI is a 2.5/97.5 percentile bootstrap of the mean.
//
// Deterministic: seeded from the sample itself, so the same data always yields
// the same interval. An interval that moved between two views of one finished
// result would undermine the whole point of reporting one.
func bootstrapCI(xs []float64) (lo, hi float64) {
	const B = 4000
	n := len(xs)
	means := make([]float64, B)

	// splitmix64 seeded off the sample size and its first value.
	st := uint64(n)*0x9E3779B97F4A7C15 + math.Float64bits(xs[0])
	next := func() uint64 {
		st += 0x9E3779B97F4A7C15
		z := st
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		return z ^ (z >> 31)
	}

	for b := 0; b < B; b++ {
		sum := 0.0
		for i := 0; i < n; i++ {
			sum += xs[next()%uint64(n)]
		}
		means[b] = sum / float64(n)
	}
	sort.Float64s(means)
	return quantile(means, 0.025), quantile(means, 0.975)
}

func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(q * float64(len(sorted)-1))
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

// pairedDiff summarises the per-replicate difference between two arms.
//
// Per-replicate, then summarised -- never mean(b) - mean(a). The difference of
// means throws away exactly the information pairing was set up to capture: two
// arms can differ reliably in every single world while their marginal
// distributions overlap almost completely.
func pairedDiff(against, label string, a, b []float64) Comparison {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	diffs := make([]float64, n)
	for i := 0; i < n; i++ {
		diffs[i] = b[i] - a[i]
	}
	st := summarise(diffs)
	return Comparison{
		Against: against, Label: label, MeanDiff: st.Mean,
		Lo: st.Lo, Hi: st.Hi,
		Significant: n > 1 && (st.Lo > 0 || st.Hi < 0),
	}
}
