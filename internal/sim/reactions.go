package sim

import "github.com/sakshamio/Populace/internal/world"

// Applying what the model said.
//
// The model returns, per archetype: where they stand, how much they care, and
// how likely they are to pass it on. Those map onto exactly three things the
// engine already had, and onto nothing else -- the model does not get to set
// reach, or decide who talks to whom, or say how far the story travelled.
// Those are the simulation's answers and the whole point of having one.

// ArchetypeReaction is the subset of a generated reaction the engine consumes.
// Deliberately narrow: package sim does not import package reaction, so the
// dynamics cannot start depending on prose.
type ArchetypeReaction struct {
	Stance   float32
	Salience float32
	Share    float32
}

// ApplyReactions folds per-archetype model output into the population.
//
//   - Stance moves the FJ prior, scaled by Salience. Someone who does not care
//     does not move, which is why salience is asked for separately rather than
//     inferred from a confident-sounding stance.
//   - Share lowers the adoption threshold. An archetype that tells everyone
//     needs less corroboration before acting.
//
// Priors and not opinions: the model says what this kind of person is disposed
// to think, and the network still decides what they end up thinking. Writing
// straight to Y would overwrite the dynamics with the model's guess and make
// the graph decorative.
//
// weight in [0,1] is how far the prior is allowed to move; 1 replaces it. It
// exists so a run can be compared against its own no-model baseline.
func (s *Sim) ApplyReactions(rx map[int]ArchetypeReaction, weight float64) (applied, personas int) {
	if weight <= 0 {
		weight = 1
	}
	if weight > 1 {
		weight = 1
	}
	// Per-worker counters rather than a shared one: incrementing a captured
	// int from every goroutine is a data race that produces a plausible-looking
	// undercount, which is the worst kind because nothing ever looks wrong.
	counts := make([]int, numWorkers(s.W.N))
	parallelIdx(s.W.N, func(chunk, lo, hi int) {
		n := 0
		for i := lo; i < hi; i++ {
			r, ok := rx[int(s.W.Arch[i])]
			if !ok {
				continue
			}
			// Salience gates the move. A reaction of "strongly against, and I
			// have never thought about this" should barely register, and
			// without this gate it would register as strongly as anything.
			k := float32(weight) * r.Salience
			s.O.Prior[i] += (r.Stance - s.O.Prior[i]) * k

			// Media are pinned (lambda 0), so their prior *is* what they
			// broadcast. Letting the model set it per archetype is strictly
			// better than the single global stance on the Event: a tabloid and
			// a documentary strand do not carry the same story the same way.
			if world.Stratum(s.W.Strat[i]) == world.Media {
				s.O.Y[i] = s.O.Prior[i]
			}

			// Eagerness to pass something on lowers the bar for acting on it,
			// but never below the reinforcement floor -- a complex contagion
			// that can be triggered by one enthusiastic neighbour is a simple
			// contagion wearing a threshold.
			t := s.C.Threshold[i] * (1 - 0.55*r.Share)
			if t < 0.01 {
				t = 0.01
			}
			s.C.Threshold[i] = t
			n++
		}
		counts[chunk] = n
	})

	for _, n := range counts {
		personas += n
	}
	s.resync()
	return len(rx), personas
}
