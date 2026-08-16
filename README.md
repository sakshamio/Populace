# Populace

A world-scale persona simulation: grounded personas on a real globe, living
daily routines and reacting to events, with a language model authoring the
content and a Go engine doing the per-capita work.

**Plan:** https://claude.ai/code/artifact/cb3b3c4a-f91b-4a80-b24c-f2afbf6f5a7c

## Status — phase 7

Grounded personas, a social network over them, opinion dynamics and contagion,
a media layer of ranked feeds on top of both, a language model writing what each
kind of person makes of the news, a paired experiment runner for testing claims
about any of it, and an instrument panel that shows all of it while it runs.

Three views of the same simulation, switchable with 1/2/3:

| view | what it is for |
|---|---|
| **Globe** | the world at once. Drag to turn, wheel to zoom, click anyone |
| **Map** | the same points under an equirectangular projection, morphed continuously so a cascade stays followable while the world unrolls |
| **Group chat** | six people who grew up in the same place, whose messages are produced by their own state in the running simulation |

Time is a transport: space plays and pauses, `→` steps one tick while paused,
and the speed dial runs 0.25× to 8×. There is deliberately no step *back* —
see "what is not here".

## The media layer

Peer ties spread behaviour; ranked feeds are a structurally different channel,
and two properties make them different in ways that change outcomes rather than
just speed.

**Amplification.** A chronological feed shows a fair sample of what is
circulating, so the chance a slot is about your story is just the engagement
rate `e`. Ranking by predicted engagement over-samples the tail, modelled as
`e^(1/(1+Amplify))` — at `Amplify = 0` the exponent is 1 and the feed is fair;
at 3.4 it is 0.23, so a story circulating among 0.1% of users occupies 21% of
feed slots. The limits are right: as `e → 1` all curves meet, because a feed
cannot over-represent something everybody is already posting. Amplification
helps the obscure, not the universal.

What that buys is not "everything spreads" — it is a **critical mass that
moves**. Measured over 60k personas, scattered seeding:

| seeded | platforms off | platforms on |
|---|---|---|
| 0.4% | 0.46% | 0.53% |
| **1.2%** | **4.12%** | **99.91%** |
| 1.6% | 99.76% | 99.94% |

Below the new threshold platforms barely help; above the old one they are
irrelevant; in between they decide the outcome. A closed channel with no ranking
function does not do this at all, which is the control that separates
"platforms" from "amplification".

Feed confirmations are **not** counted as independent evidence. Content selected
by one ranking function out of one pool is correlated, so `shown` posts are worth
`0.9·√shown` confirmations, capped at 4. Without that discount the first version
of this layer turned every story into a global cascade and erased the tipping
point entirely.

**Sorting** is the echo chamber, and the honest observable is the *peak* of
opinion variance rather than its endpoint. Friedkin-Johnsen has a unique fixed
point; the feed is not in it. Once a story burns out, presence goes to zero and
the population relaxes to exactly where it would have been. So a sorted feed
cannot move the equilibrium — it holds people apart while the story is live.
Measured: peak variance rises 1.57× from chronological to echo-chamber feeds,
and final variance is identical to six decimal places.

## The group chat

Six people who grew up in one place — four still there, two moved abroad — with
a shared chat. Every line traces to a state transition: somebody adopted,
somebody's opinion crossed zero, somebody was reached by a feed. Nothing is
authored for mood.

It exists as an audit. Every number above it is an aggregate over a population,
and an aggregate can look reasonable while the mechanism under it is wrong. If
reach says 99% and nobody in the chat has heard, one of the two is lying and the
chat is the one you can check by reading. A real transcript from a run:

```
t19  Vikram   catching up on this now              (Lahore — moved away)
t24  Paula    anyone else seen this                (Brasilia — moved away)
t24  Yuna     did you all see this. not great      (Guangzhou)
t25  Chen     just heard about this                (Guangzhou)
t28  Ling     ok I'm done with this topic
```

The two who left hear first, because they sit in different media environments.
That is the reason the cohort is not six people in one city.

## What is not here, and why

**Step back.** Rewinding needs a stored copy of every persona per tick: 20 MB a
tick at a million people, 3.6 GB for the 1800-tick history the charts already
keep. The charts are the record of what happened; the world only moves forward.

Runs unattended under systemd `--user` units — see `deploy/install-units.sh`.

```
go run ./cmd/populace -n 1000000
open http://localhost:8080
```

Flags: `-n` personas, `-seed` world seed, `-tick` ms between ticks, `-run`
start ticking immediately, `-addr`, `-web`.

With a model gateway (see phase 2), add:

```
go run ./cmd/populace -gateway http://127.0.0.1:8091 -token "$LLM_TOKEN"
```

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
  opinion.go        Friedkin-Johnsen influence, feed-blended social input
  contagion.go      threshold adoption, seeding strategies, attention decay
  media.go          platforms, engagement ranking, amplification, sorting
  cohort.go         the six-person group chat
  sim.go            tick loop, event injection, adaptive state frames
internal/gateway/   admission control, lanes, response cache, rate limits
internal/llm/       client used from Railway, over the tailnet
web/                WebGL2 renderer, one draw call
```

## Next

- Phase 2: Go gateway on the Spark (auth, admission control at 8 in flight,
  priority lanes) and Tailscale from Railway.
- LOD ladder down to palette-indexed pixel sprites.
- Calibration. The mechanisms are tested; the magnitudes are not. The
  experiment runner is the machinery calibration would use.
- Stratum share should vary by region (see phase 4).
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

## Phase 5 — the model in the loop, and instruments to watch it

### The division of labour is the architecture

The model decides **what a kind of person thinks**; the simulation decides
**how that spreads**. Neither does the other's job. A model asked to predict
aggregate public opinion would be guessing; a simulation asked to invent what a
Chennai market trader makes of a regulatory inquiry would be asserting.

`internal/reaction` walks the 450-archetype manifest through the phase-2
gateway and returns four fields per archetype — stance, salience, share, and
one sentence. Three of them feed the engine and are the *only* things the model
is allowed to set:

- **stance** moves the Friedkin–Johnsen prior, scaled by salience
- **share** lowers the adoption threshold, never below the reinforcement floor
- **line** is for the reader

It writes to *priors*, not opinions. The network still decides what people end
up thinking; writing straight to `Y` would overwrite the dynamics with the
model's guess and make the graph decorative.

### Measured, against a real DGX Spark

| | |
|---|---|
| full pass, 450 archetypes | **488 s** |
| throughput | 60.6 tok/s at concurrency 6 |
| tokens | 29,601 |
| failures | 2 of 450, both recovered on the next run |
| same story again, from cache | **5 s** |
| cache on disk | 228 kB |

Nothing here scales with population — ten million personas and ten thousand
cost the same pass.

### What it produces

> **smallholder farmer / South Asia** — stance +0.00, cares 0.10
> *"I barely care who owns the trucks since I can't even afford to ship my
> harvest to the distant market; the only inquiry that matters is whether the
> monsoon rains will hit on schedule."*

> **market trader / South Asia** — stance +0.50, cares 0.80
> *"If they are squeezing the last drop of profit from the big lines, my
> transport costs are going to spike by next week, so I need to know if I can
> pass that on to the buyers before they switch to the cheaper, slower route."*

The system prompt is blunt about the two failure modes that make generated
persona reactions useless — sounding like a press release, and agreeing with
everything. "Many people are indifferent to most news. A low salience is a
valid answer and often the right one."

### The model corrected the framing

The story was injected with `stance: -0.8` — hostile coverage of a carrier —
and the population moved **+6.27pp in favour**. That is not a bug. Coverage
hostile to a company reads as *good news* to its customers, and the per-
archetype reactions said so. `Event.Stance` is where the coverage sits, not
whether the news is good for anyone; once reactions are applied they override
it per archetype, which is the right resolution because a single scalar cannot
know who benefits.

### Observability

The panel beside the globe is the deliverable, not a status line. It shows:

- **this story** — reach, adopters, and *shift* rather than level, because the
  level is mostly what the population already believed
- **cascade** — reach, new adopters per tick, and opinion shift as sparklines
  drawn on a canvas (both the Artifact and Railway CSPs block chart CDNs, and
  three series of ≤1800 points do not need 200 kB of library)
- **the model** — generated vs from-cache vs failed, live tok/s, elapsed, ETA,
  and the last error in full
- **what people said** — ranked by how much the archetype *cares*, not by how
  strongly they feel; ranking by stance shows only the loudest, which is the
  sampling error the strata exist to avoid
- **by region and by stratum** — reach, opinion shift, and mean ties

That last cut is where the network becomes legible. From one run:

| stratum | pop | reach | moved | mean ties |
|---|---|---|---|---|
| general | 95.1% | 98.85% | +7.45pp | 13.4 |
| nomadic | 0.4% | 98.36% | +3.93pp | 9.8 |
| immigrant | 3.5% | 99.06% | +8.13pp | 14.6 |
| high net worth | 1.0% | 99.04% | +5.27pp | 44.5 |
| media | 0.0% | 100.00% | +60.60pp | 299.9 |

The mean-ties column explains most of the reach column, which is the point of
putting them side by side.

### Three things measurement caught

**The dashboard lied about a finished run.** Throughput was computed against
`time.Now()` forever, so a completed run decayed on screen: 39 tok/s, then 26,
then 13. That is a number about how long you have been looking at the screen.
The clock now stops when the run does.

**Two archetypes came back with over-escaped JSON** — the model escaped the
quotes delimiting its own string value. Repairing only the *delimiters* (a
quote right after a colon, or right before a comma or brace) recovers them
without touching escapes inside the sentence, which a blanket unescape would
eat. A single retry backs it up, because at temperature 0.8 a malformed sample
is a sample, not a property of the archetype.

**The ETA was computed from completions**, which made it lie whenever the cache
was warm — cache hits finish instantly and are not the part that takes time.
It now divides by generations only.

### Operationally

The box rebooted during this phase. Everything came back except the model
server, which has no unit — and notably `dsv4-api.service`, disabled earlier,
stayed down and left its 77 GiB free, which is exactly what disabling it was
for. SGLang takes ~500 s from `docker run` to serving.

## Phase 6 — a result you can defend, an app you can play, a process that stays up

### One run of a cascade model is not a measurement

Threshold dynamics sit near a tipping point, and near a tipping point the same
parameters give 1% reach in one world and 99% in the next. `internal/experiment`
runs **paired designs**: every arm runs on the same worlds and the same graphs
within a replicate, so the difference between arms is not contaminated by the
difference between worlds.

48 runs across 16 worlds, 2.7 s:

| arm | mean reach | 95% CI | p10–p90 | tipped |
|---|---|---|---|---|
| starts in a place | 2.2% | [1.7, 2.9] | 1.4 – 2.4% | 0% |
| starts in a network | 69.9% | [46.9, 87.6] | 3.8 – 99.1% | 69% |
| scattered worldwide | 1.4% | [1.3, 1.4] | 1.3 – 1.5% | 0% |

> network vs place **+67.7pp**, CI [+45.0, +90.7] — significant
> scattered vs place **−0.8pp**, CI [−1.5, −0.3] — significant

The interval and the spread answer different questions and are reported
separately. Near a tipping point the mean is estimated tightly while individual
worlds span everything, and collapsing those into one number is how a bimodal
outcome gets reported as a measurement.

**The seed is part of the result.** Two runs of the same design on different
worlds gave 7.7% and 2.2% mean reach for one arm. `base_seed` is echoed in the
output and shown in the UI, because without it neither number is reproducible
and the gap between them is unexplainable.

### The normal approximation was wrong here

The first version reported mean ± 1.96 SE and produced, on real output, a 95%
interval of **[−3.8%, +19.3%]** for a reach that cannot be negative. That is not
a rounding artefact: reach is bounded in [0,1] and strongly bimodal, so nothing
symmetric around the mean can describe its sampling distribution. It is now a
percentile bootstrap, which makes no distributional assumption and cannot leave
the support of the data, so every bound it reports is an outcome that could
actually happen. `TestIntervalsStayInsideThePossible` pins it, and fails if the
sample it uses stops breaking the normal approximation.

### Playable

- **Click any dot.** The inspector shows who that person is, where they live,
  their ties, their prior, how much they sway with neighbours versus their own
  view, **how many of their neighbours have adopted and how many they need** —
  which is exactly the comparison the threshold rule performs, so an
  individual's behaviour becomes explicable rather than mysterious. It inverts
  the vertex shader exactly rather than approximating it; an approximate
  unprojection returns someone a few degrees away, which reads as the app being
  wrong about who lives where.
- **Compose a story.** Headline, coverage stance, how costly it is to act on,
  media pickup, seeded share, and where it starts.
- **Run an experiment** from four presets, 8–32 worlds, with the intervals
  drawn to a shared scale so arms are comparable by eye.

### Staying up

Three `--user` units, no root: `qwen-sglang`, `populace-gateway`, `populace`.
Plus panic recovery in every HTTP handler and in the tick loop, because a 10M
world costs seconds of CPU and 1.5 GB to rebuild and throwing it away over a
malformed query is the wrong trade by a wide margin. Panics survived are
counted and shown on the dashboard — a process quietly catching panics for a
week looks healthy from outside, which is the situation that counter exists to
prevent.

Verified by killing things:

```
populace  321410 -> 321814  recovered
gateway   322447 -> 322795  recovered
          322795 -> 322975  recovered
          322975 -> 323180  recovered
simulation never stopped ticking
```

### Four things measurement caught

**`StartLimitIntervalSec` in `[Service]` is silently ignored.** It belongs in
`[Unit]`. The restart-loop protection existed only in the comment until
`systemd-analyze verify` said so.

**The first restart budget made the gateway *less* reliable.** Six failures in
300 s at `RestartSec=5` was exhausted in thirty seconds by a port held briefly
during a redeploy, and systemd then refused to start the service at all — a
transient conflict permanently disabling a service is the opposite of
self-healing. The budget now spans a wall-clock window long enough to tell a
transient fault from a broken binary.

**SGLang's `/health` lies.** It answers as soon as the HTTP server is up, which
is minutes before the model can generate — weights still loading, CUDA graphs
still capturing. Anything gating on it sends the first request into a
connection reset. `/health_generate` actually runs the model, and the unit gates
on that.

**`Reset` did not restore thresholds the model had moved.** `ApplyReactions`
lowers per-persona adoption thresholds; `Contagion.Reset` restored state and
step but not thresholds, so the second story a server ever ran was measured on
a population the first story had left behind. It would have contaminated every
experiment arm and nothing in any aggregate would have looked wrong.

### Bounded

The reaction cache is bounded by **stories**, not entries, and evicted whole.
Entries arrive in complete sets of ~450; evicting half a story would leave some
archetypes with an opinion and some without, and the aggregate would silently
be over a biased subset — worse than having none.