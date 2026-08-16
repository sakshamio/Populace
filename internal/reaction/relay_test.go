package reaction

import (
	"strings"
	"testing"
)

// Models wrap JSON in prose or fences often enough that a strict unmarshal on
// the whole body throws away answers that are perfectly good. Every case here
// is a shape a real model has produced.
func TestParseRecoversRealisticModelOutput(t *testing.T) {
	cases := []struct {
		name, in string
		want     float32
	}{
		{"bare", `{"stance":-0.6,"salience":0.4,"share":0.2,"line":"Not my problem."}`, -0.6},
		{"fenced", "```json\n{\"stance\":0.5,\"salience\":0.3,\"share\":0.1,\"line\":\"Fine by me.\"}\n```", 0.5},
		{"preamble", "Here is the JSON:\n{\"stance\":0.1,\"salience\":0.2,\"share\":0.3,\"line\":\"Whatever.\"}", 0.1},
		{"trailing", `{"stance":0,"salience":0.9,"share":0.8,"line":"Big if true."} Hope that helps!`, 0},
	}
	for _, c := range cases {
		got, err := parse(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got.Stance != c.want {
			t.Fatalf("%s: stance %v, want %v", c.name, got.Stance, c.want)
		}
		if got.Line == "" {
			t.Fatalf("%s: no line", c.name)
		}
	}
}

// Clamping rather than rejecting: a model returning 1.5 meant "very much in
// favour", and discarding the whole reaction over a formatting slip throws away
// a good answer and leaves that archetype with no opinion at all.
func TestParseClampsRatherThanRejects(t *testing.T) {
	got, err := parse(`{"stance":1.8,"salience":-0.2,"share":4,"line":"Outrageous."}`)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stance != 1 || got.Salience != 0 || got.Share != 1 {
		t.Fatalf("not clamped: stance=%v salience=%v share=%v",
			got.Stance, got.Salience, got.Share)
	}
}

// A reaction with no line is useless to the UI and suspicious as data, so it
// counts as a failure rather than being cached as an empty opinion.
func TestParseRejectsUnusableOutput(t *testing.T) {
	for _, in := range []string{
		"I'm sorry, I can't help with that.",
		`{"stance":0.5,"salience":0.5,"share":0.5,"line":"   "}`,
		`{"stance": not-a-number}`,
	} {
		if _, err := parse(in); err == nil {
			t.Fatalf("accepted unusable output %q", in)
		}
	}
}

// The cache key must move with the story. Keyed on archetype alone it would
// serve last week's reaction to this week's news, and nothing downstream would
// look wrong.
func TestCacheKeyTracksTheStory(t *testing.T) {
	a := Story{Headline: "Rates held", Stance: -0.2}
	b := Story{Headline: "Rates held", Stance: 0.6}
	c := Story{Headline: "Rates cut", Stance: -0.2}
	if a.fingerprint() == b.fingerprint() {
		t.Fatal("framing the same headline differently reuses the same cache entry")
	}
	if a.fingerprint() == c.fingerprint() {
		t.Fatal("a different headline reuses the same cache entry")
	}
	if a.fingerprint() != (Story{Headline: "Rates held", Stance: -0.2}).fingerprint() {
		t.Fatal("the same story does not hit its own cache entry")
	}
}

// The prompt is the contract with the model. If it stops naming the person's
// work and region, the engine is still applying the answer as though it had.
func TestPromptCarriesTheArchetype(t *testing.T) {
	p := userPrompt(0, Story{Headline: "Port strike enters a second week"})
	if !strings.Contains(p, "Port strike") {
		t.Fatalf("prompt omits the news: %q", p)
	}
	if !strings.Contains(strings.ToLower(p), "who") {
		t.Fatalf("prompt omits the archetype description: %q", p)
	}
	t.Log(p)
}

// Observed twice in a 450-archetype production run: the model escaped the
// quotes delimiting its own string value. Repairing only the delimiters
// recovers it without touching escapes inside the sentence.
func TestParseRepairsOverEscapedDelimiters(t *testing.T) {
	in := `{"stance": 0.1, "salience": 0.2, "share": 0.1, "line": \"I'm watching to see if this triggers a liquidity crunch.\"}`
	got, err := parse(in)
	if err != nil {
		t.Fatalf("did not recover: %v", err)
	}
	if !strings.Contains(got.Line, "liquidity crunch") {
		t.Fatalf("line came back wrong: %q", got.Line)
	}
	t.Logf("recovered: %q", got.Line)
}

// The repair must not eat escapes that belong to the sentence.
func TestRepairLeavesInteriorQuotesAlone(t *testing.T) {
	in := `{"stance":0,"salience":0.5,"share":0.2,"line":"They call it a \"review\" but nothing changes."}`
	got, err := parse(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Line, `"review"`) {
		t.Fatalf("interior quotes lost: %q", got.Line)
	}
}
