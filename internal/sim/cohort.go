package sim

import (
	"fmt"
	"sort"

	"github.com/sakshamio/Populace/internal/world"
)

// A cohort is six people who grew up in the same place and still share a group
// chat. It is the bottom rung of the view ladder: world, region, network, chat,
// person.
//
// The point is not decoration. Every aggregate above this -- reach, opinion
// shift, feed share -- is a number about a population, and a number about a
// population is exactly the kind of thing that can look reasonable while the
// mechanism underneath it is wrong. Six named people whose messages are
// generated *from their own simulation state* are a continuous audit: if the
// cascade says 40% reach and nobody in the chat has heard about it, one of the
// two is lying, and the chat is the one you can check by reading.
//
// The chat is also a literal window into the closed channel. Group chat is a
// platform in the media layer with no ranking function, and this is what that
// platform looks like from the inside: no algorithm, no amplification, just six
// people who each bring news in from their own separate networks.
type Cohort struct {
	Origin     string   // the place they grew up
	OriginID   uint16   // place index, for seeding stories where they live
	Friends    []Friend `json:"friends"`
	Messages   []Message
	maxHistory int

	// Last tick's values for these six only; see Observe.
	prevState []uint8
	prevY     []float32
}

// Friend is one member, bound to a real persona in the population. Everything
// shown about them is read from the simulation rather than authored: the role,
// the place, the opinion, and whether they have heard about the story are all
// live state.
type Friend struct {
	ID     int32  `json:"id"`
	Name   string `json:"name"`
	Role   string `json:"role"`
	Place  string `json:"place"`
	Region string `json:"region"`
	Away   bool   `json:"away"` // moved away from the origin

	// Platforms they are on, by name. This is what makes the chat legible as a
	// media experiment: the friend on the short-video feed hears about things
	// days before the one who is only on broadcast.
	Platforms []string `json:"platforms"`

	// Live state, refreshed on every read.
	Opinion float64 `json:"opinion"`
	State   string  `json:"state"`
	Degree  int     `json:"degree"`
	Feed    bool    `json:"feed_reached"`
}

// Message is one line in the chat, caused by something that happened in the
// simulation on that tick.
type Message struct {
	Tick int    `json:"tick"`
	From string `json:"from"`
	Text string `json:"text"`

	// Kind records *why* this line exists, which is what makes the chat an
	// audit rather than a mood piece. Every message traces to a state
	// transition, and the kind names the transition.
	Kind string `json:"kind"`

	Opinion float64 `json:"opinion"`
	Via     string  `json:"via,omitempty"` // platform, when the feed delivered it
}

// NewCohort picks six people who grew up in one place.
//
// Four still live there and two have moved away, which is not flavour: the two
// who left sit in different regions with different platform mixes, so they hear
// about things on a different schedule. A chat where everyone shares a media
// environment would show one arrival time and teach nothing about how news
// actually travels between places.
func NewCohort(w *world.World, g *Graph, m *Media, seed uint64) *Cohort {
	if w.N < 64 {
		return nil
	}
	r := newRNG(seed, 0xC0405)

	// Pick an origin: a well-populated place, found by sampling rather than by
	// scanning, because the population is sorted spatially and a scan would
	// always land in the same continent.
	var origin uint16
	bestCount := -1
	for try := 0; try < 24; try++ {
		cand := w.Place[int(r.u32())%w.N]
		n := 0
		for k := 0; k < 512; k++ {
			if w.Place[int(r.u32())%w.N] == cand {
				n++
			}
		}
		if n > bestCount {
			bestCount, origin = n, cand
		}
	}

	// Locals: four people who live in the origin place, drawn by reservoir
	// sampling rather than by taking the first four matches.
	//
	// Taking the first four looked equivalent and was not. Roles are assigned
	// partly from urbanity, and index order is not independent of it, so the
	// first four matches in Mumbai came back as three smallholder farmers and a
	// pastoral herder -- a real draw from a biased corner of the place rather
	// than a representative one. The chat is supposed to be the readable check
	// on the aggregates, which it cannot be if its own sample is skewed.
	locals := make([]int32, 0, 4)
	seen := 0
	for i := 0; i < w.N; i++ {
		if w.Place[i] != origin || world.Stratum(w.Strat[i]) == world.Media {
			continue
		}
		seen++
		if len(locals) < 4 {
			locals = append(locals, int32(i))
		} else if k := int(r.u32()) % seen; k < 4 {
			locals[k] = int32(i)
		}
	}
	var movers []int32
	// Movers: same generation, different region. Sampled at random across the
	// world so the two who left are genuinely elsewhere.
	_, originRegion, _, _ := world.PlaceInfo(int(origin))
	for try := 0; try < 4000 && len(movers) < 2; try++ {
		i := int32(r.u32()) % int32(w.N)
		if i < 0 {
			i = -i
		}
		if world.Stratum(w.Strat[i]) == world.Media {
			continue
		}
		if _, reg, _, _ := world.PlaceInfo(int(w.Place[i])); reg == originRegion {
			continue
		}
		movers = append(movers, i)
	}
	if len(locals) < 4 || len(movers) < 2 {
		return nil
	}

	name, _, _, _ := world.PlaceInfo(int(origin))
	c := &Cohort{Origin: name, OriginID: origin, maxHistory: 400}

	// Names must be unique inside the chat. Drawing independently per persona
	// produced two people called Chen in the same six-person conversation,
	// which makes the transcript unreadable at exactly the moment it is
	// supposed to be the legible view.
	taken := map[string]bool{}
	add := func(id int32, away bool) {
		a := world.Archetype(w.Arch[id])
		pn, reg, _, _ := world.PlaceInfo(int(w.Place[id]))
		f := Friend{
			ID:     id,
			Name:   uniqueName(reg, uint64(id), taken),
			Role:   a.Role().Name,
			Place:  pn,
			Region: reg.String(),
			Away:   away,
			Degree: g.Degree(int(id)),
		}
		if m != nil {
			for p := range m.Platforms {
				if m.Member[id]&(1<<uint(p)) != 0 {
					f.Platforms = append(f.Platforms, m.Platforms[p].Name)
				}
			}
		}
		c.Friends = append(c.Friends, f)
	}
	for _, id := range locals {
		add(id, false)
	}
	for _, id := range movers {
		add(id, true)
	}

	// Stable order, so the chat reads the same way twice.
	sort.Slice(c.Friends, func(i, j int) bool { return c.Friends[i].ID < c.Friends[j].ID })
	return c
}

// Refresh reads live state back onto each friend.
func (c *Cohort) Refresh(w *world.World, s *Sim) {
	if c == nil {
		return
	}
	for i := range c.Friends {
		id := c.Friends[i].ID
		c.Friends[i].Opinion = float64(s.O.Y[id])
		c.Friends[i].State = stateName(s.C.State[id])
		c.Friends[i].Feed = s.M.Enabled() && s.M.Exposed[id]
	}
}

func stateName(st uint8) string {
	switch st {
	case Adopted:
		return "talking about it"
	case Fatigued:
		return "moved on"
	default:
		return "hasn't heard"
	}
}

// Observe is called once per tick. It turns state transitions into chat.
//
// Every branch here is a transition, not a mood: somebody just adopted,
// somebody's opinion moved past a threshold, somebody was reached by a feed.
// A message with no transition behind it would make the chat decorative, and a
// decorative chat cannot be used to check the aggregates -- which is the only
// reason it exists.
//
// The previous tick's values are kept here, six of each, rather than by
// snapshotting the population. Copying State and Y every tick to diff six
// people would cost 50 MB of memory traffic per tick at ten million personas,
// to answer a question about 0.00006% of them.
func (c *Cohort) Observe(s *Sim) {
	if c == nil || len(c.Friends) == 0 {
		return
	}
	if c.prevState == nil {
		c.prevState = make([]uint8, len(c.Friends))
		c.prevY = make([]float32, len(c.Friends))
		for i, f := range c.Friends {
			c.prevState[i] = s.C.State[f.ID]
			c.prevY[i] = s.O.Y[f.ID]
		}
		return // nothing to diff against on the first tick
	}

	for i := range c.Friends {
		f := &c.Friends[i]
		id := f.ID
		now, was := s.C.State[id], c.prevState[i]
		y, py := float64(s.O.Y[id]), float64(c.prevY[i])
		c.prevState[i], c.prevY[i] = now, s.O.Y[id]

		switch {
		case was == Unaware && now == Adopted:
			via := ""
			if s.M.Enabled() && s.C.FeedDriven != nil && s.C.FeedDriven[id] {
				via = c.loudestPlatform(s.M, id)
			}
			c.say(s.Tick, f, arrivalLine(f, y, via), "heard", y, via)

		case was == Adopted && now == Fatigued:
			// Only some people announce that they are done with a thing.
			if hash32(uint64(id)^uint64(s.Tick))%3 == 0 {
				c.say(s.Tick, f, tiredLine(f), "moved-on", y, "")
			}

		case now != Unaware && sign(y) != sign(py) && absF(y-py) > 0.04:
			c.say(s.Tick, f, turnedLine(f, y), "changed-mind", y, "")

		case now != Unaware && absF(y-py) > 0.08:
			c.say(s.Tick, f, strengthenLine(f, y, y > py), "dug-in", y, "")
		}
	}
}

// loudestPlatform names the platform most likely to have delivered the story to
// this person: the one they are on with the highest presence right now.
func (c *Cohort) loudestPlatform(m *Media, id int32) string {
	best, bestP := "", 0.0
	for p := range m.Platforms {
		if m.Member[id]&(1<<uint(p)) == 0 {
			continue
		}
		if m.Presence[p] > bestP {
			best, bestP = m.Platforms[p].Name, m.Presence[p]
		}
	}
	return best
}

func (c *Cohort) say(tick int, f *Friend, text, kind string, y float64, via string) {
	c.Messages = append(c.Messages, Message{
		Tick: tick, From: f.Name, Text: text, Kind: kind, Opinion: y, Via: via,
	})
	if len(c.Messages) > c.maxHistory {
		c.Messages = c.Messages[len(c.Messages)-c.maxHistory:]
	}
}

// Reset clears the conversation but keeps the people. They do not stop being
// friends because a new story broke.
func (c *Cohort) Reset() {
	if c != nil {
		c.Messages = c.Messages[:0]
		// Drop the diff baseline too. Keeping it would make the first tick
		// after a reset compare against the old story's state and open the new
		// conversation with six people reacting to news that no longer exists.
		c.prevState, c.prevY = nil, nil
	}
}

// The lines below are deterministic and state-driven. They are intentionally
// plain: this is the fallback that always works, including with no model
// gateway configured. When the relay is up, /api/cohort/generate replaces them
// with model-authored dialogue for the same transitions.

// pick chooses a variant by persona id, so a given friend keeps a consistent
// voice across a run instead of drawing a fresh register every time they speak.
// Six people delivering the same sentence is what the first version did, and it
// read as one narrator with six labels rather than as six people.
func pick(f *Friend, salt uint64, opts []string) string {
	return opts[hash32(uint64(f.ID)*0x9E3779B1^salt)%uint32(len(opts))]
}

func arrivalLine(f *Friend, y float64, via string) string {
	if via != "" {
		switch {
		case y < -0.25:
			return fmt.Sprintf(pick(f, 1, []string{
				"this is all over %s and I hate it",
				"why is %s nothing but this today",
				"%s has completely lost it over this",
			}), via)
		case y > 0.25:
			return fmt.Sprintf(pick(f, 2, []string{
				"ok %s is full of this and honestly? good",
				"finally seeing this on %s",
				"%s is actually right about this one",
			}), via)
		default:
			return fmt.Sprintf(pick(f, 3, []string{
				"seeing this everywhere on %s — is it real",
				"%s keeps pushing this at me. anyone know more",
				"got this off %s, no idea if it's true",
			}), via)
		}
	}
	switch {
	case y < -0.25:
		return pick(f, 4, []string{
			"did you all see this. not great",
			"well this is bleak",
			"saw the news. not thrilled",
		})
	case y > 0.25:
		return pick(f, 5, []string{
			"finally some good news",
			"ok this one I like",
			"about time honestly",
		})
	default:
		return pick(f, 6, []string{
			"just heard about this",
			"anyone else seen this",
			"catching up on this now",
		})
	}
}

func turnedLine(f *Friend, y float64) string {
	if y > 0 {
		return pick(f, 7, []string{
			"ok I've come round on this actually",
			"fine, you were right",
			"rethought this one",
		})
	}
	return pick(f, 8, []string{
		"changed my mind, this is worse than I thought",
		"no, I've gone off this",
		"took it back. not good",
	})
}

func strengthenLine(f *Friend, y float64, up bool) string {
	if up {
		return pick(f, 9, []string{
			"more of you should be paying attention to this",
			"still think this is the right call",
			"genuinely glad about this",
		})
	}
	return pick(f, 10, []string{
		"the more I read the less I like it",
		"this keeps getting worse",
		"really not sitting right with me",
	})
}

func tiredLine(f *Friend) string {
	return pick(f, 11, []string{
		"anyway. what are we doing this weekend",
		"ok I'm done with this topic",
		"changing the subject — anyone free sunday",
		"enough of this. how's your mum",
	})
}

// givenName picks a plausible given name for a region.
//
// Names are drawn per region rather than from one global list because a chat
// where six people who grew up in Lagos are all called Emma reads as a
// simulation of somewhere else. The lists are short and obviously incomplete;
// they exist to avoid a worse default, not to represent anywhere properly.
func givenName(r world.Region, id uint64) string {
	list := givenNames[r]
	if len(list) == 0 {
		list = givenNames[world.Europe]
	}
	return list[hash32(id*0x9E3779B1)%uint32(len(list))]
}

// uniqueName is givenName with collision avoidance inside one cohort. It walks
// the region's list from the drawn position, then falls back across regions, so
// a small name list cannot produce two people with the same name in a chat of
// six. Deterministic: the walk order is fixed by the persona id.
func uniqueName(r world.Region, id uint64, taken map[string]bool) string {
	list := givenNames[r]
	if len(list) == 0 {
		list = givenNames[world.Europe]
	}
	start := int(hash32(id*0x9E3779B1) % uint32(len(list)))
	for k := 0; k < len(list); k++ {
		if n := list[(start+k)%len(list)]; !taken[n] {
			taken[n] = true
			return n
		}
	}
	// Every name in the region is already in use by this cohort, which needs a
	// region with fewer than six names to happen. Fall through the other lists
	// rather than returning a duplicate.
	for reg := world.Region(0); reg < world.NumRegions; reg++ {
		for _, n := range givenNames[reg] {
			if !taken[n] {
				taken[n] = true
				return n
			}
		}
	}
	return "?"
}

var givenNames = map[world.Region][]string{
	world.SubSaharanAfrica: {"Amara", "Kwame", "Chidi", "Thandiwe", "Sipho", "Ngozi", "Kofi", "Zanele", "Obi", "Aisha"},
	world.MENA:             {"Layla", "Omar", "Yasmin", "Karim", "Rania", "Tarek", "Nour", "Hakim", "Salma", "Idris"},
	world.SouthAsia:        {"Priya", "Arjun", "Meera", "Rohan", "Ananya", "Vikram", "Divya", "Imran", "Kavya", "Sanjay"},
	world.EastAsia:         {"Mei", "Haruki", "Jin", "Yuna", "Wei", "Sora", "Ling", "Takeshi", "Ji-woo", "Chen"},
	world.SoutheastAsia:    {"Anh", "Siti", "Rizal", "Mai", "Bayu", "Linh", "Arif", "Dewi", "Thura", "Nadia"},
	world.Europe:           {"Elena", "Mateusz", "Sofia", "Lukas", "Ines", "Niamh", "Andrei", "Freya", "Marco", "Hana"},
	world.Eurasia:          {"Dmitri", "Aigul", "Nadia", "Ruslan", "Zarina", "Timur", "Alina", "Bekzat", "Leyla", "Artem"},
	world.NorthAmerica:     {"Maya", "Dante", "Riley", "Jonah", "Camila", "Ethan", "Nia", "Owen", "Sadie", "Marcus"},
	world.LatinAmerica:     {"Valentina", "Mateo", "Luciana", "Diego", "Camila", "Tomás", "Paula", "Rafael", "Sol", "Nicolás"},
	world.Oceania:          {"Tane", "Ariana", "Jarrah", "Moana", "Kai", "Sione", "Talia", "Lachlan", "Hine", "Ruben"},
}

func hash32(x uint64) uint32 {
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	return uint32(x)
}

func sign(v float64) int {
	if v < 0 {
		return -1
	}
	if v > 0 {
		return 1
	}
	return 0
}

func absF(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
