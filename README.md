# Populace

A world-scale persona simulation: grounded personas on a real globe, living
daily routines and reacting to events, with a language model authoring the
content and a Go engine doing the per-capita work.

**Plan:** https://claude.ai/code/artifact/cb3b3c4a-f91b-4a80-b24c-f2afbf6f5a7c

## Status — phase 1

Placement and render at scale. Deliberately no ticks, no dynamics, no LLM: if
a Go process cannot hand ten million personas to a WebGL2 renderer as packed
binary at 60fps, the rest of the design changes, so that gets proven first.

```
go run ./cmd/populace -n 1000000
open http://localhost:8080
```

Flags: `-n` personas, `-seed` world seed, `-addr` listen address, `-web` asset dir.

## The three decisions that shape everything

**The LLM is not the simulation.** Measured on a single DGX Spark running
Qwen3.8-27B NVFP4 + DSpark: 99 tok/s aggregate, saturating at concurrency 8,
which is ~5,300 persona-ticks/hour. Generating for ten million personas once
would take 79 days. So the model authors content per *archetype* (~400 of
them, cached forever) and cheap deterministic Go applies it per capita. Any
feature whose cost scales with population rather than with archetypes or user
attention is out of budget by construction.

**Struct-of-arrays, not `[]*Persona`.** Ten million pointer-bearing structs
give the GC ten million objects to trace and turn the tick loop into a
cache-miss generator. See `internal/world/world.go`.

**The wire format is the GPU layout.** 16 bytes per persona, written by Go and
handed straight to `gl.bufferData`. No JSON, no parse step, no garbage. Ten
million personas is 160 MB of binary against roughly a gigabyte of text.

## Sampling is stratified, and aggregates must be weighted

Sampling in proportion to world population yields *zero* celebrities — about
1.25 per million — and a statistically invisible number of high-net-worth
individuals. Influence is not proportional to population, so rare-but-
influential strata are deliberately oversampled and carry an importance
weight.

`World.WeightedMean` is the only aggregation helper, on purpose. A raw mean
over personas is not an estimate of anything.

## Layout

```
cmd/populace/       HTTP server: static assets + binary instance endpoint
internal/world/     SoA population, stratified sampling, packing
  world.go          World, strata, weights, encoder
  place.go          weighted population centres (stands in for a GHS-POP raster)
web/                WebGL2 renderer, one draw call
```

## Next

- Phase 2: Go gateway on the Spark (auth, admission control at 8 in flight,
  priority lanes) and Tailscale from Railway.
- Phase 3: CSR social graph, Friedkin-Johnsen opinion updates, complex
  contagion. Not simple contagion — single-exposure cascades overstate reach.
- Phase 4: LOD ladder down to palette-indexed pixel sprites.

## Honest limits

This produces *plausible* reactions, not measured public opinion. Outputs are
synthetic and should be labelled as such wherever they leave the system.

## Phase 2 — the gateway

`cmd/gateway` runs on the machine with the GPU, in front of SGLang.

```sh
go build -o gateway ./cmd/gateway
./gateway -upstream http://127.0.0.1:30000 -in-flight 8 -batch-cap 6 \
          -token "railway:$(openssl rand -hex 24)" -rate 600
```

SGLang has no auth and unbounded queueing: a burst does not get rejected, it
grows the queue until everything times out together. The gateway adds bearer
auth, per-client rate limits, a response cache, and — the important one — hard
admission control at the measured saturation point.

**Admission priority is a reservation, not preemption.** An in-flight
generation cannot be cancelled usefully: the work is already spent and the slot
frees no sooner. So batch traffic is capped at `-batch-cap` of `-in-flight`
slots, guaranteeing an interactive request never queues behind a full house of
batch work. Verified in `gateway_test.go` — 60 concurrent batch requests, and
an interactive one is admitted in 151 ms.

Measured against the real model server: cache hit **4 ms** against a full
generation, and a 503 carries `Retry-After` so clients back off instead of
hammering.

See `deploy/RAILWAY.md` for the Tailscale path from Railway.
