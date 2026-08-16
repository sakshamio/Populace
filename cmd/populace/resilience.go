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
	// steps is the single-step budget: a paused clock still advances if the
	// user asked for exactly one tick. Decremented under the same lock that
	// guards the simulation, so a burst of clicks cannot race into a double
	// advance.
	switch {
	case srv.steps > 0:
		srv.steps--
		srv.s.Advance()
	case srv.running:
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
// speed is the user's time control, as a multiplier on the base interval.
// Bounded on both ends: below 0.1 the clock is indistinguishable from paused
// and the user should just pause, and above 8 the tick loop starts competing
// with the HTTP handlers for the same lock at ten million personas, which
// presents as a UI that stops responding rather than as a simulation that runs
// fast. Neither bound is a matter of taste.
const (
	minSpeed = 0.1
	maxSpeed = 8.0
)

// interval converts the requested speed into a sleep, and never returns
// something so small that the loop starves everything else.
func (srv *server) interval(base time.Duration) time.Duration {
	srv.mu.Lock()
	sp := srv.speed
	srv.mu.Unlock()
	if sp <= 0 {
		sp = 1
	}
	d := time.Duration(float64(base) / sp)

	// A paused clock must not spin at the fastest speed polling for a step
	// request. It still needs to poll often enough that Step feels immediate,
	// so it settles at a fixed 50 ms rather than following the speed dial.
	if d < 25*time.Millisecond {
		d = 25 * time.Millisecond
	}
	return d
}

func (srv *server) loop(base time.Duration) {
	const maxBackoff = 5 * time.Minute
	backoff := time.Duration(0)
	for {
		if backoff > 0 {
			time.Sleep(backoff)
		} else {
			// Re-read the speed every iteration rather than capturing it once:
			// the whole point of the control is that it takes effect without a
			// restart, and a loop that sampled the interval at startup would
			// accept the request, report success, and change nothing.
			srv.mu.Lock()
			paused := !srv.running && srv.steps == 0
			srv.mu.Unlock()
			if paused {
				time.Sleep(50 * time.Millisecond)
			} else {
				time.Sleep(srv.interval(base))
			}
		}
		if srv.tickOnce() {
			backoff = 0
			continue
		}
		if backoff == 0 {
			backoff = base
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
