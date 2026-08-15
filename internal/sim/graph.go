// Package sim is the dynamics layer: who is connected to whom, how opinions
// move along those connections, and how behaviours spread across them.
//
// Everything here is deterministic given (world seed, graph seed). That is not
// fastidiousness -- a simulation you cannot replay is a simulation whose
// surprising result you cannot investigate. The two places determinism is easy
// to lose are parallel accumulation into shared memory and adjacency order
// that depends on goroutine scheduling, and both are handled explicitly below.
package sim

import (
	"math"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/sakshamio/Populace/internal/world"
)

// Graph is an undirected social network in compressed sparse row form.
//
// Row i occupies Adj[Off[i]:End[i]]. The separate End is not an accident: rows
// are allocated at their upper-bound degree and then deduplicated in place, and
// compacting the array afterwards would need a second edge-sized allocation --
// 744 MB at ten million people. Four bytes per node is the cheaper trade.
//
// int32 indices throughout. Ten million nodes at ~19 average degree is 186
// million entries, comfortably inside int32 and half the size of int64. If this
// ever needs to exceed 2.1 billion edges the machine will have run out of
// memory long before the type does.
type Graph struct {
	N   int
	Off []int32
	End []int32
	Adj []int32

	// Order and Rank are the spatial permutation used to build geographic
	// ties, kept rather than discarded because they are also the cheapest
	// possible answer to "who lives near this person": a contiguous run of
	// Order is a place. Regional seeding and regional aggregates both need
	// that, and recomputing it would cost another counting sort.
	Order []int32 // Order[position] = persona id
	Rank  []int32 // Rank[id] = position

	Seed  uint64
	Edges int64
}

// Neighbours returns node i's adjacency row. The slice aliases the graph and
// must not be modified.
func (g *Graph) Neighbours(i int) []int32 { return g.Adj[g.Off[i]:g.End[i]] }

// Degree is the number of distinct neighbours of i.
func (g *Graph) Degree(i int) int { return int(g.End[i] - g.Off[i]) }

// GraphConfig controls the mixture of tie-formation mechanisms.
//
// The three shares are not arbitrary. A purely geographic network has no long
// ties and nothing ever crosses a continent; a purely homophilous one shatters
// into disconnected archetype cliques; a purely preferential one is a star with
// no local structure and no clustering at all. Real social networks are mostly
// local and homophilous with a thin layer of long-range and broadcast ties, and
// it is that thin layer that decides whether anything spreads.
type GraphConfig struct {
	// PropGeo, PropHomophily: the rest go to the hub pool.
	PropGeo       float64
	PropHomophily float64

	// MeanDegree is the mean number of ties each node *proposes*. Because ties
	// are symmetrised, realised mean degree is roughly twice this.
	MeanDegree float64
	SigmaLog   float64 // lognormal spread of individual degree

	// Broadcast multiplies proposed degree by stratum. Media personas are
	// broadcast nodes; without them the degree distribution has no tail and
	// every cascade result computed on the graph is wrong.
	Broadcast [world.NumStrata]float64

	MaxDegree int
	Seed      uint64
}

func DefaultGraphConfig() GraphConfig {
	return GraphConfig{
		PropGeo:       0.55,
		PropHomophily: 0.30,
		MeanDegree:    7,
		SigmaLog:      0.8,
		Broadcast: [world.NumStrata]float64{
			world.General:      1.0,
			world.Nomadic:      0.7, // mobile, weaker persistent local ties
			world.Immigrant:    1.1, // dense in-group plus origin-country ties
			world.HighNetWorth: 4.0,
			world.Media:        40.0,
		},
		MaxDegree: 20000,
		Seed:      0x50505541,
	}
}

// BuildGraph constructs the social network over an existing population.
//
// Two generation passes rather than one, on purpose. Materialising every
// proposed edge to count degrees before placing them would need a pair-sized
// scratch buffer; regenerating from the same per-node seed costs a second pass
// of arithmetic and no memory at all. On this workload arithmetic is free and
// memory is not.
func BuildGraph(w *world.World, cfg GraphConfig) *Graph {
	n := w.N
	g := &Graph{N: n, Seed: cfg.Seed}

	rank := spatialRank(w)
	archOff, archMembers := bucketByArchetype(w)

	// Proposed degree per node, drawn once and reused by both passes.
	kdeg := make([]int32, n)
	mu := math.Log(cfg.MeanDegree) - cfg.SigmaLog*cfg.SigmaLog/2
	parallel(n, func(lo, hi int) {
		for i := lo; i < hi; i++ {
			r := newRNG(cfg.Seed^0xD1B54A32D192ED03, uint64(i))
			// Stratum sets the scale, role varies within it: a celebrity
			// performer and a documentary producer are both broadcasters and
			// are not the same size of broadcaster.
			k := math.Exp(mu+cfg.SigmaLog*r.norm()) *
				cfg.Broadcast[w.Strat[i]] * world.Archetype(w.Arch[i]).Reach()
			ki := int(k + 0.5)
			if ki < 1 {
				ki = 1
			}
			if ki > cfg.MaxDegree {
				ki = cfg.MaxDegree
			}
			kdeg[i] = int32(ki)
		}
	})

	hubs := hubPool(kdeg, cfg.Seed)

	g.Rank, g.Order = rank, rankToOrder(rank)
	b := &builder{
		w: w, cfg: cfg, rank: g.Rank, order: g.Order,
		archOff: archOff, archMembers: archMembers, hubs: hubs, kdeg: kdeg,
	}

	// Pass 1: count. Each proposal contributes to both endpoints.
	cap32 := make([]int32, n+1)
	parallel(n, func(lo, hi int) {
		buf := make([]int32, 0, 64)
		for i := lo; i < hi; i++ {
			buf = b.proposals(i, buf[:0])
			atomic.AddInt32(&cap32[i], int32(len(buf)))
			for _, j := range buf {
				atomic.AddInt32(&cap32[j], 1)
			}
		}
	})

	// Prefix sum into Off. Sequential and int64-accumulated: at ten million
	// nodes this is milliseconds, and a parallel scan here would buy nothing
	// while adding a place for an overflow bug to hide.
	g.Off = make([]int32, n+1)
	var total int64
	for i := 0; i < n; i++ {
		g.Off[i] = int32(total)
		total += int64(cap32[i])
	}
	g.Off[n] = int32(total)
	g.Adj = make([]int32, total)

	// Pass 2: place. Cursors are atomic, so *within-row order is not
	// deterministic here* -- it depends on which goroutine got there first.
	// That is fixed by sorting each row below, which is not merely tidying:
	// unsorted rows would make float accumulation order scheduling-dependent
	// and two identical runs would diverge in the last bits.
	cur := make([]int32, n)
	copy(cur, g.Off[:n])
	parallel(n, func(lo, hi int) {
		buf := make([]int32, 0, 64)
		for i := lo; i < hi; i++ {
			buf = b.proposals(i, buf[:0])
			// One bulk reservation for i's own row, then one slot per target.
			// cur[i] is also incremented by other goroutines writing reverse
			// edges into row i, which is exactly why the bulk claim is atomic.
			base := atomic.AddInt32(&cur[i], int32(len(buf))) - int32(len(buf))
			for k, j := range buf {
				g.Adj[base+int32(k)] = j
				p := atomic.AddInt32(&cur[j], 1) - 1
				g.Adj[p] = int32(i)
			}
		}
	})

	// Sort and deduplicate each row.
	//
	// Duplicates arise when i proposes j and j independently proposes i. Rare
	// per pair, but with millions of pairs and a heavy-tailed degree
	// distribution it happens often enough around hubs to matter: a duplicate
	// edge silently doubles that neighbour's weight in the opinion update.
	g.End = make([]int32, n)
	var edges atomic.Int64
	parallel(n, func(lo, hi int) {
		var local int64
		for i := lo; i < hi; i++ {
			row := g.Adj[g.Off[i]:g.Off[i+1]]
			if len(row) < 32 {
				insertionSort(row)
			} else {
				slices.Sort(row)
			}
			m := 0
			self := int32(i)
			for _, v := range row {
				if v == self {
					continue
				}
				if m > 0 && row[m-1] == v {
					continue
				}
				row[m] = v
				m++
			}
			g.End[i] = g.Off[i] + int32(m)
			local += int64(m)
		}
		edges.Add(local)
	})
	g.Edges = edges.Load() / 2
	return g
}

// builder holds the per-pass lookup structures. Kept in one struct so that both
// passes provably generate the same proposals from the same inputs.
type builder struct {
	w           *world.World
	cfg         GraphConfig
	rank, order []int32
	archOff     []int32
	archMembers []int32
	hubs        []int32
	kdeg        []int32
}

// proposals regenerates node i's outgoing tie proposals. Deterministic: seeded
// only by (graph seed, i), never by iteration order or goroutine identity.
func (b *builder) proposals(i int, out []int32) []int32 {
	r := newRNG(b.cfg.Seed, uint64(i))
	n := int32(b.w.N)
	k := int(b.kdeg[i])

	// Log-uniform hop distance over the spatially sorted order: most ties are
	// to near neighbours, a few reach across the world. This is what produces
	// short path lengths without destroying local clustering, and it is the
	// mechanism the cascade results are most sensitive to.
	span := math.Log(float64(b.w.N))

	geoCut := b.cfg.PropGeo
	homCut := geoCut + b.cfg.PropHomophily

	for c := 0; c < k; c++ {
		var j int32
		switch u := r.f64(); {
		case u < geoCut:
			d := int32(math.Exp(r.f64()*span)) + 1
			if r.u32()&1 == 0 {
				d = -d
			}
			p := (b.rank[i] + d) % n
			if p < 0 {
				p += n
			}
			j = b.order[p]

		case u < homCut:
			a := b.w.Arch[i]
			lo, hi := b.archOff[a], b.archOff[a+1]
			if hi > lo {
				j = b.archMembers[lo+int32(r.u32()%uint32(hi-lo))]
			} else {
				j = int32(r.u32() % uint32(b.w.N))
			}

		default:
			j = b.hubs[int(r.u32())%len(b.hubs)]
		}
		if j != int32(i) {
			out = append(out, j)
		}
	}
	return out
}

// hubPool draws a fixed-size sample of nodes with probability proportional to
// degree, which is preferential attachment without the O(log N) search or the
// N-sized cumulative array. Systematic sampling: one random start, then a
// constant stride through cumulative weight. Exactly proportional-to-size,
// single pass, deterministic.
func hubPool(deg []int32, seed uint64) []int32 {
	const poolSize = 1 << 20
	var total float64
	for _, d := range deg {
		total += float64(d)
	}
	m := poolSize
	if m > len(deg) {
		m = len(deg)
	}
	step := total / float64(m)
	r := newRNG(seed, 0xB1)
	next := r.f64() * step

	pool := make([]int32, 0, m)
	var acc float64
	for i, d := range deg {
		acc += float64(d)
		for acc > next && len(pool) < m {
			pool = append(pool, int32(i))
			next += step
		}
	}
	if len(pool) == 0 {
		pool = append(pool, 0)
	}
	return pool
}

// spatialRank assigns each node its position in a globally sorted spatial
// order, so that "nearby in the array" means "nearby on the planet" and a
// geographic tie is an array offset rather than a distance query.
//
// Counting sort over a 2048x1024 cell grid: O(N), no comparisons, and
// deterministic because a stable counting sort has exactly one valid output.
func spatialRank(w *world.World) []int32 {
	const lonCells, latCells = 2048, 1024
	const nCells = lonCells * latCells

	cell := make([]int32, w.N)
	parallel(w.N, func(lo, hi int) {
		for i := lo; i < hi; i++ {
			x := int((float64(w.Lon[i])/math.Pi + 1) * 0.5 * lonCells)
			y := int((float64(w.Lat[i])/(math.Pi/2) + 1) * 0.5 * latCells)
			x = clamp(x, 0, lonCells-1)
			y = clamp(y, 0, latCells-1)
			cell[i] = int32(y*lonCells + x)
		}
	})

	counts := make([]int32, nCells+1)
	for _, c := range cell {
		counts[c]++
	}
	var run int32
	for c := 0; c <= nCells; c++ {
		v := counts[c]
		counts[c] = run
		run += v
	}
	rank := make([]int32, w.N)
	for i := 0; i < w.N; i++ {
		rank[i] = counts[cell[i]]
		counts[cell[i]]++
	}
	return rank
}

func rankToOrder(rank []int32) []int32 {
	order := make([]int32, len(rank))
	for i, p := range rank {
		order[p] = int32(i)
	}
	return order
}

// bucketByArchetype builds a CSR index from archetype to its members, so
// homophilous ties are an O(1) draw instead of a scan.
func bucketByArchetype(w *world.World) (off, members []int32) {
	maxA := 0
	for i := 0; i < w.N; i++ {
		if int(w.Arch[i]) > maxA {
			maxA = int(w.Arch[i])
		}
	}
	off = make([]int32, maxA+2)
	for i := 0; i < w.N; i++ {
		off[w.Arch[i]+1]++
	}
	for a := 1; a < len(off); a++ {
		off[a] += off[a-1]
	}
	members = make([]int32, w.N)
	cur := make([]int32, len(off))
	copy(cur, off)
	for i := 0; i < w.N; i++ {
		a := w.Arch[i]
		members[cur[a]] = int32(i)
		cur[a]++
	}
	return off, members
}

// DegreeStats reports the shape of the degree distribution. The mean says very
// little on its own -- a network with the right mean and no tail behaves
// nothing like a real one -- so the percentiles and max are the numbers to
// look at.
type DegreeStats struct {
	Mean            float64 `json:"mean"`
	P50             int     `json:"p50"`
	P90             int     `json:"p90"`
	P99             int     `json:"p99"`
	Max             int     `json:"max"`
	Isolated        int     `json:"isolated"`
	MediaMeanDegree float64 `json:"media_mean"`
}

func (g *Graph) DegreeStats(w *world.World) DegreeStats {
	d := make([]int, g.N)
	var sum, mediaSum, mediaN float64
	s := DegreeStats{}
	for i := 0; i < g.N; i++ {
		d[i] = g.Degree(i)
		sum += float64(d[i])
		if d[i] == 0 {
			s.Isolated++
		}
		if w != nil && world.Stratum(w.Strat[i]) == world.Media {
			mediaSum += float64(d[i])
			mediaN++
		}
	}
	slices.Sort(d)
	s.Mean = sum / float64(g.N)
	s.P50 = d[g.N/2]
	s.P90 = d[g.N*90/100]
	s.P99 = d[g.N*99/100]
	s.Max = d[g.N-1]
	if mediaN > 0 {
		s.MediaMeanDegree = mediaSum / mediaN
	}
	return s
}

func insertionSort(a []int32) {
	for i := 1; i < len(a); i++ {
		v := a[i]
		j := i - 1
		for j >= 0 && a[j] > v {
			a[j+1] = a[j]
			j--
		}
		a[j+1] = v
	}
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// parallel splits [0,n) into contiguous chunks, one per CPU. Contiguous rather
// than strided so each goroutine streams linearly through the SoA arrays, and
// fixed-size so the split does not depend on how fast any worker happens to run.
func parallel(n int, fn func(lo, hi int)) {
	if n == 0 {
		return
	}
	workers := runtime.GOMAXPROCS(0)
	if workers > n {
		workers = n
	}
	if workers <= 1 {
		fn(0, n)
		return
	}
	chunk := (n + workers - 1) / workers
	var wg sync.WaitGroup
	for start := 0; start < n; start += chunk {
		end := start + chunk
		if end > n {
			end = n
		}
		wg.Add(1)
		go func(lo, hi int) { defer wg.Done(); fn(lo, hi) }(start, end)
	}
	wg.Wait()
}

// numWorkers and parallelIdx exist so a parallel reduction can write into a
// per-worker slot instead of contending on a lock. The chunk index is stable
// for a given n, so a reduction over the slots is order-independent.
func numWorkers(n int) int {
	if n == 0 {
		return 0
	}
	w := runtime.GOMAXPROCS(0)
	if w > n {
		w = n
	}
	if w < 1 {
		w = 1
	}
	return w
}

func parallelIdx(n int, fn func(chunk, lo, hi int)) {
	workers := numWorkers(n)
	if workers <= 1 {
		if n > 0 {
			fn(0, 0, n)
		}
		return
	}
	chunk := (n + workers - 1) / workers
	var wg sync.WaitGroup
	idx := 0
	for start := 0; start < n; start += chunk {
		end := start + chunk
		if end > n {
			end = n
		}
		wg.Add(1)
		go func(c, lo, hi int) { defer wg.Done(); fn(c, lo, hi) }(idx, start, end)
		idx++
	}
	wg.Wait()
}

// rng is a private splitmix64 stream, deliberately separate from the one in
// package world. Sharing it would mean that changing a graph parameter shifted
// every persona's attributes, so a graph experiment would silently become a
// different population and the comparison would be meaningless.
type rng struct{ s uint64 }

func newRNG(seed, idx uint64) rng {
	z := seed + 0x9E3779B97F4A7C15*(idx+1)
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return rng{s: z ^ (z >> 31)}
}

func (r *rng) u32() uint32 {
	r.s += 0x9E3779B97F4A7C15
	z := r.s
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return uint32((z ^ (z >> 31)) >> 32)
}

func (r *rng) f64() float64 { return float64(r.u32()) / 4294967296.0 }

func (r *rng) norm() float64 {
	u := 1 - r.f64()
	v := r.f64()
	return math.Sqrt(-2*math.Log(u)) * math.Cos(2*math.Pi*v)
}
