// Package reaction puts the language model in the loop, once per archetype.
//
// The division of labour is the whole architecture: the model decides *what a
// kind of person thinks* about a story, and the simulation decides *how that
// spreads*. Neither does the other's job. A model asked to predict aggregate
// public opinion would be guessing; a simulation asked to invent what a Lagos
// market trader makes of a merger would be asserting. Splitting it means each
// side is doing something it can actually do.
//
// The cost model follows from that split. 450 archetypes at ~120 tokens each
// is one pass of the whole world; at the measured 99 tok/s aggregate that is
// under ten minutes, and it is cached forever after. Nothing here scales with
// population -- ten million personas and ten thousand cost the same.
package reaction

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sakshamio/Populace/internal/llm"
	"github.com/sakshamio/Populace/internal/world"
)

// Reaction is what one archetype makes of one story.
type Reaction struct {
	Archetype int     `json:"archetype"`
	Stance    float32 `json:"stance"`   // -1 opposed, +1 in favour
	Salience  float32 `json:"salience"` // 0 ignores it, 1 it is all they talk about
	Share     float32 `json:"share"`    // 0 never passes it on, 1 tells everyone
	Line      string  `json:"line"`     // one sentence, in their voice
	Cached    bool    `json:"cached"`
	Tokens    int     `json:"tokens"`
}

// Key identifies a cache entry. The story is hashed into it so that changing
// the headline invalidates exactly the entries that depended on it, and nothing
// else -- a cache keyed on archetype alone would serve last week's answer.
type Key struct {
	Archetype int    `json:"archetype"`
	Story     string `json:"story"`
}

// Progress is what the UI needs to show the relay honestly: not a spinner, but
// how much is done, how much failed, and what it is costing.
type Progress struct {
	Running   bool    `json:"running"`
	Story     string  `json:"story"`
	Total     int     `json:"total"`
	Done      int     `json:"done"`
	FromCache int     `json:"from_cache"`
	Failed    int     `json:"failed"`
	Tokens    int     `json:"tokens"`
	ElapsedS  float64 `json:"elapsed_s"`
	TokPerSec float64 `json:"tok_per_sec"`
	ETASec    float64 `json:"eta_s"`
	LastError string  `json:"last_error"`
	LastLine  string  `json:"last_line"`
	CacheSize int     `json:"cache_size"`
}

type Relay struct {
	client *llm.Client
	model  string

	mu    sync.RWMutex
	cache map[Key]Reaction
	path  string

	// Counters are atomic so the progress endpoint never blocks behind a
	// generation. A dashboard that stalls whenever the thing it is watching is
	// busy is worse than no dashboard.
	running    atomic.Bool
	total      atomic.Int64
	done       atomic.Int64
	fromCache  atomic.Int64
	failed     atomic.Int64
	tokens     atomic.Int64
	startedAt  atomic.Int64
	finishedAt atomic.Int64
	story      atomic.Value // string
	lastErr    atomic.Value // string
	lastLine   atomic.Value // string
}

func New(client *llm.Client, model, cachePath string) *Relay {
	r := &Relay{client: client, model: model, cache: map[Key]Reaction{}, path: cachePath}
	r.story.Store("")
	r.lastErr.Store("")
	r.lastLine.Store("")
	r.load()
	return r
}

// Get returns a cached reaction if there is one.
func (r *Relay) Get(k Key) (Reaction, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.cache[k]
	return v, ok
}

func (r *Relay) Progress() Progress {
	p := Progress{
		Running: r.running.Load(), Story: r.story.Load().(string),
		Total: int(r.total.Load()), Done: int(r.done.Load()),
		FromCache: int(r.fromCache.Load()), Failed: int(r.failed.Load()),
		Tokens:    int(r.tokens.Load()),
		LastError: r.lastErr.Load().(string), LastLine: r.lastLine.Load().(string),
	}
	r.mu.RLock()
	p.CacheSize = len(r.cache)
	r.mu.RUnlock()

	if start := r.startedAt.Load(); start > 0 {
		// Freeze the clock when the run ends. Measuring against time.Now()
		// forever made a finished run's throughput decay on the dashboard --
		// 39 tok/s, then 26, then 13 -- which is a number about how long you
		// have been looking at the screen, not about the model.
		end := time.Now()
		if fin := r.finishedAt.Load(); fin > 0 {
			end = time.Unix(0, fin)
		}
		p.ElapsedS = end.Sub(time.Unix(0, start)).Seconds()
		if p.ElapsedS > 0 {
			p.TokPerSec = float64(p.Tokens) / p.ElapsedS
		}
		// ETA from generated-per-second rather than done-per-second, because
		// cache hits complete instantly and would make the estimate lie about
		// the part that actually takes time.
		gen := p.Done - p.FromCache
		if gen > 0 && p.Done < p.Total {
			perItem := p.ElapsedS / float64(gen)
			p.ETASec = perItem * float64(p.Total-p.Done)
		}
	}
	return p
}

// Story is a headline plus its framing, which together decide what the model is
// asked. Fingerprinted into the cache key.
type Story struct {
	Headline string
	Stance   float64
	Detail   string
}

func (s Story) fingerprint() string {
	return fmt.Sprintf("%s|%.2f|%s", s.Headline, s.Stance, s.Detail)
}

// systemPrompt is deliberately blunt about the two failure modes that make
// generated persona reactions useless: sounding like a press release, and
// agreeing with everything.
const systemPrompt = `You model how a specific kind of person reacts to news.

Reply with ONLY a JSON object, no prose and no code fence:
{"stance": <number -1..1>, "salience": <number 0..1>, "share": <number 0..1>, "line": "<one sentence>"}

stance   how favourably this person views the news (-1 strongly against, 1 strongly for)
salience how much they care at all (0 ignores it, 1 it dominates their week)
share    how likely they are to pass it on to people they know (0 never, 1 tells everyone)
line     one sentence in their own voice, concrete and specific to their situation

Rules:
- Many people are indifferent to most news. A low salience is a valid answer and often the right one.
- Self-interest, cost and distance matter more than ideology. Ask what this changes for them this month.
- Do not be even-handed for its own sake. If this person would be angry, be angry.
- No slogans, no "as a <role>", no summarising the headline back.`

func userPrompt(a world.Archetype, st Story) string {
	var b strings.Builder
	b.WriteString(a.Describe())
	b.WriteString("\n\nNews: ")
	b.WriteString(st.Headline)
	if st.Detail != "" {
		b.WriteString("\n")
		b.WriteString(st.Detail)
	}
	return b.String()
}

// Run generates reactions for the given archetypes, respecting the gateway's
// admission control rather than trying to beat it.
//
// Concurrency is capped at the gateway's batch reservation. Going wider does
// not go faster -- the gateway sheds past its cap and the client backs off, so
// the only effect is a burst of 503s and a slower wall clock. The measured
// saturation point is 8 concurrent, of which batch may hold 6.
func (r *Relay) Run(ctx context.Context, st Story, archetypes []int, conc int, onEach func(Reaction)) error {
	if !r.running.CompareAndSwap(false, true) {
		return fmt.Errorf("a generation is already running")
	}
	defer r.running.Store(false)

	r.total.Store(int64(len(archetypes)))
	r.done.Store(0)
	r.fromCache.Store(0)
	r.failed.Store(0)
	r.tokens.Store(0)
	r.startedAt.Store(time.Now().UnixNano())
	r.finishedAt.Store(0)
	r.story.Store(st.Headline)
	r.lastErr.Store("")

	if conc <= 0 {
		conc = 6
	}
	fp := st.fingerprint()
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, id := range archetypes {
		if ctx.Err() != nil {
			break
		}
		key := Key{Archetype: id, Story: fp}
		if got, ok := r.Get(key); ok {
			got.Cached = true
			r.done.Add(1)
			r.fromCache.Add(1)
			if onEach != nil {
				mu.Lock()
				onEach(got)
				mu.Unlock()
			}
			continue
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(id int, key Key) {
			defer wg.Done()
			defer func() { <-sem }()

			rec, err := r.one(ctx, world.Archetype(id), st)
			if err != nil && ctx.Err() == nil {
				// One retry. At temperature 0.8 a malformed sample is a sample,
				// not a property of the archetype, and re-rolling costs one
				// generation against permanently having no opinion for a cell
				// that may hold tens of thousands of people.
				rec, err = r.one(ctx, world.Archetype(id), st)
			}
			if err != nil {
				r.failed.Add(1)
				r.done.Add(1)
				r.lastErr.Store(err.Error())
				return
			}
			r.mu.Lock()
			r.cache[key] = rec
			r.mu.Unlock()
			r.done.Add(1)
			r.tokens.Add(int64(rec.Tokens))
			r.lastLine.Store(rec.Line)
			if onEach != nil {
				mu.Lock()
				onEach(rec)
				mu.Unlock()
			}
		}(id, key)
	}
	wg.Wait()
	r.finishedAt.Store(time.Now().UnixNano())
	r.save()
	return ctx.Err()
}

func (r *Relay) one(ctx context.Context, a world.Archetype, st Story) (Reaction, error) {
	resp, err := r.client.Complete(ctx, llm.Batch, llm.Request{
		Model: r.model,
		Messages: []llm.Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt(a, st)},
		},
		MaxTokens:   220,
		Temperature: 0.8, // varied personas need varied answers; 0 gives 450 near-identical ones
		Template:    map[string]any{"enable_thinking": false},
	})
	if err != nil {
		return Reaction{}, err
	}
	if len(resp.Choices) == 0 {
		return Reaction{}, fmt.Errorf("archetype %d: empty response", int(a))
	}

	rec, err := parse(resp.Choices[0].Message.Content)
	if err != nil {
		return Reaction{}, fmt.Errorf("archetype %d (%s): %w", int(a), a, err)
	}
	rec.Archetype = int(a)
	rec.Tokens = resp.Usage.CompletionTokens
	return rec, nil
}

// parse extracts the JSON object from whatever the model actually returned.
//
// Models wrap JSON in prose or fences often enough that a strict
// json.Unmarshal on the whole body fails on answers that are otherwise
// perfectly good. Taking the outermost braces recovers those without accepting
// anything a stricter parser would reject on content.
func parse(s string) (Reaction, error) {
	i := strings.IndexByte(s, '{')
	j := strings.LastIndexByte(s, '}')
	if i < 0 || j <= i {
		return Reaction{}, fmt.Errorf("no JSON object in %q", truncate(s, 120))
	}
	var raw struct {
		Stance   float32 `json:"stance"`
		Salience float32 `json:"salience"`
		Share    float32 `json:"share"`
		Line     string  `json:"line"`
	}
	body := s[i : j+1]
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		// Observed twice in a 450-archetype run: the model escapes the quotes
		// that delimit its own string value, emitting `"line": \"text\"`.
		// Repairing only the delimiters -- a quote right after a colon, or
		// right before a comma or closing brace -- leaves genuinely escaped
		// quotes inside the sentence alone, which a blanket unescape would not.
		if err2 := json.Unmarshal([]byte(repairDelimiters(body)), &raw); err2 != nil {
			return Reaction{}, fmt.Errorf("unparseable JSON %q: %w", truncate(body, 120), err)
		}
	}
	if strings.TrimSpace(raw.Line) == "" {
		return Reaction{}, fmt.Errorf("no line in %q", truncate(s, 120))
	}
	// Clamp rather than reject: a model returning 1.5 for stance meant "very
	// much in favour", and discarding the whole reaction over it would throw
	// away a good answer for a formatting slip.
	return Reaction{
		Stance:   clamp(raw.Stance, -1, 1),
		Salience: clamp(raw.Salience, 0, 1),
		Share:    clamp(raw.Share, 0, 1),
		Line:     strings.TrimSpace(raw.Line),
	}, nil
}

// repairDelimiters unescapes only the quotes acting as string delimiters.
func repairDelimiters(s string) string {
	r := strings.NewReplacer(
		`: \"`, `: "`,
		`:\"`, `:"`,
		`\",`, `",`,
		`\"}`, `"}`,
		`\" }`, `" }`,
	)
	return r.Replace(s)
}

func clamp(v, lo, hi float32) float32 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Snapshot returns every cached reaction for a story, sorted by archetype.
func (r *Relay) Snapshot(st Story) map[int]Reaction {
	fp := st.fingerprint()
	out := map[int]Reaction{}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for k, v := range r.cache {
		if k.Story == fp {
			out[k.Archetype] = v
		}
	}
	return out
}

// --- persistence --------------------------------------------------------
//
// "Cached forever" is a design claim, and a cache that dies with the process
// does not make it. This is a plain JSON file rather than a database because
// the whole cache is a few hundred kilobytes and the ability to read it in an
// editor is worth more than any query it would support.

type diskEntry struct {
	Key      Key      `json:"key"`
	Reaction Reaction `json:"reaction"`
}

func (r *Relay) load() {
	if r.path == "" {
		return
	}
	b, err := os.ReadFile(r.path)
	if err != nil {
		return
	}
	var entries []diskEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		r.lastErr.Store("cache file unreadable: " + err.Error())
		return
	}
	r.mu.Lock()
	for _, e := range entries {
		r.cache[e.Key] = e.Reaction
	}
	r.mu.Unlock()
}

func (r *Relay) save() {
	if r.path == "" {
		return
	}
	r.mu.RLock()
	entries := make([]diskEntry, 0, len(r.cache))
	for k, v := range r.cache {
		entries = append(entries, diskEntry{k, v})
	}
	r.mu.RUnlock()

	b, err := json.MarshalIndent(entries, "", " ")
	if err != nil {
		return
	}
	// Write-then-rename: a crash mid-write would otherwise leave a truncated
	// file that load() silently discards, losing the whole cache.
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		r.lastErr.Store("cache write failed: " + err.Error())
		return
	}
	if err := os.Rename(tmp, r.path); err != nil {
		r.lastErr.Store("cache rename failed: " + err.Error())
	}
}
