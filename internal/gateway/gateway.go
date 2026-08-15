// Package gateway fronts an SGLang server with the things it does not have.
//
// SGLang exposes an OpenAI-compatible API with no authentication and unbounded
// queueing. A burst does not get rejected; it grows the queue until everything
// times out at once, and there is no way to say that the request a user is
// watching matters more than a batch job nobody is watching. Pointing a public
// app at it directly is not a shortcut, it is an outage waiting for traffic.
//
// Measured ceiling on the target hardware (DGX Spark, Qwen3.8-27B NVFP4 +
// DSpark): 99 tok/s aggregate, saturating at 8 concurrent requests. Past 8,
// latency doubles and throughput does not move. That number is the whole
// reason this package exists: admission is capped there rather than left to
// chance.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Lane is admission priority.
//
// Note what this cannot be: an in-flight generation is not preemptible. Once a
// request is on the GPU, cancelling it wastes the work already done and frees
// the slot no sooner than finishing would. So "spotlight preempts batch" -- as
// the design doc originally put it -- is not implementable, and pretending
// otherwise would produce a scheduler that lies about its own behaviour.
//
// What is implementable is admission priority by reservation: batch traffic
// may occupy at most BatchCap of the MaxInFlight slots, so an interactive
// request never has to wait behind a full house of batch work.
type Lane int

const (
	LaneInteractive Lane = iota // a user is watching; latency matters
	LaneBatch                   // archetype generation; nobody is watching
)

func (l Lane) String() string {
	if l == LaneInteractive {
		return "interactive"
	}
	return "batch"
}

// Config is the gateway's tuning surface.
type Config struct {
	Upstream    string        // e.g. http://127.0.0.1:30000
	MaxInFlight int           // hard cap; measured saturation point is 8
	BatchCap    int           // of MaxInFlight, how many batch may hold
	MaxWaiting  int           // shed beyond this many queued, per lane
	WaitTimeout time.Duration // how long a request may wait for a slot
	CallTimeout time.Duration // upstream request timeout
	CacheBytes  int64
	Tokens      map[string]string // bearer token -> client name
	RatePerMin  int               // per-token request budget, 0 = unlimited
}

func (c *Config) defaults() {
	if c.MaxInFlight <= 0 {
		c.MaxInFlight = 8
	}
	if c.BatchCap <= 0 || c.BatchCap > c.MaxInFlight {
		// Leave at least two slots that batch can never occupy.
		c.BatchCap = max(1, c.MaxInFlight-2)
	}
	if c.MaxWaiting <= 0 {
		c.MaxWaiting = 64
	}
	if c.WaitTimeout <= 0 {
		c.WaitTimeout = 45 * time.Second
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = 10 * time.Minute
	}
	if c.CacheBytes <= 0 {
		c.CacheBytes = 256 << 20
	}
}

// Gateway is an http.Handler.
type Gateway struct {
	cfg   Config
	slots chan struct{} // MaxInFlight
	batch chan struct{} // BatchCap, held in addition to slots
	cache *Cache
	limit *Limiter
	http  *http.Client

	inFlight atomic.Int64
	waiting  [2]atomic.Int64
	served   atomic.Int64
	shed     atomic.Int64
	errs     atomic.Int64
	upTokens atomic.Int64 // completion tokens seen
	upNanos  atomic.Int64 // nanoseconds spent upstream

	mu       sync.Mutex
	recent   []sample // rolling window for live tok/s
	lastSeen time.Time
}

type sample struct {
	at     time.Time
	tokens int64
	dur    time.Duration
}

func New(cfg Config) *Gateway {
	cfg.defaults()
	g := &Gateway{
		cfg:   cfg,
		slots: make(chan struct{}, cfg.MaxInFlight),
		batch: make(chan struct{}, cfg.BatchCap),
		cache: NewCache(cfg.CacheBytes),
		limit: NewLimiter(cfg.RatePerMin),
		http:  &http.Client{Timeout: cfg.CallTimeout},
	}
	return g
}

func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", g.handleHealth)
	mux.HandleFunc("/v1/chat/completions", g.auth(g.handleCompletions))
	mux.HandleFunc("/v1/completions", g.auth(g.handleCompletions))
	return mux
}

func (g *Gateway) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(g.cfg.Tokens) == 0 { // unauthenticated mode, local dev only
			next(w, r)
			return
		}
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		name, ok := g.cfg.Tokens[tok]
		if !ok || tok == "" {
			httpErr(w, http.StatusUnauthorized, "unknown or missing bearer token")
			return
		}
		if !g.limit.Allow(name) {
			w.Header().Set("Retry-After", "60")
			httpErr(w, http.StatusTooManyRequests,
				fmt.Sprintf("rate limit for %q exceeded (%d req/min)", name, g.cfg.RatePerMin))
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxClient{}, name)))
	}
}

type ctxClient struct{}

func (g *Gateway) handleCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		httpErr(w, http.StatusBadRequest, "could not read request body")
		return
	}

	lane := LaneBatch
	if r.Header.Get("X-Populace-Lane") == "interactive" {
		lane = LaneInteractive
	}

	// Streaming is passed through untouched and never cached: a cached stream
	// would have to be replayed with fabricated timings, and the point of
	// streaming is that the timings are real.
	streaming := isStreaming(body)

	if !streaming {
		if hit, ok := g.cache.Get(body); ok {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Populace-Cache", "hit")
			w.Write(hit)
			g.served.Add(1)
			return
		}
	}

	if err := g.admit(r.Context(), lane); err != nil {
		g.shed.Add(1)
		if errors.Is(err, errBusy) {
			w.Header().Set("Retry-After", "10")
			httpErr(w, http.StatusServiceUnavailable,
				fmt.Sprintf("gateway saturated: %d in flight, %d waiting on the %s lane. "+
					"This is admission control, not a failure -- retry after the header says.",
					g.inFlight.Load(), g.waiting[lane].Load(), lane))
			return
		}
		httpErr(w, http.StatusRequestTimeout, "client went away while waiting for a slot")
		return
	}
	defer g.release(lane)

	start := time.Now()
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		g.cfg.Upstream+r.URL.Path, bytes.NewReader(body))
	if err != nil {
		g.errs.Add(1)
		httpErr(w, http.StatusInternalServerError, "could not build upstream request")
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.http.Do(req)
	if err != nil {
		g.errs.Add(1)
		httpErr(w, http.StatusBadGateway,
			"upstream model server is not reachable: "+err.Error())
		return
	}
	defer resp.Body.Close()

	w.Header().Set("X-Populace-Cache", "miss")
	w.Header().Set("X-Populace-Lane", lane.String())
	for _, h := range []string{"Content-Type"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	if streaming {
		g.pipeStream(w, resp.Body)
		g.served.Add(1)
		g.mark(start, 0)
		return
	}

	out, err := io.ReadAll(resp.Body)
	if err != nil {
		g.errs.Add(1)
		return
	}
	w.Write(out)
	g.served.Add(1)

	if resp.StatusCode == http.StatusOK {
		g.cache.Put(body, out)
		g.mark(start, completionTokens(out))
	}
}

// pipeStream forwards SSE without buffering, flushing per chunk. Without the
// flush the whole point of streaming is lost: the client would receive the
// response in one lump at the end.
func (g *Gateway) pipeStream(w http.ResponseWriter, src io.Reader) {
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 8<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

var errBusy = errors.New("saturated")

// admit acquires a slot, respecting the batch reservation.
func (g *Gateway) admit(ctx context.Context, lane Lane) error {
	if g.waiting[lane].Load() >= int64(g.cfg.MaxWaiting) {
		return errBusy
	}
	ctx, cancel := context.WithTimeout(ctx, g.cfg.WaitTimeout)
	defer cancel()

	g.waiting[lane].Add(1)
	defer g.waiting[lane].Add(-1)

	// Batch takes the reservation token first, so it can never occupy more
	// than BatchCap of MaxInFlight and interactive always has room.
	if lane == LaneBatch {
		select {
		case g.batch <- struct{}{}:
		case <-ctx.Done():
			return busyOr(ctx)
		}
	}
	select {
	case g.slots <- struct{}{}:
		g.inFlight.Add(1)
		return nil
	case <-ctx.Done():
		if lane == LaneBatch {
			<-g.batch
		}
		return busyOr(ctx)
	}
}

func busyOr(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errBusy
	}
	return ctx.Err()
}

func (g *Gateway) release(lane Lane) {
	<-g.slots
	g.inFlight.Add(-1)
	if lane == LaneBatch {
		<-g.batch
	}
}

func (g *Gateway) mark(start time.Time, tokens int64) {
	d := time.Since(start)
	g.upNanos.Add(d.Nanoseconds())
	g.upTokens.Add(tokens)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lastSeen = time.Now()
	g.recent = append(g.recent, sample{at: time.Now(), tokens: tokens, dur: d})
	cut := time.Now().Add(-60 * time.Second)
	i := 0
	for i < len(g.recent) && g.recent[i].at.Before(cut) {
		i++
	}
	g.recent = g.recent[i:]
}

// liveTokPerSec is aggregate throughput over the last minute -- tokens divided
// by wall time, not by summed request duration, so concurrency is reflected.
func (g *Gateway) liveTokPerSec() float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.recent) < 2 {
		return 0
	}
	var tok int64
	for _, s := range g.recent {
		tok += s.tokens
	}
	span := g.recent[len(g.recent)-1].at.Sub(g.recent[0].at).Seconds()
	if span <= 0 {
		return 0
	}
	return float64(tok) / span
}

func (g *Gateway) handleHealth(w http.ResponseWriter, r *http.Request) {
	hits, misses, bytes := g.cache.Stats()
	total := hits + misses
	var rate float64
	if total > 0 {
		rate = float64(hits) / float64(total) * 100
	}
	g.mu.Lock()
	last := g.lastSeen
	g.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"upstream":            g.cfg.Upstream,
		"max_in_flight":       g.cfg.MaxInFlight,
		"batch_cap":           g.cfg.BatchCap,
		"in_flight":           g.inFlight.Load(),
		"waiting_interactive": g.waiting[LaneInteractive].Load(),
		"waiting_batch":       g.waiting[LaneBatch].Load(),
		"served":              g.served.Load(),
		"shed":                g.shed.Load(),
		"errors":              g.errs.Load(),
		"tok_per_sec_60s":     round1(g.liveTokPerSec()),
		"cache": map[string]any{
			"hits": hits, "misses": misses,
			"hit_rate_pct": round1(rate), "bytes": bytes,
		},
		"last_upstream_ok": nullableTime(last),
	})
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

func httpErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "code": code},
	})
}

func isStreaming(body []byte) bool {
	var probe struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.Stream
}

func completionTokens(body []byte) int64 {
	var probe struct {
		Usage struct {
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return 0
	}
	return probe.Usage.CompletionTokens
}

// Log writes a one-line startup summary. Worth printing the admission numbers
// explicitly: a gateway silently configured for 64 in flight would look
// healthy right up until it made the model server useless.
func (g *Gateway) Log() {
	log.Printf("gateway -> %s | admission %d in flight (batch may hold %d) | "+
		"wait %s | cache %d MB | %d token(s)",
		g.cfg.Upstream, g.cfg.MaxInFlight, g.cfg.BatchCap,
		g.cfg.WaitTimeout, g.cfg.CacheBytes>>20, len(g.cfg.Tokens))
}
