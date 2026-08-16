package sim

import (
	"strings"
	"testing"
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
