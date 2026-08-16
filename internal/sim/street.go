package sim

import "github.com/sakshamio/Populace/internal/world"

// Street is the zoom rung between the population-scale globe and a single
// persona's inspector card: a bounded, named roster of people who actually
// live at one place, with the real graph ties among them.
//
// Unlike Cohort, membership isn't chosen to hold an experimental variable
// fixed -- it's just "who lives here", drawn once by NewStreet and then
// refreshed for live state on every poll by Refresh. The caller is
// responsible for holding onto one Street and calling Refresh rather than
// calling NewStreet again, so a viewer watches the same people's state
// change instead of a different draw shuffling under them every few seconds.
type Street struct {
	PlaceID   uint16
	PlaceName string
	Region    string
	Lat, Lon  float64 // radians, the place's own centre

	Residents []StreetResident
}

// StreetResident is one real person in the roster. Position is their actual
// generated lon/lat (radians, same convention as world.World.Lon/Lat) --
// street level shows the true Gaussian scatter around the place centre,
// nothing laid out for legibility.
type StreetResident struct {
	ID      int32   `json:"id"`
	Name    string  `json:"name"`
	Role    string  `json:"role"`
	Lon     float64 `json:"lon"`
	Lat     float64 `json:"lat"`
	Opinion float64 `json:"opinion"`
	State   string  `json:"state"`
	Degree  int     `json:"degree"`
	Feed    bool    `json:"feed_reached"`

	// Ties indexes into this same Street's Residents slice -- graph
	// neighbours who also happen to live in this roster. Outside is the
	// count of real graph neighbours who don't: per graph.go, only about
	// PropGeo of anyone's edges are local, the rest are homophily or
	// hub/broadcast ties that can span continents. Outside is not hidden or
	// approximated away -- it's exactly Degree minus len(Ties).
	// Ties, LocalOther and Outside partition Degree three ways, checked
	// against w.Place directly rather than against roster membership: an
	// empirical check on a 2M population found ~10% of anyone's ties share
	// their own named place, but a random 48-person roster only ever catches
	// about 1 in 6,000 of those -- checking "is this neighbour also in the
	// slice I happened to draw" would report zero ties almost everywhere and
	// make the view look broken. w.Place[nb]==w.Place[id] is the honest
	// question, and it's an O(1) lookup per neighbour either way.
	//
	//   Ties       neighbours in this same place who also made the roster --
	//              drawable, indexes into this Street's Residents slice.
	//   LocalOther neighbours in this same place who didn't make the roster --
	//              real local ties, just not one of the k shown here.
	//   Outside    neighbours who live somewhere else entirely -- per
	//              graph.go, homophily and hub ties can span continents, and
	//              this is not hidden or approximated away.
	Ties       []int `json:"ties"`
	LocalOther int   `json:"local_other"`
	Outside    int   `json:"outside"`
}

// NewStreet draws up to k residents from place p and resolves their graph
// ties within that draw. idx must come from world.BuildPlaceIndex, built once
// at load -- this walks only p's own bucket, never the full population.
//
// Returns nil if the place has nobody in it (possible at small -n).
func NewStreet(w *world.World, g *Graph, idx *world.PlaceIndex, p uint16, seed uint64, k int) *Street {
	bucket := idx.Personas(int(p))
	if len(bucket) == 0 {
		return nil
	}
	name, region, lat, lon := world.PlaceInfo(int(p))

	r := newRNG(seed, uint64(p))
	ids := reservoirFromSlice(bucket, &r, k)

	st := &Street{
		PlaceID: p, PlaceName: name, Region: region.String(), Lat: lat, Lon: lon,
		Residents: make([]StreetResident, len(ids)),
	}
	roster := make(map[int32]int, len(ids)) // persona id -> index into Residents
	for i, id := range ids {
		roster[id] = i
	}

	taken := map[string]bool{}
	for i, id := range ids {
		a := world.Archetype(w.Arch[id])
		st.Residents[i] = StreetResident{
			ID:     id,
			Name:   uniqueName(a.Region(), uint64(id), taken),
			Role:   a.Role().Name,
			Lon:    float64(w.Lon[id]),
			Lat:    float64(w.Lat[id]),
			Degree: g.Degree(int(id)),
		}
	}
	for i, id := range ids {
		// Non-nil: an empty result must serialise as [] rather than null, or
		// every client has to special-case "no ties" (same reasoning as
		// Cohort.Friends/Messages in cohort.go).
		ties := []int{}
		localOther, outside := 0, 0
		for _, nb := range g.Neighbours(int(id)) {
			if j, ok := roster[nb]; ok {
				ties = append(ties, j)
			} else if w.Place[nb] == p {
				localOther++
			} else {
				outside++
			}
		}
		st.Residents[i].Ties = ties
		st.Residents[i].LocalOther = localOther
		st.Residents[i].Outside = outside
	}
	return st
}

// Refresh reads live opinion/state back onto a fixed roster -- called every
// poll, unlike NewStreet which only runs on entry or an explicit reroll.
func (st *Street) Refresh(s *Sim) {
	if st == nil {
		return
	}
	for i := range st.Residents {
		id := st.Residents[i].ID
		st.Residents[i].Opinion = float64(s.O.Y[id])
		st.Residents[i].State = stateName(s.C.State[id])
		st.Residents[i].Feed = s.M.Enabled() && s.M.Exposed[id]
	}
}

// reservoirFromSlice draws up to k ids from candidates by reservoir sampling
// -- the same reasoning as reservoirK, just over an already-filtered slice
// instead of a predicate scan, since a place's bucket is already exactly
// "everyone who matches".
func reservoirFromSlice(candidates []int32, r *rng, k int) []int32 {
	if len(candidates) <= k {
		out := make([]int32, len(candidates))
		copy(out, candidates)
		return out
	}
	out := make([]int32, k)
	copy(out, candidates[:k])
	for i := k; i < len(candidates); i++ {
		if j := int(r.u32()) % (i + 1); j < k {
			out[j] = candidates[i]
		}
	}
	return out
}
