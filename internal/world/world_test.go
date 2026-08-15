package world

import (
	"strings"
	"testing"
)

func TestArchetypeSpaceIsTheSizeTheBudgetAllows(t *testing.T) {
	n := NumArchetypes()
	// The bound that matters is generation cost: at the measured 99 tok/s
	// aggregate and ~250 tokens of reaction each, a thousand archetypes is
	// about forty minutes for the whole world. Ten thousand would be a week.
	if n < 200 || n > 1200 {
		t.Fatalf("%d archetypes (%d roles x %d regions) is outside the "+
			"generation budget the design assumes", n, NumRoles, NumRegions)
	}
	if uint16(n) != uint16(n&0xFFFF) || n > 65535 {
		t.Fatalf("%d archetypes will not fit the uint16 in the GPU record", n)
	}
	t.Logf("%d archetypes = %d roles x %d regions", n, NumRoles, NumRegions)
}

// Round-tripping matters because the archetype is stored as a bare uint16 in
// the render record and in the LLM cache key. If pack and unpack disagree, the
// model's answer for a Lagos herder gets applied to a Zurich banker and nothing
// about the output would look wrong.
func TestArchetypeRoundTrips(t *testing.T) {
	for reg := Region(0); reg < NumRegions; reg++ {
		for role := 0; role < NumRoles; role++ {
			a := MakeArchetype(reg, role)
			if a.Region() != reg {
				t.Fatalf("region %v round-tripped to %v", reg, a.Region())
			}
			if a.Role() != &Roles[role] {
				t.Fatalf("role %d round-tripped to %q", role, a.Role().Name)
			}
		}
	}
}

// Every stratum must be describable, or a persona exists that the model cannot
// be asked about.
func TestEveryStratumHasRoles(t *testing.T) {
	for st := Stratum(0); st < NumStrata; st++ {
		if len(rolesByStratum[st]) == 0 {
			t.Fatalf("stratum %s has no roles", Strata[st].Name)
		}
		t.Logf("%-15s %2d roles", Strata[st].Name, len(rolesByStratum[st]))
	}
}

// The point of conditioning roles on region: the occupational mix has to
// actually differ, or "region" is a label rather than a fact about the person.
func TestOccupationalMixVariesByRegion(t *testing.T) {
	w := Generate(400_000, 20260815)

	// counts[region][role]
	counts := make([][]int, NumRegions)
	for i := range counts {
		counts[i] = make([]int, NumRoles)
	}
	totals := make([]int, NumRegions)
	for i := 0; i < w.N; i++ {
		a := Archetype(w.Arch[i])
		counts[a.Region()][int(a)%NumRoles]++
		totals[a.Region()]++
	}

	share := func(reg Region, name string) float64 {
		for i := range Roles {
			if Roles[i].Name == name {
				if totals[reg] == 0 {
					return 0
				}
				return float64(counts[reg][i]) / float64(totals[reg])
			}
		}
		t.Fatalf("no role named %q", name)
		return 0
	}

	// A smallholder farmer should be common where most people still farm and
	// rare where almost nobody does. Not absent -- absent would be wrong.
	poorFarm := share(SubSaharanAfrica, "smallholder farmer")
	richFarm := share(NorthAmerica, "smallholder farmer")
	if poorFarm < richFarm*3 {
		t.Fatalf("smallholder share is %.3f%% in Sub-Saharan Africa and %.3f%% in "+
			"North America; the region conditioning is not doing anything",
			poorFarm*100, richFarm*100)
	}
	if richFarm == 0 {
		t.Fatal("no smallholder farmers at all in North America; the weighting " +
			"should make roles rare, never impossible")
	}

	// And the other pole.
	richEng := share(NorthAmerica, "software engineer")
	poorEng := share(SubSaharanAfrica, "software engineer")
	if richEng < poorEng*3 {
		t.Fatalf("software engineers are %.3f%% in North America and %.3f%% in "+
			"Sub-Saharan Africa; expected the opposite skew", richEng*100, poorEng*100)
	}

	t.Logf("smallholder farmer: Sub-Saharan Africa %.2f%%  North America %.2f%%  (%.1fx)",
		poorFarm*100, richFarm*100, poorFarm/richFarm)
	t.Logf("software engineer:  Sub-Saharan Africa %.2f%%  North America %.2f%%  (%.1fx)",
		poorEng*100, richEng*100, richEng/poorEng)

	// Within a stratum the mix should shift too, and it shifts the way a
	// conditional distribution should rather than the way the marginal does:
	// *given* that someone is high-net-worth, wealth in a lower-income region
	// is likelier to sit in plant and payroll than in a trading book.
	//
	// This was the first assertion written the wrong way round -- expecting
	// more finance professionals in Sub-Saharan Africa to be the failure -- and
	// the model was right. Worth keeping as a test precisely because the
	// intuition it corrects is easy to have twice.
	if share(SubSaharanAfrica, "industrialist") <= share(NorthAmerica, "industrialist") {
		t.Fatal("expected industrialists to dominate high-net-worth wealth in " +
			"lower-income regions")
	}
	if share(NorthAmerica, "finance professional") <= share(SubSaharanAfrica, "finance professional") {
		t.Fatal("expected finance to dominate high-net-worth wealth in higher-income regions")
	}
	t.Logf("of the high-net-worth: industrialist %.2f%% SSA vs %.2f%% NA; "+
		"finance %.2f%% NA vs %.2f%% SSA",
		share(SubSaharanAfrica, "industrialist")*100, share(NorthAmerica, "industrialist")*100,
		share(NorthAmerica, "finance professional")*100,
		share(SubSaharanAfrica, "finance professional")*100)
}

// Broadcasters must stay inside the media stratum. The oversampling of that
// stratum is what gives the network its degree tail, and a role leaking across
// strata would quietly break it.
func TestRolesStayInsideTheirStratum(t *testing.T) {
	w := Generate(200_000, 7)
	for i := 0; i < w.N; i++ {
		a := Archetype(w.Arch[i])
		if a.Role().Stratum != Stratum(w.Strat[i]) {
			t.Fatalf("persona %d is stratum %s but has role %q (stratum %s)",
				i, Strata[w.Strat[i]].Name, a.Role().Name,
				Strata[a.Role().Stratum].Name)
		}
	}
	t.Logf("all %d personas carry a role from their own stratum", w.N)
}

// The description is what the model is given. It must name the person's work
// and their region, because those are the two things the engine then assumes
// the answer was conditioned on.
func TestDescriptionNamesTheWorkAndThePlace(t *testing.T) {
	a := MakeArchetype(SubSaharanAfrica, 0)
	d := a.Describe()
	if !strings.Contains(d, Roles[0].Name) || !strings.Contains(d, "Sub-Saharan Africa") {
		t.Fatalf("description does not identify the archetype: %q", d)
	}
	t.Log(d)
	t.Log(MakeArchetype(NorthAmerica, len(Roles)-2).Describe())
	t.Log(MakeArchetype(SouthAsia, 25).Describe())
}

// Urbanity has to separate a city core from its hinterland, or UrbanBias is
// multiplying by noise.
func TestUrbanityFallsOffWithDistance(t *testing.T) {
	w := Generate(200_000, 3)
	var core, edge, coreN, edgeN float64
	for i := 0; i < w.N; i++ {
		if w.Urbanity[i] > 0.8 {
			coreN++
			if Archetype(w.Arch[i]).Role().UrbanBias > 0.3 {
				core++
			}
		} else if w.Urbanity[i] < 0.3 {
			edgeN++
			if Archetype(w.Arch[i]).Role().UrbanBias > 0.3 {
				edge++
			}
		}
	}
	if coreN == 0 || edgeN == 0 {
		t.Fatalf("urbanity did not spread: %v core, %v edge", coreN, edgeN)
	}
	cs, es := core/coreN, edge/edgeN
	if cs <= es {
		t.Fatalf("urban-biased roles are %.1f%% of city cores and %.1f%% of the "+
			"hinterland; urbanity is not conditioning anything", cs*100, es*100)
	}
	t.Logf("urban-biased roles: %.1f%% in city cores, %.1f%% in the hinterland "+
		"(%.0f%% / %.0f%% of the population)", cs*100, es*100,
		coreN/float64(w.N)*100, edgeN/float64(w.N)*100)
}
