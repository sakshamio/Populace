# Deploying to Railway, with the model on a Spark behind NAT

The model server lives on a DGX Spark on a residential connection with no root
access and no public IP. Railway reaches it over Tailscale; nothing is exposed
to the internet.

```
Railway container                          DGX Spark (home)
┌──────────────────────────┐               ┌────────────────────────────┐
│ populace  ──ALL_PROXY──► │               │  gateway :8080             │
│ tailscaled (userspace)   │══ tailnet ═══►│    auth, admission ≤8,     │
│   SOCKS5 :1055           │               │    lanes, cache            │
└──────────────────────────┘               │        │                   │
                                           │        ▼                   │
                                           │  sglang :30000  (no auth)  │
                                           └────────────────────────────┘
```

## On the Spark

Everything on this side runs as systemd `--user` units, installed by
`deploy/install-units.sh`. There are four, and the one that keeps getting
forgotten is `tailscaled`:

| unit | what it is |
|---|---|
| `tailscaled` | userspace Tailscale. **Without it the Spark is not on the tailnet at all** |
| `qwen-sglang` | the model server, ~500 s from start to serving |
| `populace-gateway` | auth, admission control, cache |
| `populace` | the simulation (optional here; Railway runs its own) |

After a reboot, anything without a unit is simply gone. That has now happened
twice — once to SGLang and once to `tailscaled` — and in both cases the visible
symptom was on the *other* side of the connection, which is the worst place to
start debugging.

Run the gateway bound to the tailnet interface. SGLang itself stays on
localhost, unauthenticated but unreachable.

```sh
go build -o gateway ./cmd/gateway
./gateway \
  -addr 100.104.179.41:8080 \
  -upstream http://127.0.0.1:30000 \
  -in-flight 8 -batch-cap 6 \
  -token "railway:$(openssl rand -hex 24)" \
  -rate 600 -cache-mb 512
```

`-in-flight 8` is measured, not guessed: on this hardware aggregate throughput
saturates at 8 concurrent requests (99 tok/s) and past that latency doubles for
nothing. Raising it makes the model server worse, not busier.

Keep it running with a systemd `--user` unit and `loginctl enable-linger`,
which works without root. Without lingering it dies with your shell session.

### The gateway must be *served* onto the tailnet

Binding `127.0.0.1:8091` is deliberate — it keeps the gateway off the LAN — but
userspace tailscaled has no TUN device, so the tailnet IP is not a local
interface and a localhost listener is **not reachable from other nodes**. The
bridge is `tailscale serve`:

```sh
tailscale serve --bg --http=8091 http://127.0.0.1:8091
tailscale serve status     # http://dgx-spark-k3:8091 -> proxy 127.0.0.1:8091
```

This is easy to get wrong in a way that looks like a Railway problem: the
gateway is running, `/healthz` answers on localhost, and the client on the
other end times out. The serve config lives in tailscaled's state directory, so
it survives a restart — but a *stale* rule survives too. One pointed at port
8713 (the retired DSV4 API) for weeks after that service was disabled.

## On Railway

`railway.json` at the repo root points the builder at `deploy/Dockerfile`.
**That file is not optional** — see "the 404" below.

Variables. Only the first two are needed to see the app; the rest connect it to
the model.

| Variable | Value |
|---|---|
| `POPULACE_N` | personas, e.g. `250000`. Sized to the instance — 1M costs ~160 MB of heap plus the graph |
| `POPULACE_TICK_MS` | ms between ticks, default `400` |
| `TAILSCALE_AUTHKEY` | an **ephemeral, pre-authorised** key from the Tailscale admin console |
| `TAILSCALE_HOSTNAME` | `populace-railway` |
| `LLM_GATEWAY_URL` | `http://dgx-spark-k3:8091` (MagicDNS name) |
| `LLM_TOKEN` | the token half of the gateway's `name:token` pair |

`PORT` is set by Railway and picked up automatically.

Use an **ephemeral** auth key. Railway redeploys frequently, and a reusable
key leaves a dead node in the tailnet for every deploy until the list is
unusable.

## The 404

The first deploy served `404 page not found` on every path, and the cause is
worth writing down because nothing in the logs points at it.

Railway looks for a `Dockerfile` **at the repository root**. This one lives in
`deploy/`, so Railway did not find it, fell back to Nixpacks, and Nixpacks
found three commands under `cmd/` — `gateway`, `populace`, `simbench` — and
built the wrong one. `cmd/gateway` is a reverse proxy with no static file
serving, so it answers every path except `/v1/chat/completions` and `/healthz`
with exactly that string.

Two things now prevent it:

- `railway.json` names `deploy/Dockerfile` explicitly, so the builder cannot
  fall back to guessing.
- The Dockerfile names `./cmd/populace` explicitly and then asserts
  `/app/web/index.html` exists at build time. An image that cannot find its own
  assets fails the build rather than starting and 404ing.

If you ever see a bare `404 page not found` again, check *which binary* is
running before checking anything else:

```sh
railway logs | head        # the app logs its population and graph at startup;
                           # the gateway logs "admission N in flight"
```

## Verifying the image before pushing it

Build and run it exactly as Railway will. This catches the whole class of
"works locally, 404 in production" without a deploy cycle:

```sh
docker build -f deploy/Dockerfile -t populace-test .
docker run --rm -e PORT=8080 -e POPULACE_N=200000 -p 8099:8080 populace-test

curl -o /dev/null -w '%{http_code} %{content_type}\n' localhost:8099/   # 200 text/html
curl -s localhost:8099/api/stats | head -c 120                           # real numbers
```

## Verifying, in the order that isolates failures

```sh
# 1. Is the container on the tailnet at all?
railway run tailscale status

# 2. Can it see the Spark? (SOCKS5, because there is no kernel route)
railway run curl -sS --proxy socks5h://localhost:1055 \
  http://dgx-spark-k3:8080/healthz

# 3. Does auth work end to end?
railway run curl -sS --proxy socks5h://localhost:1055 \
  -H "Authorization: Bearer $LLM_TOKEN" \
  -H 'X-Populace-Lane: interactive' \
  -H 'content-type: application/json' \
  -d '{"model":"default","messages":[{"role":"user","content":"ping"}],"max_tokens":8}' \
  http://dgx-spark-k3:8080/v1/chat/completions
```

## The failure that wastes an afternoon

**A missing `ALL_PROXY` looks identical to the Spark being down.** Userspace
tailscaled has no TUN device, so the kernel has no route to `100.x`; a request
does not fail fast, it hangs until it times out. `start.sh` sets the variable
and `llm.Client.Check` fails loudly at startup if it is absent, because the
alternative is debugging a timeout that has nothing to do with the model.

Two related notes:

- **You cannot test tailnet reachability from the Spark itself.** In userspace
  mode the machine has no route to its own tailnet IP, so a self-`curl` failing
  proves nothing. Test from another node.
- **`socks5h://`, not `socks5://`,** in curl: the `h` sends DNS through the
  proxy, which is the only way MagicDNS names resolve.

## What happens when the Spark is offline

By design, not much. The simulation clock and the LLM queue are separate
processes: ticks keep advancing, cached archetype reactions keep being applied,
and the UI reports degraded status. Only novel `(archetype, event)` pairs
block, and they queue rather than fail.

`llm.Client` retries a gateway 503 with backoff, honouring `Retry-After`. A 503
is admission control working correctly, not an error — the right response is to
come back, not to give up or to hammer.
