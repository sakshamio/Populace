package world

import "math"

// Archetypes.
//
// An archetype is the unit the language model writes for. The whole economics
// of the app rest on there being few enough of them to generate once and cache
// forever, and enough of them that "how would this person react" has a
// different answer for a Lagos market trader and a Zurich fund manager.
//
// The construction is a role crossed with a region: ~45 roles by 10 regions is
// ~450 cells, which at the measured 99 tok/s aggregate is about ten minutes of
// generation for the entire world, once. A finer cross -- adding age bands and
// education would give tens of thousands -- would be more faithful and would
// also mean the model never finishes. That trade is the reason archetypes exist
// as a concept rather than every persona being generated individually.
//
// What varies *within* an archetype is handled cheaply and deterministically in
// Go: age, exact position, opinion prior, adoption threshold, tie count. Two
// personas of the same archetype are not the same person; they are people the
// model would write the same way about.

// Region is a coarse geography. Coarse because its job is to make an
// occupation plausible, not to be a gazetteer -- and because every extra region
// multiplies the generation budget.
type Region uint8

const (
	SubSaharanAfrica Region = iota
	MENA
	SouthAsia
	EastAsia
	SoutheastAsia
	Europe
	Eurasia
	NorthAmerica
	LatinAmerica
	Oceania
	NumRegions
)

// RegionSpec carries the one number that most changes which roles are common.
//
// Dev is a rough income/urbanisation index in [0,1]. It is deliberately a
// single scalar: a 45-by-10 hand-written weight table would be more precise and
// would also be 450 numbers nobody could check, and the point here is that the
// mix shifts in the right direction, not that it matches a labour survey.
type RegionSpec struct {
	Name string
	Dev  float64
}

var Regions = [NumRegions]RegionSpec{
	SubSaharanAfrica: {"Sub-Saharan Africa", 0.18},
	MENA:             {"Middle East & North Africa", 0.42},
	SouthAsia:        {"South Asia", 0.25},
	EastAsia:         {"East Asia", 0.62},
	SoutheastAsia:    {"Southeast Asia", 0.42},
	Europe:           {"Europe", 0.85},
	Eurasia:          {"Eurasia", 0.50},
	NorthAmerica:     {"North America", 0.90},
	LatinAmerica:     {"Latin America", 0.45},
	Oceania:          {"Oceania", 0.85},
}

func (r Region) String() string { return Regions[r].Name }

// Role is what someone does, which in this model is the strongest single
// predictor of both who they know and how they read the news.
type Role struct {
	Name    string
	Stratum Stratum

	// Base is how common the role is before any region or urbanity
	// adjustment, relative to the others in its stratum.
	//
	// Without it every rural-biased role is equally likely and the world comes
	// out with almost as many fishers as farmers, which is wrong by more than
	// an order of magnitude. Occupational frequency is mostly a fact about the
	// job, not about the place.
	Base float64

	// DevBias is where in the world this role concentrates: -1 is almost
	// entirely low-income regions, +1 almost entirely high-income. A market
	// trader and a fund manager are the two poles.
	DevBias float64

	// UrbanBias is -1 for roles that exist away from cities and +1 for roles
	// that exist only in a dense core.
	UrbanBias float64

	// Openness and Security place the role on the two axes that survey work
	// keeps recovering: traditional-to-secular and survival-to-expression.
	// They set the prior, not the opinion -- what someone thinks about a
	// specific story is still decided by the network.
	Openness float64 // -1 traditional, +1 secular-rational
	Security float64 // -1 survival-focused, +1 self-expression-focused

	// Reach multiplies the number of ties this role forms. An influencer and a
	// smallholder differ here by two orders of magnitude, and that difference
	// is most of what decides whose reaction spreads.
	Reach float64

	// Sketch is the seed of the prompt the model gets. One clause, present
	// tense, no adjectives about character -- the model fills in the person,
	// and a sketch that already asserts a personality produces 450 variations
	// on whatever personality was written here.
	Sketch string
}

// Roles is the taxonomy. It reaches deliberately from the bottom of the income
// distribution to the top, because a simulation stocked only with the
// comfortable middle answers every question the same way.
var Roles = []Role{
	// --- general population -------------------------------------------------
	{"smallholder farmer", General, 10, -0.85, -0.95, -0.5, -0.6, 0.7,
		"works a few hectares they do not own outright, and reads the weather before anything else"},
	{"market trader", General, 5, -0.7, 0.3, -0.2, -0.4, 1.3,
		"sells from a stall six days a week and knows every price in the district"},
	{"factory worker", General, 6, -0.3, 0.4, -0.1, -0.3, 0.9,
		"works a shift pattern on an assembly line and is one rota change from a childcare crisis"},
	{"construction labourer", General, 5, -0.5, 0.5, -0.3, -0.5, 0.8,
		"is paid by the day on sites that finish and then have to be found again"},
	{"domestic worker", General, 3, -0.5, 0.4, -0.3, -0.5, 0.7,
		"cleans and cooks in other people's homes, often for families richer than anyone they grew up near"},
	{"rideshare driver", General, 2, -0.1, 0.7, 0.0, -0.2, 1.1,
		"drives against an app's estimate of what their time is worth"},
	{"shopkeeper", General, 4, -0.3, 0.2, -0.2, -0.2, 1.2,
		"runs a small shop where the customers are also the neighbours"},
	{"schoolteacher", General, 2.5, 0.0, 0.0, 0.3, 0.1, 1.4,
		"teaches a class larger than the one they were trained for"},
	{"nurse", General, 1.5, 0.1, 0.3, 0.3, 0.0, 1.2,
		"works nights in a ward that is chronically short-staffed"},
	{"civil servant", General, 2, 0.2, 0.4, 0.2, 0.1, 1.1,
		"administers a policy they did not write and cannot change"},
	{"police officer", General, 1, 0.0, 0.3, -0.3, -0.2, 1.0,
		"polices a district where trust in policing is contested"},
	{"soldier", General, 0.8, -0.1, -0.1, -0.4, -0.4, 0.8,
		"serves in a force that is respected in public and argued about in private"},
	{"office administrator", General, 4, 0.4, 0.6, 0.2, 0.2, 1.0,
		"keeps a mid-sized organisation running from a desk nobody visits"},
	{"software engineer", General, 1.5, 0.7, 0.7, 0.6, 0.6, 1.3,
		"builds systems for a company headquartered somewhere else"},
	{"university student", General, 3, 0.3, 0.6, 0.6, 0.5, 1.6,
		"is studying something their family has opinions about"},
	{"retired pensioner", General, 5, 0.3, -0.1, -0.3, -0.1, 0.7,
		"lives on a fixed income and watched this argument happen once before"},
	{"unemployed young adult", General, 3, 0.0, 0.4, 0.1, -0.3, 1.2,
		"has more qualifications than the local labour market has jobs"},
	{"care worker", General, 1.5, 0.2, 0.2, 0.1, -0.1, 0.9,
		"looks after other people's elderly parents for close to minimum wage"},
	{"long-haul driver", General, 1, 0.0, -0.4, -0.2, -0.3, 0.6,
		"spends more nights in a cab than at home"},
	{"fisher", General, 0.7, -0.6, -0.8, -0.3, -0.4, 0.6,
		"works water that yields less each year than it did"},
	{"mine or energy worker", General, 0.8, -0.2, -0.6, -0.3, -0.3, 0.8,
		"works an extraction job that the region depends on and outsiders campaign against"},
	{"hospitality worker", General, 2.5, 0.1, 0.6, 0.2, 0.0, 1.1,
		"serves tourists whose daily spend exceeds their weekly pay"},
	{"freelance creative", General, 0.8, 0.6, 0.8, 0.6, 0.7, 1.5,
		"invoices four clients and is owed by three"},
	{"small business owner", General, 3, 0.2, 0.3, 0.0, 0.1, 1.4,
		"employs six people and personally guarantees the lease"},
	{"religious leader", General, 0.5, -0.3, -0.2, -0.8, -0.2, 1.8,
		"is asked for a view on things far outside their training, and gives one"},

	// --- nomadic ------------------------------------------------------------
	{"pastoral herder", Nomadic, 4, -0.9, -1.0, -0.6, -0.6, 0.6,
		"moves livestock along routes that are being fenced off"},
	{"seasonal migrant labourer", Nomadic, 4, -0.7, -0.7, -0.2, -0.6, 0.7,
		"follows harvests across borders and is counted in neither place"},
	{"digital nomad", Nomadic, 0.5, 0.8, 0.7, 0.7, 0.8, 1.4,
		"works remotely from whichever country's visa is easiest this year"},
	{"itinerant trader", Nomadic, 2, -0.6, -0.3, -0.3, -0.4, 1.2,
		"runs goods between markets that do not talk to each other"},

	// --- immigrant ----------------------------------------------------------
	{"first-generation labour migrant", Immigrant, 5, -0.2, 0.5, -0.2, -0.5, 1.0,
		"sends money home monthly and has not been back in years"},
	{"refugee", Immigrant, 2, -0.4, 0.3, -0.1, -0.7, 0.8,
		"is waiting on a decision made by people they will never meet"},
	{"second-generation professional", Immigrant, 3, 0.6, 0.7, 0.5, 0.5, 1.3,
		"is fluent in two worlds and fully trusted by neither"},
	{"international student", Immigrant, 1.5, 0.5, 0.8, 0.6, 0.4, 1.4,
		"is on a visa that depends on staying enrolled"},
	{"diaspora business owner", Immigrant, 1, 0.3, 0.5, 0.0, 0.2, 1.5,
		"built a business serving a community that arrived when they did"},

	// --- high net worth -----------------------------------------------------
	{"industrialist", HighNetWorth, 1, 0.4, 0.4, -0.1, 0.3, 3.0,
		"owns plant and payroll in a region that notices when either moves"},
	{"finance professional", HighNetWorth, 2, 0.85, 0.9, 0.4, 0.5, 2.5,
		"prices other people's risk and is paid on the outcome"},
	{"property developer", HighNetWorth, 1, 0.5, 0.9, -0.1, 0.2, 2.8,
		"is rebuilding a district whose residents were not asked"},
	{"senior executive", HighNetWorth, 2.5, 0.7, 0.8, 0.2, 0.4, 2.6,
		"answers to a board and is quoted as though they answer to the public"},
	{"inherited wealth", HighNetWorth, 1, 0.7, 0.6, 0.0, 0.5, 2.0,
		"has never had to price a decision by what it costs"},

	// --- media --------------------------------------------------------------
	{"broadcast journalist", Media, 2, 0.5, 0.9, 0.5, 0.5, 8.0,
		"has ninety seconds to explain something that took a week to understand"},
	{"newspaper editor", Media, 1, 0.5, 0.9, 0.4, 0.4, 7.0,
		"decides each day what several million people will consider important"},
	{"social media influencer", Media, 3, 0.4, 0.8, 0.4, 0.7, 9.0,
		"is paid by attention and knows exactly which framing earns it"},
	{"documentary producer", Media, 0.5, 0.7, 0.8, 0.6, 0.7, 5.0,
		"spends two years on a story that will be discussed for two days"},
	{"celebrity performer", Media, 0.5, 0.75, 0.9, 0.4, 0.8, 10.0,
		"is recognised in countries they have never worked in"},
	{"podcast host", Media, 1, 0.6, 0.7, 0.4, 0.6, 6.0,
		"talks for three hours a week to an audience that trusts them more than the news"},
}

// NumRoles is fixed at init and used as the archetype radix.
var NumRoles = len(Roles)

// Archetype packs a (region, role) pair into the uint16 that rides in the GPU
// record. Region-major so that all of a region's archetypes are adjacent, which
// makes a region-scoped generation job a contiguous range.
type Archetype uint16

func MakeArchetype(reg Region, role int) Archetype {
	return Archetype(int(reg)*NumRoles + role)
}

func (a Archetype) Region() Region { return Region(int(a) / NumRoles) }
func (a Archetype) Role() *Role    { return &Roles[int(a)%NumRoles] }

// Reach is how many ties this archetype forms relative to a typical member of
// its own stratum. Centred on 1 by construction; see reachNorm.
func (a Archetype) Reach() float64 {
	r := a.Role()
	if reachNorm[r.Stratum] == 0 {
		return 1
	}
	return r.Reach / reachNorm[r.Stratum]
}

// Values returns the archetype's position on the two survey axes, which is
// what gives homophily something to be homophilous about.
func (a Archetype) Values() (openness, security float64) {
	r := a.Role()
	return r.Openness, r.Security
}

// NumArchetypes is the size of the generation budget: every one of these needs
// content written once.
func NumArchetypes() int { return int(NumRegions) * NumRoles }

// Describe renders the archetype as the sentence the model is asked to write
// about. Kept here rather than in the LLM client so that the prompt and the
// simulated attributes cannot drift apart: whatever the model is told is
// exactly what the engine believes.
func (a Archetype) Describe() string {
	r := a.Role()
	return "A " + r.Name + " in " + Regions[a.Region()].Name + ", who " + r.Sketch + "."
}

func (a Archetype) String() string {
	return a.Role().Name + " / " + Regions[a.Region()].Name
}

// roleWeight is how common a role is in a given region and urbanity.
//
// Exponential in the mismatch rather than a hard filter, because a hard filter
// produces a world with no finance professionals in Lagos and no smallholders
// in France, and both of those exist. Rare should be rare, not absent -- the
// tails are where the interesting personas are.
func roleWeight(role *Role, dev float64, urbanity float64) float64 {
	// dev and urbanity arrive in [0,1]; the biases are in [-1,1].
	d := (dev - 0.5) * 2
	u := (urbanity - 0.5) * 2
	return role.Base * math.Exp(2.2*role.DevBias*d+1.6*role.UrbanBias*u)
}

// urbanityBuckets quantises urbanity for the precomputed weight tables. Sixteen
// levels is far finer than the distinction the weights can actually express,
// and it turns twenty-five exponentials per persona into one table lookup --
// which at ten million people was the difference between 3.1s and 0.9s of
// startup.
const urbanityBuckets = 16

// roleCDF[stratum][region][urbanity bucket] is the cumulative weight over that
// stratum's roles, built once at init.
// pickRole samples a role for a persona, restricted to their stratum.
//
// The stratum is chosen first and the role second, rather than sampling roles
// freely, because the strata carry the importance weights that make the whole
// sample defensible. A media persona must get a media role or the oversampling
// of broadcasters -- the thing that gives the network its degree tail -- would
// silently stop working.
func pickRole(r *rand, st Stratum, reg Region, urbanity float64) int {
	cand := rolesByStratum[st]
	if len(cand) == 0 {
		return 0
	}
	b := int(urbanity * urbanityBuckets)
	if b < 0 {
		b = 0
	}
	if b >= urbanityBuckets {
		b = urbanityBuckets - 1
	}
	cum := roleCDF[st][reg][b]

	p := r.f64() * cum[len(cum)-1]
	for k := range cum {
		if p <= cum[k] {
			return cand[k]
		}
	}
	return cand[len(cand)-1]
}

// rolesByStratum indexes the taxonomy once at startup rather than scanning all
// forty-five roles per persona. The panic is deliberate: the weight scratch
// buffer is a fixed array to keep this allocation-free at ten million calls,
// and a taxonomy that outgrows it should fail loudly at init rather than
// silently dropping the roles that did not fit.
var rolesByStratum [NumStrata][]int

// roleCDF[stratum][region][urbanity bucket] is the cumulative weight over that
// stratum's roles, built once at startup.
var roleCDF [NumStrata][NumRegions][urbanityBuckets][]float64

// reachNorm is the mean Reach within each stratum, so that Archetype.Reach can
// return a multiplier centred on 1. The stratum-level broadcast scale is
// calibrated in GraphConfig and produces the measured degree tail; role Reach
// varies *within* that, letting an influencer out-reach a documentary producer
// without either changing what the stratum as a whole contributes.
var reachNorm [NumStrata]float64

func init() {
	for i := range Roles {
		st := Roles[i].Stratum
		rolesByStratum[st] = append(rolesByStratum[st], i)
	}
	for st := range rolesByStratum {
		var sum float64
		for _, i := range rolesByStratum[st] {
			sum += Roles[i].Reach
		}
		if n := len(rolesByStratum[st]); n > 0 {
			reachNorm[st] = sum / float64(n)
		}
	}
	for st := range rolesByStratum {
		if len(rolesByStratum[st]) > 64 {
			panic("world: more than 64 roles in one stratum; widen the scratch buffer in pickRole")
		}
		if len(rolesByStratum[st]) == 0 {
			panic("world: stratum " + Strata[st].Name + " has no roles, so it can never be described")
		}
		// A zero Base is a role that can never be drawn. Silent in production
		// and invisible in aggregate, so it fails here instead.
		for _, i := range rolesByStratum[st] {
			if Roles[i].Base <= 0 {
				panic("world: role " + Roles[i].Name + " has no base prevalence and can never be sampled")
			}
		}
	}

	// Built here rather than in a second init: this depends on rolesByStratum
	// above, and Go's ordering between two init functions is a fact about
	// source order rather than about the dependency. One init, no question.
	for st := Stratum(0); st < NumStrata; st++ {
		cand := rolesByStratum[st]
		for reg := Region(0); reg < NumRegions; reg++ {
			for b := 0; b < urbanityBuckets; b++ {
				u := (float64(b) + 0.5) / urbanityBuckets
				cum := make([]float64, len(cand))
				acc := 0.0
				for k, i := range cand {
					acc += roleWeight(&Roles[i], Regions[reg].Dev, u)
					cum[k] = acc
				}
				roleCDF[st][reg][b] = cum
			}
		}
	}
}
