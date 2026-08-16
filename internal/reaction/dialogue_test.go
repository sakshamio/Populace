package reaction

import "testing"

// The ordinary case: valid speakers, reply_to pointing backward.
func TestParseDialogueRecoversAWellFormedExchange(t *testing.T) {
	raw := `{"turns":[
		{"speaker":0,"reply_to":-1,"text":"did everyone see this"},
		{"speaker":1,"reply_to":0,"text":"yeah, not loving it"},
		{"speaker":2,"reply_to":1,"text":"same tbh"}
	]}`
	turns, err := parseDialogue(raw, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(turns) != 3 {
		t.Fatalf("got %d turns, want 3", len(turns))
	}
	if turns[0].ReplyTo != -1 {
		t.Errorf("turn 0 reply_to = %d, want -1", turns[0].ReplyTo)
	}
	if turns[1].ReplyTo != 0 || turns[2].ReplyTo != 1 {
		t.Errorf("reply chain broken: %+v", turns)
	}
}

// A turn naming a speaker index outside the roster must be dropped, not
// crash the whole exchange -- one bad line from the model should cost one
// line, not the conversation.
func TestParseDialogueDropsOutOfRangeSpeakers(t *testing.T) {
	raw := `{"turns":[
		{"speaker":0,"reply_to":-1,"text":"opens fine"},
		{"speaker":9,"reply_to":0,"text":"speaker does not exist"},
		{"speaker":1,"reply_to":0,"text":"replies to the opener"}
	]}`
	turns, err := parseDialogue(raw, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2 (the bad one dropped): %+v", len(turns), turns)
	}
	// The third raw turn's reply_to=0 must still point at the FIRST output
	// turn, not at index 1 (which is where it sat before the drop) -- this is
	// the remapping the dropped-turn case has to get right.
	if turns[1].ReplyTo != 0 {
		t.Errorf("surviving reply_to should remap to output index 0, got %d", turns[1].ReplyTo)
	}
}

// reply_to aimed at a turn that got dropped must fall back to "opens a new
// thread" rather than pointing at nothing or at the wrong message.
func TestParseDialogueRemapsReplyToAroundADroppedTarget(t *testing.T) {
	raw := `{"turns":[
		{"speaker":9,"reply_to":-1,"text":"bad speaker, will be dropped"},
		{"speaker":0,"reply_to":0,"text":"replies to the dropped turn"}
	]}`
	turns, err := parseDialogue(raw, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(turns))
	}
	if turns[0].ReplyTo != -1 {
		t.Errorf("reply_to pointing at a dropped turn should become -1, got %d", turns[0].ReplyTo)
	}
}

// reply_to pointing forward, or at itself, is not a valid reply -- the release
// mechanism in package sim depends on every reply pointing strictly backward,
// or a turn could be asked to wait on itself and stall forever.
func TestParseDialogueRejectsForwardAndSelfReplies(t *testing.T) {
	raw := `{"turns":[
		{"speaker":0,"reply_to":1,"text":"points forward, invalid"},
		{"speaker":1,"reply_to":1,"text":"points at itself, invalid"}
	]}`
	turns, err := parseDialogue(raw, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i, tn := range turns {
		if tn.ReplyTo >= i {
			t.Errorf("turn %d has reply_to %d, which does not point strictly backward", i, tn.ReplyTo)
		}
	}
}

// Empty text is not a message.
func TestParseDialogueDropsEmptyText(t *testing.T) {
	raw := `{"turns":[{"speaker":0,"reply_to":-1,"text":"   "}]}`
	if _, err := parseDialogue(raw, 3); err == nil {
		t.Error("expected an error when every turn is empty text")
	}
}

// The whole point of this parser existing separately from parse(): recovers
// a response wrapped in prose or a code fence, same as the single-Reaction
// parser already does.
func TestParseDialogueRecoversFromACodeFence(t *testing.T) {
	raw := "Sure, here's the exchange:\n```json\n" +
		`{"turns":[{"speaker":0,"reply_to":-1,"text":"hello"}]}` +
		"\n```"
	turns, err := parseDialogue(raw, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(turns) != 1 || turns[0].Text != "hello" {
		t.Errorf("did not recover the fenced JSON: %+v, err=%v", turns, err)
	}
}

func TestParseDialogueRejectsGarbage(t *testing.T) {
	if _, err := parseDialogue("not json at all", 3); err == nil {
		t.Error("expected an error for non-JSON input")
	}
	if _, err := parseDialogue(`{"turns":[]}`, 3); err == nil {
		t.Error("expected an error for an empty turns array")
	}
}
