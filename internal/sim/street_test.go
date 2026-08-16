package sim

import (
	"testing"

	"github.com/sakshamio/Populace/internal/world"
)

func TestNewStreetDrawsARealRosterFromOnePlace(t *testing.T) {
	w := testWorld(t, testN)
	g := testGraph(t, w)
	idx := world.BuildPlaceIndex(w)

	// Place 0 is guaranteed to have someone in it at testN scale (186 places,
	// 60,000 personas, and centres are weighted so none are near-empty).
	st := NewStreet(w, g, idx, 0, 1, 40)
	if st == nil {
		t.Fatal("expected a roster, got nil")
	}
	if len(st.Residents) == 0 {
		t.Fatal("expected at least one resident")
	}
	if len(st.Residents) > 40 {
		t.Errorf("asked for at most 40, got %d", len(st.Residents))
	}

	// Every resident actually belongs to place 0.
	for _, res := range st.Residents {
		if w.Place[res.ID] != 0 {
			t.Errorf("resident %d has place %d, want 0", res.ID, w.Place[res.ID])
		}
	}

	// Names must be unique within the roster, same reasoning as the chat.
	seen := map[string]bool{}
	for _, res := range st.Residents {
		if seen[res.Name] {
			t.Errorf("duplicate name %q in the roster", res.Name)
		}
		seen[res.Name] = true
	}
}

func TestNewStreetTiesAreSymmetricWithinTheRoster(t *testing.T) {
	w := testWorld(t, testN)
	g := testGraph(t, w)
	idx := world.BuildPlaceIndex(w)

	st := NewStreet(w, g, idx, 0, 1, 60)
	if st == nil {
		t.Fatal("expected a roster")
	}

	// Ties + LocalOther + Outside must equal Degree exactly, and every tie
	// index must point back at this same street (nothing lossy or approximated).
	for i, res := range st.Residents {
		if len(res.Ties)+res.LocalOther+res.Outside != res.Degree {
			t.Errorf("resident %d: %d ties + %d local-other + %d outside != degree %d",
				i, len(res.Ties), res.LocalOther, res.Outside, res.Degree)
		}
		for _, j := range res.Ties {
			if j < 0 || j >= len(st.Residents) {
				t.Fatalf("resident %d has a tie index %d out of range", i, j)
			}
		}
	}

	// A graph tie within the roster must be visible from both ends -- the
	// underlying CSR graph is undirected (see graph.go), so if A ties to B,
	// B's own Ties must contain A too.
	for i, res := range st.Residents {
		for _, j := range res.Ties {
			found := false
			for _, back := range st.Residents[j].Ties {
				if back == i {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("resident %d ties to %d, but %d does not tie back",
					i, j, j)
			}
		}
	}
}

// This is the reason LocalOther exists rather than only Ties/Outside: an
// empirical check found only about 1 in 6,000 of a place's real same-place
// ties survive into an arbitrary 48-person roster sample, so counting ties
// only against roster membership would report ~0 almost everywhere and make
// the view look broken even though real local structure exists.
func TestNewStreetFindsLocalTiesThatMissedTheRoster(t *testing.T) {
	w := testWorld(t, 300_000)
	g := testGraph(t, w)
	idx := world.BuildPlaceIndex(w)

	// Place 1 is populous enough at this scale (see the probe this test
	// generalises: ~50 LocalOther hits across a 48-person roster there).
	st := NewStreet(w, g, idx, 1, 42, streetTestRosterSize)
	if st == nil {
		t.Fatal("expected a roster")
	}
	total := 0
	for _, res := range st.Residents {
		total += res.LocalOther
	}
	if total == 0 {
		t.Error("expected some residents to have real ties to people from the " +
			"same place who didn't make the roster, got zero across the board")
	}
}

const streetTestRosterSize = 48

func TestNewStreetIsDeterministic(t *testing.T) {
	w := testWorld(t, testN)
	g := testGraph(t, w)
	idx := world.BuildPlaceIndex(w)

	a := NewStreet(w, g, idx, 3, 42, 30)
	b := NewStreet(w, g, idx, 3, 42, 30)
	if a == nil || b == nil {
		t.Fatal("expected a roster")
	}
	if len(a.Residents) != len(b.Residents) {
		t.Fatalf("same seed gave different roster sizes: %d vs %d",
			len(a.Residents), len(b.Residents))
	}
	for i := range a.Residents {
		if a.Residents[i].ID != b.Residents[i].ID {
			t.Errorf("resident %d differs between runs: %d vs %d",
				i, a.Residents[i].ID, b.Residents[i].ID)
		}
	}
}

func TestNewStreetRerollWithADifferentSeedGivesADifferentDraw(t *testing.T) {
	w := testWorld(t, testN)
	g := testGraph(t, w)
	idx := world.BuildPlaceIndex(w)

	a := NewStreet(w, g, idx, 3, 1, 20)
	b := NewStreet(w, g, idx, 3, 2, 20)
	if a == nil || b == nil {
		t.Fatal("expected a roster")
	}
	same := 0
	for i := range a.Residents {
		if i < len(b.Residents) && a.Residents[i].ID == b.Residents[i].ID {
			same++
		}
	}
	if same == len(a.Residents) {
		t.Error("rerolling with a different seed produced an identical roster")
	}
}

func TestNewStreetOnAnEmptyPlaceIsNil(t *testing.T) {
	w := testWorld(t, 200) // small enough that some of the 186 places are empty
	g := testGraph(t, w)
	idx := world.BuildPlaceIndex(w)

	foundEmpty := false
	for p := 0; p < world.NumPlaces(); p++ {
		if len(idx.Personas(p)) == 0 {
			foundEmpty = true
			if st := NewStreet(w, g, idx, uint16(p), 1, 40); st != nil {
				t.Errorf("place %d has nobody in it, expected nil, got %d residents",
					p, len(st.Residents))
			}
		}
	}
	if !foundEmpty {
		t.Skip("no empty place at this population size to test against")
	}
}

func TestStreetRefreshTracksLiveState(t *testing.T) {
	w := testWorld(t, testN)
	s := New(w, DefaultConfig())
	idx := world.BuildPlaceIndex(w)

	st := NewStreet(w, s.G, idx, s.Chat.OriginID, 7, 40)
	if st == nil {
		t.Fatal("expected a roster")
	}
	for _, res := range st.Residents {
		if res.State != "" {
			t.Fatalf("expected zero-value state before the first Refresh, got %q", res.State)
		}
	}

	s.Inject(Event{Headline: "big one", Stance: -0.6, Salience: 0.9,
		Seeding: SeedInRegion, SeedSize: testN / 10, Seed: 5, Difficulty: 0.9})
	for i := 0; i < 60; i++ {
		s.Advance()
	}
	st.Refresh(s)

	heard := 0
	for _, res := range st.Residents {
		if res.State != "hasn't heard" {
			heard++
		}
	}
	if heard == 0 {
		t.Error("seeded a large event and let it run 60 ticks, but nobody in the street roster heard about it")
	}
}
