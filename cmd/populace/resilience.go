package main

import (
	"log"
	"net/http"
	"runtime/debug"
	"sync/atomic"
	"time"
)

// Staying up.
//
// This is meant to run for weeks unattended, so the question is not whether it
// is correct but what it does when something is not. Two rules, both about not
// letting a local failure become a global one:
//
//   - A panic in one HTTP handler must not take the process down with it. The
//     simulation in memory is the expensive thing here -- a 10M world costs
//     seconds of CPU and 1.5 GB to rebuild -- so throwing it away because a
//     malformed query hit a nil map is the wrong trade by a wide margin.
//
//   - A panic in the tick loop must not stop the clock forever. It must also
//     not spin: a panic that repeats every tick would fill the disk with
//     stack traces faster than anyone would notice.

// recovered counts panics survived, and is surfaced on /api/stats. A process
// that has been quietly catching panics for a week looks healthy from outside,
// which is exactly the situation this counter exists to prevent.
var recovered atomic.Int64

func recoverHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				recovered.Add(1)
				log.Printf("PANIC in %s %s: %v\n%s", r.Method, r.URL.Path, v, debug.Stack())
				// Best effort: if the handler already wrote a header this is a
				// no-op, which is fine -- the client gets a truncated body and
				// the server stays up, and only one of those two is worth
				// protecting.
				http.Error(rw, "internal error; the simulation is still running",
					http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(rw, r)
	})
}

// tickOnce runs one tick and survives a panic inside it, returning whether the
// tick succeeded so the caller can decide how hard to back off.
func (srv *server) tickOnce() (ok bool) {
	defer func() {
		if v := recover(); v != nil {
			recovered.Add(1)
			log.Printf("PANIC in tick %d: %v\n%s", srv.s.Tick, v, debug.Stack())
			ok = false
		}
	}()
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.running {
		srv.s.Advance()
	}
	return true
}

// loop drives the clock. It skips rather than queues when a tick overruns, and
// backs off exponentially when ticks panic.
//
// Skipping matters at ten million, where a tick is ~230 ms against a 400 ms
// interval: catching up by running back-to-back ticks would turn one slow tick
// into a permanently saturated box and a UI that never gets a response.
//
// Backing off matters more. A tick that panics deterministically would panic
// again immediately, and an un-throttled loop would write a stack trace every
// 400 ms until the disk filled -- turning a bug in one subsystem into an
// outage of the whole machine.
func (srv *server) loop(every time.Duration) {
	backoff := every
	const maxBackoff = 5 * time.Minute
	for {
		time.Sleep(backoff)
		if srv.tickOnce() {
			backoff = every
			continue
		}
		if backoff < maxBackoff {
			backoff *= 4
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
		log.Printf("tick failed; backing off to %s before trying again", backoff)
	}
}
