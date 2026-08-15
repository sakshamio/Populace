// Command populace serves the world and the renderer.
//
// Phase 1 of the build plan: prove that a Go process can generate a population
// at real scale and hand it to a WebGL2 renderer as packed binary, with no
// JSON and no parse step on the client. Everything else -- ticks, dynamics,
// the LLM relay -- is deliberately absent. If this shape does not hold at ten
// million, the rest of the design changes, so it is worth finding out first.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"time"

	"github.com/sakshamio/Populace/internal/world"
)

func main() {
	var (
		addr    = flag.String("addr", ":"+envOr("PORT", "8080"), "listen address")
		n       = flag.Int("n", 1_000_000, "personas to generate at startup")
		seed    = flag.Uint64("seed", 20260815, "world seed")
		webDir  = flag.String("web", "web", "static assets directory")
		maxSend = flag.Int("max", 10_000_000, "cap on personas served per request")
	)
	flag.Parse()

	log.Printf("generating %s personas (seed %d)...", commas(*n), *seed)
	t0 := time.Now()
	w := world.Generate(*n, *seed)
	gen := time.Since(t0)

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	log.Printf("generated in %s — %.1f MB heap, %.0f ns/persona",
		gen.Round(time.Millisecond), float64(ms.HeapAlloc)/1e6,
		float64(gen.Nanoseconds())/float64(*n))

	counts := w.StratumCounts()
	for i := 0; i < int(world.NumStrata); i++ {
		s := world.Stratum(i)
		log.Printf("  %-15s %9s  weight %.4f",
			world.Strata[i].Name, commas(counts[i]), s.Weight())
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir(*webDir)))

	// The bulk endpoint. Content is the GPU vertex buffer, byte for byte.
	mux.HandleFunc("/api/instances", func(rw http.ResponseWriter, r *http.Request) {
		want := w.N
		if q := r.URL.Query().Get("n"); q != "" {
			if v, err := strconv.Atoi(q); err == nil && v > 0 {
				want = v
			}
		}
		if want > w.N {
			want = w.N
		}
		if want > *maxSend {
			want = *maxSend
		}

		// Serving a prefix of the population is a valid subsample precisely
		// because generation is per-persona deterministic and independent --
		// index order carries no structure.
		view := *w
		view.N = want

		rw.Header().Set("Content-Type", "application/octet-stream")
		rw.Header().Set("Content-Length",
			strconv.Itoa(8+want*world.InstanceBytes))
		rw.Header().Set("Cache-Control", "public, max-age=60")
		start := time.Now()
		if err := view.EncodeInstances(rw); err != nil {
			log.Printf("encode: %v", err)
			return
		}
		log.Printf("served %s instances (%.1f MB) in %s",
			commas(want), float64(want*world.InstanceBytes)/1e6,
			time.Since(start).Round(time.Millisecond))
	})

	mux.HandleFunc("/api/stats", func(rw http.ResponseWriter, r *http.Request) {
		c := w.StratumCounts()
		rw.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(rw, `{"n":%d,"seed":%d,"instance_bytes":%d,"strata":{`,
			w.N, w.Seed, world.InstanceBytes)
		for i := 0; i < int(world.NumStrata); i++ {
			if i > 0 {
				fmt.Fprint(rw, ",")
			}
			fmt.Fprintf(rw, `"%s":{"count":%d,"weight":%g}`,
				world.Strata[i].Name, c[i], world.Stratum(i).Weight())
		}
		fmt.Fprint(rw, "}}")
	})

	mux.HandleFunc("/healthz", func(rw http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(rw, "ok")
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
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

func commas(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}
