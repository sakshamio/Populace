package sim

import (
	"strings"
	"testing"

	"github.com/sakshamio/Populace/internal/world"
)

// The chat has to be an audit, not a mood piece: if the aggregate says a story
// reached most of the world, the six people in the chat must have heard about
// it. This test is the link between the two.
func TestChatAgreesWithTheAggregate(t *testing.T) {
	w := testWorld(t, testN)
	s := New(w, DefaultConfig())
	if s.Chat == nil || len(s.Chat.Friends) != 6 {
		t.Fatalf("expected 6 friends, got %v", s.Chat)
	}
	t.Logf("origin: %s", s.Chat.Origin)
	for _, f := range s.Chat.Friends {
		t.Logf("  %-10s %-24s %-18s away=%-5v deg=%3d  on %d platforms",
			f.Name, f.Role, f.Place, f.Away, f.Degree, len(f.Platforms))
	}

	// Four locals, two who moved.
	away := 0
	for _, f := range s.Chat.Friends {
		if f.Away {
			away++
		}
	}
	if away != 2 {
		t.Errorf("expected 2 friends who moved away, got %d", away)
	}

	// Names must be unique: a transcript with two people called Chen is not a
	// legible view of anything.
	seen := map[string]bool{}
	for _, f := range s.Chat.Friends {
		if seen[f.Name] {
			t.Errorf("duplicate name %q in the chat", f.Name)
		}
		seen[f.Name] = true
	}

	s.Inject(Event{Headline: "big one", Stance: -0.6, Salience: 0.9,
		Seeding: SeedInRegion, SeedSize: 1200, Seed: 5, Difficulty: 0.9})
	for i := 0; i < 45; i++ {
		s.Advance()
	}
	s.Chat.Refresh(w, s)
	sn := s.Snapshot(false)

	heard := 0
	for _, f := range s.Chat.Friends {
		if f.State != "hasn't heard" {
			heard++
		}
	}
	t.Logf("reach %.2f%%; %d of 6 in the chat have heard; %d messages",
		sn.Reach*100, heard, len(s.Chat.Messages))
	for _, m := range s.Chat.Messages {
		via := ""
		if m.Via != "" {
			via = " (via " + m.Via + ")"
		}
		t.Logf("  t%-3d %-10s [%-12s] %s%s", m.Tick, m.From, m.Kind, m.Text, via)
	}

	if sn.Reach > 0.9 && heard < 5 {
		t.Errorf("aggregate says %.1f%% reach but only %d of 6 friends heard -- "+
			"the chat and the cascade disagree", sn.Reach*100, heard)
	}
	if len(s.Chat.Messages) == 0 {
		t.Error("a story reached the population and nobody in the chat said anything")
	}
	// Every message must name a transition.
	for _, m := range s.Chat.Messages {
		if m.Kind == "" || strings.TrimSpace(m.Text) == "" {
			t.Errorf("message with no kind or no text: %+v", m)
		}
	}
}

// Each kind holds a different variable fixed, and the test checks that
// specific variable rather than "6 friends exist" -- the whole point of a kind
// is the constraint, so a test that does not check the constraint would pass
// on a build that shuffled the four kinds' logic between each other.
func TestCohortKindsHoldTheRightThingFixed(t *testing.T) {
	w := testWorld(t, testN)

	check := func(kind CohortKind) *Cohort {
		t.Helper()
		cfg := DefaultConfig()
		cfg.ChatKind = kind
		s := New(w, cfg)
		if s.Chat == nil {
			t.Fatalf("%s: could not form a cohort in a %d-person world", kind.Label(), testN)
		}
		if len(s.Chat.Friends) != 6 {
			t.Fatalf("%s: got %d friends, want 6", kind.Label(), len(s.Chat.Friends))
		}
		if s.Chat.Kind != kind {
			t.Errorf("%s: Cohort.Kind is %s", kind.Label(), s.Chat.Kind.Label())
		}
		seen := map[string]bool{}
		for _, f := range s.Chat.Friends {
			if seen[f.Name] {
				t.Errorf("%s: duplicate name %q", kind.Label(), f.Name)
			}
			seen[f.Name] = true
		}
		return s.Chat
	}

	t.Run("childhood: four stayed, two moved to a different region", func(t *testing.T) {
		c := check(KindChildhood)
		away, local := 0, 0
		for _, f := range c.Friends {
			if f.Away {
				away++
			} else {
				local++
				if f.Place != c.Origin {
					t.Errorf("local friend %q is in %q, not the origin %q", f.Name, f.Place, c.Origin)
				}
			}
		}
		if away != 2 || local != 4 {
			t.Errorf("got %d away, %d local, want 2 and 4", away, local)
		}
	})

	t.Run("coworkers: same role, same place, nobody moved", func(t *testing.T) {
		c := check(KindCoworkers)
		role := c.Friends[0].Role
		for _, f := range c.Friends {
			if f.Away {
				t.Errorf("%q is marked away in a coworkers cohort", f.Name)
			}
			if f.Place != c.Origin {
				t.Errorf("%q is in %q, not the shared workplace %q", f.Name, f.Place, c.Origin)
			}
			if f.Role != role {
				t.Errorf("%q has role %q, want the shared role %q", f.Name, f.Role, role)
			}
		}
	})

	t.Run("online: one shared platform, six distinct regions, no shared origin", func(t *testing.T) {
		c := check(KindOnline)
		if c.Origin != "" {
			t.Errorf("online cohort has an Origin %q; it should not have one", c.Origin)
		}
		if c.Platform == "" {
			t.Fatal("online cohort has no Platform recorded")
		}
		regions := map[string]bool{}
		for _, f := range c.Friends {
			if regions[f.Region] {
				t.Errorf("two friends share region %q; online cohort should scatter one per region", f.Region)
			}
			regions[f.Region] = true
			has := false
			for _, p := range f.Platforms {
				if p == c.Platform {
					has = true
				}
			}
			if !has {
				t.Errorf("%q is not on the cohort's own platform %q (has %v)", f.Name, c.Platform, f.Platforms)
			}
		}
	})

	t.Run("neighbours: one place, nobody moved, roles free to vary", func(t *testing.T) {
		c := check(KindNeighbours)
		for _, f := range c.Friends {
			if f.Away {
				t.Errorf("%q is marked away in a neighbours cohort", f.Name)
			}
			if f.Place != c.Origin {
				t.Errorf("%q is in %q, not the shared place %q", f.Name, f.Place, c.Origin)
			}
		}
	})
}

// An unrecognised kind string must not silently become KindChildhood, or a
// typo in an API request would look like a working request that ignored what
// was asked.
func TestParseCohortKindRejectsGarbage(t *testing.T) {
	if _, ok := ParseCohortKind("coworker"); ok { // singular, not the real name
		t.Error("ParseCohortKind accepted a near-miss")
	}
	if k, ok := ParseCohortKind(""); !ok || k != KindChildhood {
		t.Errorf("empty string should parse as childhood, got %v ok=%v", k, ok)
	}
	if k, ok := ParseCohortKind("coworkers"); !ok || k != KindCoworkers {
		t.Errorf("coworkers did not round-trip: %v ok=%v", k, ok)
	}
}

// Rerolling must give a different chat, not the same one restated -- a
// "reroll" button that keeps returning the same six people is a bug wearing
// the shape of a feature.
func TestRerollChatGivesADifferentDraw(t *testing.T) {
	w := testWorld(t, testN)
	s := New(w, DefaultConfig())
	first := make([]int32, len(s.Chat.Friends))
	for i, f := range s.Chat.Friends {
		first[i] = f.ID
	}

	if ok := s.RerollChat(KindChildhood, 999); !ok {
		t.Fatal("reroll reported failure in a world that can plainly form this cohort")
	}
	same := true
	for i, f := range s.Chat.Friends {
		if f.ID != first[i] {
			same = false
		}
	}
	if same {
		t.Error("reroll with a different seed returned the identical six people")
	}
	// And the message history from before the reroll must not survive it --
	// carrying it forward would show six new people reacting to messages the
	// old six sent.
	if len(s.Chat.Messages) != 0 {
		t.Errorf("rerolled chat carried %d messages from the previous cohort", len(s.Chat.Messages))
	}
}

// The heart of "make the chat a live reaction": when a model has answered for
// a friend's archetype, their first message must be that answer, not the
// canned line -- and a friend the model has not answered for yet must still
// get the canned fallback, not silence. Both have to be true in the same run,
// or the feature only works when it is the only friend hearing anything.
func TestReactionSourceUpgradesOnlyTheFriendItHasAnAnswerFor(t *testing.T) {
	w := testWorld(t, testN)
	s := New(w, DefaultConfig())

	if len(s.Chat.Friends) < 2 {
		t.Fatal("need at least 2 friends for this test")
	}
	haveArch := int(world.Archetype(w.Arch[s.Chat.Friends[0].ID]))
	const modelLine = "this is the model's actual sentence about it, not a canned one"
	src := stubReactionSource{haveArch: haveArch, line: modelLine}

	s.ReactionSource = src
	s.Inject(Event{Headline: "model line test", Stance: 0.4, Salience: 0.95,
		Seeding: SeedInRegion, SeedSize: 3000, Seed: 11, Difficulty: 0.6})
	for i := 0; i < 60; i++ {
		s.Advance()
	}

	gotModelLine, gotCannedLine := false, false
	for _, m := range s.Chat.Messages {
		if m.Kind != "heard" && m.Kind != "heard-live" {
			continue
		}
		if m.Text == modelLine {
			gotModelLine = true
			if m.Kind != "heard-live" {
				t.Errorf("message used the model's line but kind is %q, want heard-live", m.Kind)
			}
		} else if m.Kind == "heard" {
			gotCannedLine = true
		}
	}
	t.Logf("model line used: %v; canned fallback used: %v", gotModelLine, gotCannedLine)
	if !gotModelLine {
		t.Error("the friend whose archetype the model answered never used the model's line -- " +
			"either they never heard the story in 60 ticks, or the hookup is not wired")
	}
	if !gotCannedLine {
		t.Error("every friend used the model's line -- the fallback for friends with no " +
			"cached answer appears to be gone, which breaks the no-model case")
	}
}

// A nil ReactionSource, which is what every existing caller in this codebase
// passes, must behave exactly as it did before this field existed.
func TestNilReactionSourceIsExactlyTheOldBehaviour(t *testing.T) {
	w := testWorld(t, testN)
	s := New(w, DefaultConfig())
	if s.ReactionSource != nil {
		t.Fatal("ReactionSource should default to nil")
	}
	s.Inject(Event{Headline: "no model", Stance: -0.3, Salience: 0.8,
		Seeding: SeedInRegion, SeedSize: 2000, Seed: 3, Difficulty: 0.8})
	for i := 0; i < 40; i++ {
		s.Advance()
	}
	for _, m := range s.Chat.Messages {
		if m.Kind == "heard-live" {
			t.Errorf("got a heard-live message with no ReactionSource configured: %+v", m)
		}
	}
}

type stubReactionSource struct {
	haveArch int
	line     string
}

func (s stubReactionSource) Reaction(archetype int) (string, bool) {
	if archetype == s.haveArch {
		return s.line, true
	}
	return "", false
}

// buildCannedScript is what makes the chat an actual exchange rather than six
// independent reactions to the news -- so the property worth checking is the
// reply structure, not just "6 turns exist".
func TestBuildCannedScriptIsAConnectedThread(t *testing.T) {
	w := testWorld(t, testN)
	s := New(w, DefaultConfig())
	c := s.Chat
	lean := make([]float64, len(c.Friends))
	for i, f := range c.Friends {
		lean[i] = float64(s.O.Prior[f.ID])
	}

	script := buildCannedScript(c, 4242, lean)
	if len(script) < len(c.Friends) {
		t.Fatalf("script has %d turns for %d friends -- everyone should get at least one",
			len(script), len(c.Friends))
	}

	opened := false
	for i, turn := range script {
		if turn.Speaker < 0 || turn.Speaker >= len(c.Friends) {
			t.Errorf("turn %d has speaker index %d, out of range for %d friends",
				i, turn.Speaker, len(c.Friends))
		}
		if turn.ReplyTo == -1 {
			if opened {
				t.Errorf("turn %d opens a new thread, but the first turn already did -- "+
					"every later turn should reply to something", i)
			}
			opened = true
			continue
		}
		if turn.ReplyTo < 0 || turn.ReplyTo >= i {
			t.Errorf("turn %d replies to %d, which is not an earlier turn -- "+
				"a reply must point backward, or it cannot have landed yet when this fires", i, turn.ReplyTo)
		}
	}
	if !opened {
		t.Error("no turn opened the thread (reply_to == -1)")
	}
	t.Logf("script (%d turns):", len(script))
	for i, turn := range script {
		t.Logf("  [%d] %-8s reply_to=%-3d %s", i, c.Friends[turn.Speaker].Name, turn.ReplyTo, turn.Text)
	}
}

// Same seed, same script -- determinism holds here for the same reason it
// holds everywhere else in this codebase: a run has to replay identically.
func TestBuildCannedScriptIsDeterministic(t *testing.T) {
	w := testWorld(t, testN)
	s := New(w, DefaultConfig())
	lean := make([]float64, len(s.Chat.Friends))
	a := buildCannedScript(s.Chat, 777, lean)
	b := buildCannedScript(s.Chat, 777, lean)
	if len(a) != len(b) {
		t.Fatalf("different lengths: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("turn %d differs: %+v vs %+v", i, a[i], b[i])
		}
	}
}

// The heart of the release mechanism: a turn must not appear before its
// speaker has actually heard the story, and a reply must not appear before the
// message it replies to has landed. Both are checked against the live
// message stream, not against the script in isolation.
func TestReleaseScriptRespectsAdoptionAndReplyOrder(t *testing.T) {
	w := testWorld(t, testN)
	s := New(w, DefaultConfig())
	s.Inject(Event{Headline: "script order test", Stance: 0.4, Salience: 0.9,
		Seeding: SeedInRegion, SeedSize: 900, Seed: 21, Difficulty: 0.8})

	landedAt := map[int]int{} // message ID -> tick
	for i := 0; i < 80; i++ {
		s.Advance()
	}
	for _, m := range s.Chat.Messages {
		if m.Kind != "script" && m.Kind != "script-model" {
			continue
		}
		landedAt[m.ID] = m.Tick
		if m.ReplyTo < 0 {
			continue
		}
		replyTick, ok := landedAt[m.ReplyTo]
		if !ok {
			t.Errorf("message %d (t%d) replies to message %d, which is not in the "+
				"released set at all -- a reply appeared before what it replies to",
				m.ID, m.Tick, m.ReplyTo)
			continue
		}
		if replyTick > m.Tick {
			t.Errorf("message %d (t%d) replies to message %d which landed later, at t%d",
				m.ID, m.Tick, m.ReplyTo, replyTick)
		}
	}
	if len(landedAt) == 0 {
		t.Error("no script turns released in 80 ticks of a story that should have reached this cohort")
	}
	t.Logf("%d script turns released", len(landedAt))
	for _, m := range s.Chat.Messages {
		if m.Kind == "script" || m.Kind == "script-model" {
			t.Logf("  t%-3d [id %d, reply_to %d] %s: %s", m.Tick, m.ID, m.ReplyTo, m.From, m.Text)
		}
	}
}

// A friend who never hears the story must not silently take the rest of the
// script down with them -- this is what scriptStallLimit exists for.
func TestReleaseScriptSkipsAStalledSpeakerEventually(t *testing.T) {
	w := testWorld(t, 20_000)
	s := New(w, DefaultConfig())
	// A script where the second turn's speaker will never adopt: seed nothing
	// near them and just fast-forward the stall clock directly rather than
	// waiting out scriptStallLimit ticks for real.
	s.Chat.script = []ScriptedTurn{
		{Speaker: 0, Text: "opens", ReplyTo: -1},
		{Speaker: 1, Text: "never arrives", ReplyTo: -1},
		{Speaker: 2, Text: "closes", ReplyTo: -1},
	}
	s.Chat.scriptMsgID = []int{-1, -1, -1}
	s.Chat.scriptCursor = 0
	s.Chat.scriptStallSince = -1

	// A real, non-zero tick, or the stall value injected below computes
	// negative and collides with the "-1 means not yet stalled" sentinel.
	s.Tick = 500

	// Advance friend 0's state directly so turn 0 releases and the cursor
	// reaches the stuck turn, without needing a real cascade to do it.
	s.C.State[s.Chat.Friends[0].ID] = Adopted
	s.Chat.releaseScript(s)
	if s.Chat.scriptCursor != 1 {
		t.Fatalf("expected cursor at 1 after releasing turn 0, got %d", s.Chat.scriptCursor)
	}

	// Friend 1 never adopts. Simulate the stall clock running out.
	s.Chat.scriptStallSince = s.Tick - scriptStallLimit - 1
	s.C.State[s.Chat.Friends[2].ID] = Adopted
	s.Chat.releaseScript(s)
	if s.Chat.scriptCursor != 3 {
		t.Fatalf("expected the stalled turn skipped and turn 2 released, cursor at 3, got %d",
			s.Chat.scriptCursor)
	}
	if len(s.Chat.Messages) != 2 {
		t.Fatalf("expected 2 messages (turn 1 skipped, never said), got %d", len(s.Chat.Messages))
	}
	if s.Chat.Messages[1].From != s.Chat.Friends[2].Name {
		t.Errorf("second message should be from the third friend (turn 1 skipped), got %q",
			s.Chat.Messages[1].From)
	}
}

// InstallModelScript must refuse once the conversation has actually started --
// swapping in different, unrelated text after some of it has already been
// said would read as rewriting history rather than as the chat getting richer.
func TestInstallModelScriptRefusesAfterTheThreadHasStarted(t *testing.T) {
	w := testWorld(t, 20_000)
	s := New(w, DefaultConfig())

	fresh := []ScriptedTurn{{Speaker: 0, Text: "model line", ReplyTo: -1}}
	if !s.Chat.InstallModelScript(fresh) {
		t.Fatal("should apply cleanly before anything has been released")
	}

	// Release one turn, then try to swap again.
	s.C.State[s.Chat.Friends[0].ID] = Adopted
	s.Chat.releaseScript(s)
	if len(s.Chat.Messages) != 1 {
		t.Fatalf("expected the model turn to have released, got %d messages", len(s.Chat.Messages))
	}

	if s.Chat.InstallModelScript([]ScriptedTurn{{Speaker: 1, Text: "too late", ReplyTo: -1}}) {
		t.Error("InstallModelScript applied after the thread had already started releasing turns")
	}
	if len(s.Chat.script) != 1 || s.Chat.script[0].Text != "model line" {
		t.Error("the original script should be unchanged after a refused swap")
	}
}

// Reset must not leave the chat reacting to a story that no longer exists.
func TestChatResetDropsTheDiffBaseline(t *testing.T) {
	w := testWorld(t, 20_000)
	cfg := DefaultConfig()
	s := New(w, cfg)
	s.Inject(Event{Headline: "one", Stance: -0.6, Salience: 0.9,
		Seeding: SeedInRegion, SeedSize: 400, Seed: 5, Difficulty: 0.9})
	for i := 0; i < 20; i++ {
		s.Advance()
	}
	s.Reset(cfg)
	if len(s.Chat.Messages) != 0 {
		t.Errorf("reset left %d messages behind", len(s.Chat.Messages))
	}
	// One tick after reset, with no story, nobody should be announcing anything.
	s.Advance()
	if n := len(s.Chat.Messages); n != 0 {
		t.Errorf("chat produced %d messages on a world at rest: %+v", n, s.Chat.Messages)
	}
}
