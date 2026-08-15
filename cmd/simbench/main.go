// Command simbench reproduces the scale numbers quoted in the README.
//
// Kept in the tree rather than run once and written down, because a
// performance claim nobody can re-run is a performance claim that quietly
// stops being true.
package main

import (
	"fmt"
	"runtime"
	"time"

	"github.com/sakshamio/Populace/internal/sim"
	"github.com/sakshamio/Populace/internal/world"
)

func mem() float64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return float64(m.HeapAlloc) / 1e9
}

// countingWriter measures a frame without materialising it.
type countingWriter int

func (c *countingWriter) Write(p []byte) (int, error) {
	*c += countingWriter(len(p))
	return len(p), nil
}

func main() {
	for _, n := range []int{1_000_000, 10_000_000} {
		t0 := time.Now()
		w := world.Generate(n, 20260815)
		tw := time.Since(t0)
		mw := mem()

		t0 = time.Now()
		s := sim.New(w, sim.DefaultConfig())
		ts := time.Since(t0)
		ms := mem()

		st := s.G.DegreeStats(w)
		// One percent of the population, not a fixed count: reach tracks the
		// seeded fraction, so a fixed count would measure something different
		// at each scale. See TestReachDependsOnSeededFractionNotCount.
		s.Inject(sim.Event{Stance: -0.8, Salience: 0.7,
			Seeding: sim.SeedInRegion, SeedSize: n / 100, Seed: 42})

		t0 = time.Now()
		const ticks = 20
		for i := 0; i < ticks; i++ {
			s.Advance()
		}
		tt := time.Since(t0) / ticks

		var frame countingWriter
		_, full, _ := s.Encode(&frame)

		snap := s.Snapshot(false)
		fmt.Printf("n=%-11d population %6s  graph+state %7s  tick %7s\n",
			n, tw.Round(time.Millisecond), ts.Round(time.Millisecond), tt.Round(time.Microsecond))
		fmt.Printf("            heap %.2f GB after population, %.2f GB with graph (%d edges, mean deg %.1f, max %d)\n",
			mw, ms, s.G.Edges, st.Mean, st.Max)
		form := "delta"
		if full {
			form = "full"
		}
		fmt.Printf("            after 20 ticks: reach %.3f%% of a %d-seed story, mean opinion %.4f\n",
			snap.Reach*100, n/100, snap.MeanOpinion)
		fmt.Printf("            state frame: %s, %.2f MB (a full resend is always %.2f MB)\n\n",
			form, float64(frame)/1e6, float64(n*2+12)/1e6)

		w, s = nil, nil
		runtime.GC()
	}
}
