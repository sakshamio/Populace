package gateway

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeUpstream stands in for SGLang and, critically, records the maximum
// concurrency it ever saw. That is the property the whole package exists to
// guarantee, so it is the property the tests measure directly rather than
// inferring from timings.
func fakeUpstream(delay time.Duration) (*httptest.Server, *int64, *int64) {
	var cur, peak int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&cur, 1)
		for {
			old := atomic.LoadInt64(&peak)
			if n <= old || atomic.CompareAndSwapInt64(&peak, old, n) {
				break
			}
		}
		time.Sleep(delay)
		atomic.AddInt64(&cur, -1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],`+
			`"usage":{"completion_tokens":12}}`)
	}))
	return srv, &peak, &cur
}

func post(t *testing.T, h http.Handler, body, lane string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	r.Header.Set("Content-Type", "application/json")
	if lane != "" {
		r.Header.Set("X-Populace-Lane", lane)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// The measured hardware saturates at 8 concurrent requests. Exceeding that
// costs latency and buys no throughput, so the cap must hold under a burst
// far larger than itself.
func TestAdmissionCapIsNeverExceeded(t *testing.T) {
	up, peak, _ := fakeUpstream(25 * time.Millisecond)
	defer up.Close()

	g := New(Config{Upstream: up.URL, MaxInFlight: 8, BatchCap: 6, MaxWaiting: 1000})
	h := g.Handler()

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			post(t, h, fmt.Sprintf(`{"n":%d}`, i), "batch")
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt64(peak); got > 8 {
		t.Fatalf("upstream saw %d concurrent requests, cap is 8", got)
	}
	t.Logf("peak upstream concurrency %d of 8 under a 200-request burst", atomic.LoadInt64(peak))
}

// Batch must not be able to fill every slot, or an interactive request -- one
// a user is watching -- waits behind work nobody is watching.
func TestBatchCannotStarveInteractive(t *testing.T) {
	up, _, cur := fakeUpstream(150 * time.Millisecond)
	defer up.Close()

	g := New(Config{Upstream: up.URL, MaxInFlight: 8, BatchCap: 6,
		MaxWaiting: 1000, WaitTimeout: 5 * time.Second})
	h := g.Handler()

	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); post(t, h, fmt.Sprintf(`{"b":%d}`, i), "batch") }(i)
	}
	// Let batch saturate its reservation.
	time.Sleep(60 * time.Millisecond)
	if got := atomic.LoadInt64(cur); got > 6 {
		t.Fatalf("batch occupied %d slots, reservation is 6", got)
	}

	start := time.Now()
	w := post(t, h, `{"interactive":1}`, "interactive")
	took := time.Since(start)
	wg.Wait()

	if w.Code != http.StatusOK {
		t.Fatalf("interactive request got %d, want 200", w.Code)
	}
	// One upstream call is 150ms; anything near that means it was admitted
	// promptly rather than queued behind the batch flood.
	if took > 600*time.Millisecond {
		t.Fatalf("interactive waited %s behind batch work", took)
	}
	t.Logf("interactive admitted in %s with 60 batch requests in flight", took.Round(time.Millisecond))
}

// Shedding is the correct behaviour at saturation. Accepting work the hardware
// cannot do just converts a fast rejection into a slow timeout.
func TestShedsRatherThanQueueingForever(t *testing.T) {
	up, _, _ := fakeUpstream(200 * time.Millisecond)
	defer up.Close()

	g := New(Config{Upstream: up.URL, MaxInFlight: 2, BatchCap: 1, MaxWaiting: 2,
		WaitTimeout: 100 * time.Millisecond})
	h := g.Handler()

	var shed, ok int64
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := post(t, h, fmt.Sprintf(`{"s":%d}`, i), "batch")
			switch w.Code {
			case http.StatusServiceUnavailable:
				atomic.AddInt64(&shed, 1)
				if w.Header().Get("Retry-After") == "" {
					t.Error("503 without Retry-After leaves the client guessing")
				}
			case http.StatusOK:
				atomic.AddInt64(&ok, 1)
			}
		}(i)
	}
	wg.Wait()
	if shed == 0 {
		t.Fatal("nothing was shed; the queue grew without bound")
	}
	t.Logf("under 40 requests at cap 2: %d served, %d shed with Retry-After", ok, shed)
}

func TestCacheServesRepeats(t *testing.T) {
	up, _, _ := fakeUpstream(5 * time.Millisecond)
	defer up.Close()
	g := New(Config{Upstream: up.URL, MaxInFlight: 4})
	h := g.Handler()

	body := `{"messages":[{"role":"user","content":"same"}]}`
	if w := post(t, h, body, "batch"); w.Header().Get("X-Populace-Cache") != "miss" {
		t.Fatalf("first call should miss, got %q", w.Header().Get("X-Populace-Cache"))
	}
	w := post(t, h, body, "batch")
	if w.Header().Get("X-Populace-Cache") != "hit" {
		t.Fatalf("repeat should hit, got %q", w.Header().Get("X-Populace-Cache"))
	}
	hits, misses, _ := g.cache.Stats()
	t.Logf("cache hits=%d misses=%d", hits, misses)
}

func TestAuthRejectsUnknownToken(t *testing.T) {
	up, _, _ := fakeUpstream(time.Millisecond)
	defer up.Close()
	g := New(Config{Upstream: up.URL, Tokens: map[string]string{"good": "railway"}})
	h := g.Handler()

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{}`))
	r.Header.Set("Authorization", "Bearer bad")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bad token got %d, want 401", w.Code)
	}

	r = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{}`))
	r.Header.Set("Authorization", "Bearer good")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("good token got %d, want 200", w.Code)
	}
}

func TestRateLimitPerClient(t *testing.T) {
	up, _, _ := fakeUpstream(time.Millisecond)
	defer up.Close()
	g := New(Config{Upstream: up.URL, Tokens: map[string]string{"t": "c"}, RatePerMin: 3})
	h := g.Handler()

	var codes []int
	for i := 0; i < 5; i++ {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			bytes.NewBufferString(fmt.Sprintf(`{"i":%d}`, i)))
		r.Header.Set("Authorization", "Bearer t")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		codes = append(codes, w.Code)
	}
	if codes[3] != http.StatusTooManyRequests && codes[4] != http.StatusTooManyRequests {
		t.Fatalf("expected a 429 after the budget, got %v", codes)
	}
	t.Logf("codes with a 3/min budget: %v", codes)
}
