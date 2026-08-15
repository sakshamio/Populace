// Package world holds the simulated population.
//
// The layout here is struct-of-arrays rather than []*Persona, and that is a
// load-bearing decision rather than a style preference. Ten million
// pointer-bearing structs give the garbage collector ten million objects to
// trace every cycle, and turn the tick loop into a pointer-chasing cache-miss
// generator. SoA gives the GC a handful of objects to scan and lets the
// numeric kernels stream linearly through memory.
//
// Decide this before writing the tick loop, not after profiling one.
package world

import (
	"encoding/binary"
	"io"
	"math"
)

// InstanceBytes is the on-wire and in-GPU size of one persona's render record.
//
// The wire format IS the GPU vertex layout. There is deliberately no
// marshalling step: the server writes these bytes, the browser hands the
// ArrayBuffer straight to gl.bufferData, and nothing parses anything. JSON
// would be roughly 100 bytes per persona and seconds of parse time at ten
// million; this is 16 bytes and zero.
const InstanceBytes = 16

// Stratum is a sampling stratum, not a demographic label.
//
// Sampling ten million people in proportion to world population yields zero
// celebrities -- there are perhaps ten thousand globally, about 1.25 per
// million -- and a statistically invisible number of high-net-worth
// individuals. That matters beyond flavour: influence is not proportional to
// population, and if broadcast nodes are absent the degree distribution's tail
// is wrong and every cascade result with it.
//
// So rare-but-influential strata are deliberately oversampled and carry an
// importance weight, exactly as a survey does. Every aggregate must be
// weighted; see World.WeightedMean.
type Stratum uint8

const (
	General Stratum = iota
	Nomadic
	Immigrant
	HighNetWorth
	Media
	NumStrata
)

// StratumSpec records the real-world share and the share we actually sample,
// from which the importance weight follows.
type StratumSpec struct {
	Name       string
	RealShare  float64 // fraction of the actual world population
	SampleFrac float64 // fraction of our sampled population
}

// Shares are order-of-magnitude figures, good enough to size the strata and
// explicit enough to argue with. Replace with sourced values before any
// published result depends on them.
var Strata = [NumStrata]StratumSpec{
	General:      {"general", 0.99400, 0.920},
	Nomadic:      {"nomadic", 0.00450, 0.020},
	Immigrant:    {"immigrant", 0.03600, 0.050},
	HighNetWorth: {"high_net_worth", 0.01100, 0.030},
	Media:        {"media", 0.0000010, 0.003},
}

// Weight is the importance weight for a stratum: how many real people each
// sampled persona stands for, relative to the general population.
func (s Stratum) Weight() float32 {
	sp := Strata[s]
	if sp.SampleFrac == 0 {
		return 0
	}
	return float32(sp.RealShare / sp.SampleFrac)
}

// World is the population. All slices are parallel and indexed by persona id.
type World struct {
	N    int
	Seed uint64

	// Immutable after generation. Uploaded to the GPU once and never moved
	// again, which is why they are separated from the mutable state below.
	Lon, Lat []float32
	Arch     []uint16 // archetype id: see Archetype
	Palette  []uint16 // row in the palette texture
	Shape    []uint16 // index into the sprite atlas
	Strat    []uint8
	Weight   []float32

	// Server-side only; never reaches the GPU record.
	Place    []uint16  // index into the centre table -- where this person lives
	Urbanity []float32 // 1 at the core of a centre, falling off with distance

	// Mutable per tick. This is the only buffer that goes back to the client
	// after the first snapshot, as run-length-encoded deltas.
	Flags []uint16
}

// Generate builds a population deterministically from a seed.
//
// Determinism is per persona, not per world: each persona's attributes come
// from a PRNG seeded with (worldSeed, index). Sharing one generator across
// goroutines would make output depend on scheduling, which is easy to do by
// accident and painful to discover once a "replay" turns out to be a different
// world.
func Generate(n int, seed uint64) *World {
	w := &World{
		N:        n,
		Seed:     seed,
		Lon:      make([]float32, n),
		Lat:      make([]float32, n),
		Arch:     make([]uint16, n),
		Palette:  make([]uint16, n),
		Shape:    make([]uint16, n),
		Strat:    make([]uint8, n),
		Weight:   make([]float32, n),
		Flags:    make([]uint16, n),
		Place:    make([]uint16, n),
		Urbanity: make([]float32, n),
	}

	// Cumulative stratum distribution, computed once.
	var cum [NumStrata]float64
	acc := 0.0
	for i := 0; i < int(NumStrata); i++ {
		acc += Strata[i].SampleFrac
		cum[i] = acc
	}

	for i := 0; i < n; i++ {
		r := newRand(seed, uint64(i))

		st := General
		p := r.f64() * acc
		for k := 0; k < int(NumStrata); k++ {
			if p <= cum[k] {
				st = Stratum(k)
				break
			}
		}
		w.Strat[i] = uint8(st)
		w.Weight[i] = st.Weight()

		lon, lat, place, urbanity := samplePosition(&r, st)
		w.Lon[i] = float32(lon * math.Pi / 180)
		w.Lat[i] = float32(lat * math.Pi / 180)
		w.Place[i] = place
		w.Urbanity[i] = urbanity

		// Archetype is derived, not drawn: region comes from where the person
		// landed, and the role from their stratum weighted by that region's
		// development index and how urban their spot is. So a herder is common
		// in the Sahel and vanishingly rare in Zurich without either being
		// forbidden -- which is the point, because the rare cells are where the
		// interesting personas live.
		reg := centres[place].region
		role := pickRole(&r, st, reg, float64(urbanity))
		arch := MakeArchetype(reg, role)
		w.Arch[i] = uint16(arch)

		// Palette and sprite are functions of the archetype so that what a dot
		// looks like encodes who it is. Derived rather than drawn: two personas
		// of the same archetype must be indistinguishable on screen, or the
		// globe shows noise where it should show structure.
		w.Palette[i] = uint16(arch) % 1024
		w.Shape[i] = uint16(role % 8)
	}
	return w
}

// WeightedMean is the only correct way to aggregate this population.
//
// There is no unweighted equivalent on purpose. With deliberate oversampling
// of rare strata, a raw mean over personas is not an estimate of anything, and
// it is a mistake that would otherwise be made about once a month.
func (w *World) WeightedMean(value func(i int) float64) float64 {
	var num, den float64
	for i := 0; i < w.N; i++ {
		wt := float64(w.Weight[i])
		num += wt * value(i)
		den += wt
	}
	if den == 0 {
		return 0
	}
	return num / den
}

// EncodeInstances writes the packed render records: an 8-byte header
// (magic, count) followed by N fixed-size instances.
//
// Encoded field by field rather than with binary.Write over a struct slice,
// because that path goes through reflection and costs more than the copy it
// performs. An unsafe.Slice reinterpret would be faster still and is the right
// move if this ever shows up in a profile; it is avoided here because a
// portable encoder is worth more than the microseconds until it does.
func (w *World) EncodeInstances(out io.Writer) error {
	hdr := make([]byte, 8)
	copy(hdr[0:4], "PPL1")
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(w.N))
	if _, err := out.Write(hdr); err != nil {
		return err
	}

	const batch = 8192
	buf := make([]byte, 0, batch*InstanceBytes)
	for i := 0; i < w.N; i++ {
		buf = binary.LittleEndian.AppendUint32(buf, math.Float32bits(w.Lon[i]))
		buf = binary.LittleEndian.AppendUint32(buf, math.Float32bits(w.Lat[i]))
		buf = binary.LittleEndian.AppendUint16(buf, w.Palette[i])
		buf = binary.LittleEndian.AppendUint16(buf, w.Shape[i])
		buf = binary.LittleEndian.AppendUint16(buf, w.Arch[i])
		buf = binary.LittleEndian.AppendUint16(buf, w.Flags[i])
		if len(buf) >= batch*InstanceBytes {
			if _, err := out.Write(buf); err != nil {
				return err
			}
			buf = buf[:0]
		}
	}
	if len(buf) > 0 {
		if _, err := out.Write(buf); err != nil {
			return err
		}
	}
	return nil
}

// StratumCounts reports how many personas landed in each stratum, which is the
// first thing to check when a population looks wrong.
func (w *World) StratumCounts() [NumStrata]int {
	var c [NumStrata]int
	for i := 0; i < w.N; i++ {
		c[w.Strat[i]]++
	}
	return c
}

// rand is a small splitmix64-seeded PCG-ish generator. Value type so it stays
// on the stack and costs no allocation per persona.
type rand struct{ s uint64 }

func newRand(seed, idx uint64) rand {
	// splitmix64 the (seed, index) pair so adjacent indices do not produce
	// correlated streams.
	z := seed + 0x9E3779B97F4A7C15*(idx+1)
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return rand{s: z ^ (z >> 31)}
}

func (r *rand) u32() uint32 {
	r.s += 0x9E3779B97F4A7C15
	z := r.s
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return uint32((z ^ (z >> 31)) >> 32)
}

func (r *rand) f64() float64 { return float64(r.u32()) / 4294967296.0 }

// norm returns a standard normal via Box-Muller. Only one of the pair is used;
// at one call per persona per axis the waste is not worth caching.
func (r *rand) norm() float64 {
	u := 1 - r.f64()
	v := r.f64()
	return math.Sqrt(-2*math.Log(u)) * math.Cos(2*math.Pi*v)
}
