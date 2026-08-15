# Populace

A world-scale persona simulation: grounded personas on a real globe, living
daily routines and reacting to events, with a language model authoring the
content and a Go engine doing the per-capita work.

**Plan:** https://claude.ai/code/artifact/cb3b3c4a-f91b-4a80-b24c-f2afbf6f5a7c

## Status — phase 4

Grounded personas, a social network over them, opinion dynamics and contagion,
ticking live behind an HTTP API and a WebGL2 globe.

```
go run ./cmd/populace -n 1000000
open http://localhost:8080
```

Flags: `-n` personas, `-seed` world seed, `-tick` ms between ticks, `-run`
start ticking immediately, `-addr`, `-web`.

`go run ./cmd/simbench` reproduces every performance number quoted below.

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
cmd/populace/       HTTP server: assets, instances, state frames, events
cmd/gateway/        model-server front door (auth, admission control, cache)
cmd/simbench/       reproduces the performance numbers in this README
internal/world/     SoA population, stratified sampling, packing
  world.go          World, strata, weights, encoder
  place.go          weighted population centres (stands in for a GHS-POP raster)
internal/sim/       the dynamics
  graph.go          CSR social network, spatial order, degree tail
  opinion.go        Friedkin-Johnsen influence
  contagion.go      threshold adoption, seeding strategies, attention decay
  sim.go            tick loop, event injection, adaptive state frames
internal/gateway/   admission control, lanes, response cache, rate limits
internal/llm/       client used from Railway, over the tailnet
web/                WebGL2 renderer, one draw call
```

## Next

- Phase 2: Go gateway on the Spark (auth, admission control at 8 in flight,
  priority lanes) and Tailscale from Railway.
- The language model in the loop: `/api/archetypes` is already the generation
  manifest. Walk it once through the phase-2 gateway, cache forever.
- LOD ladder down to palette-indexed pixel sprites.
- Stratum share should vary by region. It currently does not: high-net-worth is
  3% of Lagos and 3% of Zurich, which understates real inequality. The mix
  *within* a stratum does shift correctly; the marginal does not.

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


## Phase 3 — the dynamics

`internal/sim`. Three pieces: a social network, opinions that move along it,
and behaviour that spreads across it. All deterministic given a seed, because a
simulation you cannot replay is a simulation whose surprising result you cannot
investigate.

### Measured, at ten million

| | 1M | 10M |
|---|---|---|
| population | 96 ms | 917 ms |
| social graph + initial state | 231 ms | 2.18 s |
| one tick (contagion + opinion) | 24 ms | 336 ms |
| heap, population and network | 0.15 GB | 1.46 GB |
| ties | 8.3M | 83.2M |

Degree: mean 16.5, p50 13, p90 25, p99 60, max 3490. Media personas average
310 — they are broadcast nodes, and without that tail nothing crosses a
continent and every cascade number is a number about the wrong network.

### Friedkin–Johnsen, not DeGroot

Each agent keeps a stubborn prior it never fully abandons:

    y_i(t+1) = λ_i · mean(neighbours) + (1 − λ_i) · s_i

Under DeGroot averaging (λ = 1) any connected population converges to a single
shared opinion, which is not a property the world has. Measured at equilibrium
on the same graph: **FJ variance 0.074, DeGroot variance 9.5e-17.** The second
number is consensus. The stubborn prior is the entire difference.

Media personas enter as near-immovable broadcasters, which is how an event gets
into the world at all — a story does not change minds directly, it changes what
broadcasters are saying and the network does the rest.

### Complex contagion, and what it costs to get it wrong

Adoption requires *reinforcement* — several independent neighbours, not one
exposure. Same graph, same 200 scattered seeds, same RNG streams, only the
transmission rule differs:

| | adopters | rounds |
|---|---|---|
| simple contagion (β = 0.12) | 58,781 | 16 |
| complex contagion (θ ≈ 0.18) | 220 | 4 |

**Single-exposure spread overstates reach by 267×.** That is why the phase-1
renderer's expanding circle was labelled a placeholder rather than shipped as a
result.

Two consequences fall out of the model rather than being assumed by it:

- **Coverage is not adoption.** Seeding a story outward from the biggest
  broadcaster reaches its ~1,275 neighbours, and they are scattered worldwide
  and unconnected to each other — a star, not a cluster. Every one gets exactly
  one exposure. 3,959 adopters against 59,906 for the same budget started in a
  dense community.
- **Where a story starts beats how loudly it starts.** Share of ties that stay
  inside the seed set, with 400 seeds: a region 24.9%, a network neighbourhood
  7.7%, scattered worldwide 0.5%. Only 4 of 400 scattered seeds begin with the
  two exposures the threshold rule needs.

### The result that makes a sample mean something

Seed the same *fraction* of two populations sixteen times apart in size:

| seeded | 60k adopts | 1M adopts |
|---|---|---|
| 0.1% | 0.118% | 0.117% |
| 0.4% | 0.483% | 0.493% |
| 1.6% | 3.427% | 3.302% |

Reach tracks the seeded fraction, not the seed count — so it is a property of
the modelled network rather than of how many personas happen to be in the
sample. If it were not, every number the app reported would be an artefact of
the sampler.

### Two things measurement changed

**The delta stream was worse than the thing it replaced.** A tick during a live
event dirties ~99% of the population, because the opinion field moves for
almost everyone at once. At six bytes per delta record against two for a full
one, that is 59 MB where a full resend is 20 MB. `Sim.Encode` now picks per
frame, and the client dispatches on the magic. Watched live at 1M: 2.00 MB,
2.00 MB, 1.73, 1.48, 0.27, 0.15, 0.02, 0.00 as the field settles.

**A predicted ordering did not survive contact.** The seeding test first
asserted region > network > scattered. Network seeding grows outward from a
hub, and whether a hub-mediated cascade tips is unstable — at 60k it took the
whole population, at 1M it went nowhere. The test now asserts the mechanism
(reinforcement) rather than an outcome that was a coin flip.

### Determinism is not fastidiousness

The CSR is built with atomic cursors, so within-row order depends on which
goroutine arrived first. Left alone, that makes float accumulation order in the
opinion update scheduling-dependent and two identical runs diverge in the last
bits. Rows are sorted and deduplicated after the build, which fixes the order,
removes duplicate ties that would silently double a neighbour's weight, and
speeds up the gather. `TestGraphIsDeterministic` is what catches a regression.

Both dynamics update synchronously into a second buffer. In-place would be
Gauss-Seidel rather than Jacobi — it converges too, often faster, but it is not
the model, and a cascade could travel an unbounded distance in one tick at a
speed set by memory layout.

### Honest limits, specific to this phase

The network is generated from a plausible mixture of mechanisms — geographic
proximity, archetype homophily, preferential attachment — not measured from
data. Thresholds and susceptibilities are drawn from distributions with the
right shape and uncalibrated parameters. What the tests establish is that the
*mechanisms* behave as the literature says they do; they establish nothing
about the magnitudes. Calibration against observed cascades is the work that
would make a number here quotable.

## Phase 4 — who these people actually are

Until now `Arch[i]` was `rand % 400`: a number with no referent. It is now a
**role crossed with a region**, and both the sprites and the language model
need it to be real.

### 45 roles x 10 regions = 450 archetypes

An archetype is the unit the model writes for, and its size is set by a budget,
not by taste. At the measured 99 tok/s aggregate, 450 archetypes is about ten
minutes of generation for the entire world, once, cached forever. Adding age
bands and education would be more faithful and would also mean the model never
finishes — that trade is the whole reason archetypes exist as a concept rather
than generating every persona individually.

What varies *within* an archetype stays in Go: position, age, opinion prior,
adoption threshold, tie count. Two personas of one archetype are not the same
person; they are people the model would write the same way about.

The taxonomy reaches deliberately from a smallholder farmer to a celebrity
performer, because a simulation stocked only with the comfortable middle answers
every question the same way. `GET /api/archetypes` is the manifest, with live
counts and the exact prompt each cell will be given:

```
[nomadic]        A pastoral herder in South Asia, who moves livestock along
                 routes that are being fenced off.
[immigrant]      A refugee in South Asia, who is waiting on a decision made by
                 people they will never meet.
[high_net_worth] A finance professional in Eurasia, who prices other people's
                 risk and is paid on the outcome.
```

### Roles are conditioned, not assigned

Region comes from where the persona landed; the role from their stratum,
weighted by that region's development index and how urban their spot is.
Exponential in the mismatch rather than filtered, so rare is rare and never
impossible — the tails are where the interesting personas are.

| | Sub-Saharan Africa | North America |
|---|---|---|
| smallholder farmer | 38.2% | 5.9% |
| software engineer | 0.33% | 5.97% |

Within a stratum the mix shifts the way a *conditional* distribution should:
given someone is high-net-worth, wealth in a lower-income region is likelier to
sit in plant and payroll than in a trading book (industrialist 0.69% vs 0.26%;
finance 0.78% NA vs 0.60% SSA). The first version of that test asserted the
opposite and the model was right — it is kept because the wrong intuition is
easy to have twice.

### Two things measurement caught

**Fishers were the third most common job on Earth.** Weighting by region and
urbanity alone made every rural-biased role roughly equally likely: 12,467
fishers against 18,113 smallholder farmers. Occupational frequency is mostly a
fact about the job, not the place, so roles now carry a base prevalence. The
global mix is now 21.8% smallholder farmer — against ~26% of real global
employment in agriculture — and fishers are out of the top twelve.

**Every event reported the same opinion shift.** Three stories with different
stances and difficulties all moved the population about −8pp against and −20pp
for. None of that was the event: the opinion field started away from
equilibrium and simply relaxing toward it swamped everything. `Sim.New` and
`Sim.Reset` now settle the field first, so the "before" state is a population
already at rest on the topic. Same three stories afterwards:

| story | stance | reach | moved |
|---|---|---|---|
| leaked memo, cheap to pass on | −0.9 | 99.9% | +0.40pp against |
| flood-defence fund | +0.9 | 1.1% | +6.26pp for |
| merger, low salience | −0.5 | 1.0% | ±0.02pp |

The UI now reports the shift rather than the level, because the level is mostly
what the population already believed and the story did not cause it.

### Stories differ in what reacting costs

`Event.Difficulty` scales adoption thresholds. Passing on a leaked memo is
nearly free; changing what you buy is not. Without it every story on a given
population tips or fizzles together, which reports a property of the parameters
rather than of the news. Same world, same 1% regional seed:

```
low-cost reaction  (x0.6):  99.94% adopt
high-cost reaction (x2.0):   1.21% adopt
```

### Places have names

186 named centres with regions. An event now breaks *in Pune* rather than at
spatial rank 4,712,883 — and the region is what makes the archetype plausible
rather than generic.

### Measured

Generation slowed 3.3x when role selection added twenty-five exponentials per
persona. Precomputing a cumulative weight table per (stratum, region, urbanity
bucket) turned that into one lookup:

| | 1M | 10M |
|---|---|---|
| population + archetypes | 164 ms | 1.42 s |
| social graph + settle | 310 ms | 4.28 s |
| one tick | 23 ms | 236 ms |
| heap | 0.15 GB | 1.47 GB |

21 tests across `internal/world` and `internal/sim`, clean under `-race`.