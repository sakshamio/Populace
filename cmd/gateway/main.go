// Command gateway runs in front of SGLang on the machine that holds the GPU.
//
// It exists because SGLang has no auth and unbounded queueing, and because the
// hardware has a measured saturation point (8 concurrent requests, 99 tok/s
// aggregate) that something has to enforce. Exposing SGLang to an application
// directly means the first burst turns a working model server into a queue of
// requests that all time out together.
//
//	gateway -upstream http://127.0.0.1:30000 -addr :8080 \
//	        -token 'railway:REDACTED' -rate 600
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sakshamio/Populace/internal/gateway"
)

func main() {
	var (
		addr     = flag.String("addr", ":"+envOr("PORT", "8080"), "listen address")
		upstream = flag.String("upstream", envOr("UPSTREAM", "http://127.0.0.1:30000"), "SGLang base URL")
		inFlight = flag.Int("in-flight", 8, "hard admission cap (measured saturation is 8)")
		batchCap = flag.Int("batch-cap", 6, "of -in-flight, how many batch requests may hold")
		waiting  = flag.Int("max-waiting", 64, "queued requests per lane before shedding")
		waitFor  = flag.Duration("wait", 45*time.Second, "how long a request may wait for a slot")
		call     = flag.Duration("call-timeout", 10*time.Minute, "upstream timeout")
		cacheMB  = flag.Int64("cache-mb", 256, "response cache size")
		rate     = flag.Int("rate", 0, "per-token requests/min, 0 = unlimited")
		tokens   = flag.String("token", os.Getenv("GATEWAY_TOKENS"),
			"comma-separated name:token pairs; empty disables auth (local dev only)")
	)
	flag.Parse()

	toks := map[string]string{}
	for _, pair := range strings.Split(*tokens, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, tok, ok := strings.Cut(pair, ":")
		if !ok || tok == "" {
			log.Fatalf("-token entries must be name:token, got %q", pair)
		}
		toks[tok] = name
	}
	if len(toks) == 0 {
		log.Print("WARNING: no -token set, so anyone who can reach this port " +
			"can spend the GPU. Acceptable on localhost; not over a tailnet.")
	}

	g := gateway.New(gateway.Config{
		Upstream:    strings.TrimRight(*upstream, "/"),
		MaxInFlight: *inFlight,
		BatchCap:    *batchCap,
		MaxWaiting:  *waiting,
		WaitTimeout: *waitFor,
		CallTimeout: *call,
		CacheBytes:  *cacheMB << 20,
		Tokens:      toks,
		RatePerMin:  *rate,
	})
	g.Log()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           g.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: a streamed generation legitimately runs for
		// minutes, and a write deadline would sever it mid-answer.
	}
	log.Printf("listening on %s", *addr)
	log.Fatal(srv.ListenAndServe())
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
