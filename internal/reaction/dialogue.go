package reaction

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sakshamio/Populace/internal/llm"
)

// Dialogue is the second thing the model can be asked to write about a story,
// and a genuinely different one from Reaction.
//
// A Reaction is one sentence: what a kind of person thinks. That answers "what
// does the model say a market trader in Lagos makes of this", which is the
// right question for the population -- 450 archetypes cannot hold a
// conversation, only opinions. But six *named* people who know each other do
// not each independently state an opinion at the group; they talk. Someone
// breaks the news, someone else pushes back, someone asks a question, and
// there is one exchange with a shape rather than six unrelated sentences that
// happen to appear in the same feed. That shape is what Dialogue asks for.

// DialogueMember is one participant, described the same way an archetype is
// for Reaction, plus whatever Reaction already found out about them -- so the
// conversation is grounded in the same facts rather than inventing a second,
// disconnected read of who this person is.
type DialogueMember struct {
	Name, Role, Place, Region string

	// Stance and Salience come from this person's own archetype's Reaction, if
	// one is cached, so their dialogue lines land consistent with what the
	// model already said this kind of person thinks. Zero when there is none
	// yet, which the prompt is written to treat as "no strong signal" rather
	// than as neutral.
	Stance, Salience float32
	HaveReaction     bool
}

// Turn is one line, in order. Speaker indexes into the DialogueMember slice
// passed to Dialogue; ReplyTo indexes into the returned Turn slice itself, or
// is -1 for a line that opens a new thread rather than answering one.
type Turn struct {
	Speaker int    `json:"speaker"`
	Text    string `json:"text"`
	ReplyTo int    `json:"reply_to"`
}

const dialogueSystemPrompt = `You write a short group chat exchange among named
people who already know each other, reacting to one piece of news.

Reply with ONLY a JSON object, no prose and no code fence:
{"turns": [{"speaker": <index>, "reply_to": <index or -1>, "text": "<message>"}, ...]}

speaker   index into the numbered list of people you were given
reply_to  the array index of an EARLIER turn in your own "turns" list that this
          one is answering, or -1 if it opens a new thread rather than replying
text      one short chat message, the way a person actually texts a group

Rules:
- This is a conversation, not six independent opinions. Most turns should have
  a reply_to pointing at something an earlier turn in your own list said --
  someone agreeing, pushing back, asking a question, or teasing. Only the
  first turn or two should open new threads.
- Text messages are short. One or two sentences, not a paragraph. No "as a
  <role>", no restating the headline back, no even-handedness for its own
  sake -- if a person would be annoyed, they sound annoyed.
- People can go off topic, joke, or change the subject, the way real group
  chats do. Not every line needs to be substantive.
- 8 to 14 turns. Every person should speak at least once.
- Use each person's name naturally when addressing them, the way a friend
  actually would in a text.`

func dialogueUserPrompt(members []DialogueMember, relationship string, st Story) string {
	var b strings.Builder
	b.WriteString("These people ")
	b.WriteString(relationship)
	b.WriteString(":\n\n")
	for i, m := range members {
		fmt.Fprintf(&b, "%d. %s -- %s in %s, %s", i, m.Name, m.Role, m.Place, m.Region)
		if m.HaveReaction {
			fmt.Fprintf(&b, " (already known to lean %+.2f on this, cares %.2f)", m.Stance, m.Salience)
		}
		b.WriteString("\n")
	}
	b.WriteString("\nNews: ")
	b.WriteString(st.Headline)
	if st.Detail != "" {
		b.WriteString("\n")
		b.WriteString(st.Detail)
	}
	return b.String()
}

// Dialogue asks the model for one exchange among the given members about the
// given story. One call regardless of how many members -- unlike Reaction,
// which is priced per archetype, a cohort is a fixed six people and the whole
// exchange is one artifact, not six separate requests.
func (r *Relay) Dialogue(ctx context.Context, members []DialogueMember, relationship string, st Story) ([]Turn, int, error) {
	if len(members) == 0 {
		return nil, 0, fmt.Errorf("dialogue: no members given")
	}
	resp, err := r.client.Complete(ctx, llm.Batch, llm.Request{
		Model: r.model,
		Messages: []llm.Message{
			{Role: "system", Content: dialogueSystemPrompt},
			{Role: "user", Content: dialogueUserPrompt(members, relationship, st)},
		},
		MaxTokens:   900,
		Temperature: 0.9, // a scripted-feeling exchange is worse than a varied one here
		Template:    map[string]any{"enable_thinking": false},
	})
	if err != nil {
		return nil, 0, err
	}
	if len(resp.Choices) == 0 {
		return nil, 0, fmt.Errorf("dialogue: empty response")
	}
	turns, err := parseDialogue(resp.Choices[0].Message.Content, len(members))
	if err != nil {
		return nil, 0, err
	}
	return turns, resp.Usage.CompletionTokens, nil
}

// parseDialogue extracts and validates the turns array, discarding individual
// turns that fail rather than the whole response -- one bad index from a
// model that otherwise wrote a good exchange should cost one line, not the
// conversation.
func parseDialogue(s string, numMembers int) ([]Turn, error) {
	i := strings.IndexByte(s, '{')
	j := strings.LastIndexByte(s, '}')
	if i < 0 || j <= i {
		return nil, fmt.Errorf("no JSON object in %q", truncate(s, 120))
	}
	var raw struct {
		Turns []struct {
			Speaker int    `json:"speaker"`
			ReplyTo int    `json:"reply_to"`
			Text    string `json:"text"`
		} `json:"turns"`
	}
	body := s[i : j+1]
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		if err2 := json.Unmarshal([]byte(repairDelimiters(body)), &raw); err2 != nil {
			return nil, fmt.Errorf("unparseable JSON %q: %w", truncate(body, 200), err)
		}
	}
	if len(raw.Turns) == 0 {
		return nil, fmt.Errorf("no turns in %q", truncate(s, 120))
	}

	// Validated in order, tracking which output indices actually made it
	// through -- reply_to has to be remapped, because a turn dropped for a bad
	// speaker index shifts every later reply_to that pointed past it.
	out := make([]Turn, 0, len(raw.Turns))
	remap := make([]int, len(raw.Turns)) // original index -> output index, -1 if dropped
	for k, t := range raw.Turns {
		remap[k] = -1
		text := strings.TrimSpace(t.Text)
		if text == "" || t.Speaker < 0 || t.Speaker >= numMembers {
			continue
		}
		replyTo := -1
		if t.ReplyTo >= 0 && t.ReplyTo < k {
			if mapped := remap[t.ReplyTo]; mapped >= 0 {
				replyTo = mapped
			}
			// t.ReplyTo pointing at a dropped turn silently becomes -1 (opens
			// a new thread) rather than failing the whole turn -- a message
			// that stands alone is a much smaller defect than a missing one.
		}
		out = append(out, Turn{Speaker: t.Speaker, Text: text, ReplyTo: replyTo})
		remap[k] = len(out) - 1
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("every turn was malformed in %q", truncate(s, 200))
	}
	return out, nil
}
