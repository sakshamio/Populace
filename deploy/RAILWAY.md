# Deploying to Railway, with the model on a Spark behind NAT

The model server lives on a DGX Spark on a residential connection, with no root
access and no public IP. Railway reaches it over a Cloudflare Tunnel: the Spark
dials **out** to Cloudflare and the edge routes requests back down that
connection, so nothing is port-forwarded and the origin IP never appears in DNS.

```
Railway container                          DGX Spark (home)
┌──────────────────────────┐               ┌────────────────────────────┐
│ populace                 │               │  cloudflared ──dials out──►│
│   ordinary HTTPS client  │               │        │                   │
│   + bearer token         │               │        ▼                   │
└───────────┬──────────────┘               │  gateway :8091 (localhost) │
            │                              │    auth, admission ≤8,     │
            │   https://gateway.<domain>   │    lanes, cache            │
            └──────► Cloudflare edge ──────►        │                   │
                                           │        ▼                   │
                                           │  sglang :30000  (no auth)  │
                                           └────────────────────────────┘
```

## Why not Tailscale

It was Tailscale first, and the transport cost more than the problem was worth.
A Railway container has no TUN device and no `NET_ADMIN`, so there is no kernel
route to a `100.x` address. Making it work meant a userspace `tailscaled` in the
image, a SOCKS5 proxy, `ALL_PROXY` plumbing that Go's transport had to pick up,
and an **auth key with an expiry**. When the key lapsed the container came up
healthy, served the simulation perfectly, and quietly reported the model as not
configured — and a request to a `100.x` address with no route does not fail
fast, it hangs for the full timeout, which is indistinguishable from the Spark
being switched off.

A tunnel moves every one of those constraints to the machine that has none of
them. The Railway side is now what it always should have been: an HTTPS client
with a bearer token, and an image with no networking stack in it.

## On the Spark

Everything runs as systemd `--user` units with `loginctl enable-linger`, which
needs no root. After a reboot, anything without a unit is simply gone — that has
happened twice, and both times the visible symptom was on the *other* side of
the connection, which is the worst place to start debugging.

| unit | what it is |
|---|---|
| `cloudflared` | the tunnel. **Without it the gateway has no public hostname** |
| `qwen-sglang` | the model server, ~500 s from start to serving |
| `populace-gateway` | auth, admission control, cache |
| `populace` | the simulation (optional here; Railway runs its own) |
| `tailscaled` | still present for shell access; **no longer on the Railway path** |

`deploy/install-units.sh` installs all but the tunnel. The tunnel needs a domain,
so it has its own one-time script:

```sh
./deploy/setup-tunnel.sh example.com          # -> https://gateway.example.com
```

It authorises with Cloudflare (prints a URL to open), creates the tunnel, writes
the DNS route and `~/.cloudflared/config.yml`, installs the unit, and then
**proves the path end to end** by requiring an unauthenticated request to come
back `401` from the public hostname. That specific response is the useful one: it
shows the tunnel reaches the gateway *and* that the gateway is still the thing
deciding who gets in. A `502` means the tunnel is up but the gateway is not; a
`530` or a timeout means the tunnel itself is not connected.

### The gateway stays on localhost

`-addr 127.0.0.1:8091` is deliberate and does not change: `cloudflared` connects
from the same machine, so the listener never needs to be on the LAN, let alone
routable. The only public surface is the hostname, and behind it the gateway
answers `401` to anything without a valid bearer token — verified before the
tunnel was ever opened.

`-in-flight 8` is measured, not guessed: aggregate throughput on this hardware
saturates at 8 concurrent requests (99 tok/s) and past that latency doubles for
nothing. Raising it makes the model server worse, not busier.

## On Railway

`railway.json` at the repo root points the builder at `deploy/Dockerfile`.
**That file is not optional** — see "the 404" below.

| Variable | Value |
|---|---|
| `POPULACE_N` | personas, e.g. `250000`. 1M costs ~1.5 GB of heap with the graph |
| `POPULACE_TICK_MS` | ms between ticks, default `400` |
| `LLM_GATEWAY_URL` | `https://gateway.<your-domain>` |
| `LLM_TOKEN` | the token half of the gateway's `name:token` pair |

`PORT` is set by Railway and picked up automatically. There is no auth key, no
hostname, and no proxy variable any more — and nothing here expires.

## The 404

The first deploy served `404 page not found` on every path, and the cause is
worth keeping because nothing in the logs points at it.

Railway looks for a `Dockerfile` **at the repository root**. This one lives in
`deploy/`, so Railway did not find it, fell back to Nixpacks, and Nixpacks found
three commands under `cmd/` — `gateway`, `populace`, `simbench` — and built the
wrong one. `cmd/gateway` is a reverse proxy with no static file serving, so it
answers every path except `/v1/chat/completions` and `/healthz` with exactly
that string.

Two things now prevent it:

- `railway.json` names `deploy/Dockerfile` explicitly, so the builder cannot
  fall back to guessing.
- The Dockerfile names `./cmd/populace` explicitly and then asserts
  `/app/web/index.html` exists at build time. An image that cannot find its own
  assets fails the build rather than starting and 404ing.

If a bare `404 page not found` ever comes back, check *which binary* is running
before checking anything else — the app logs its population and graph at
startup, the gateway logs `admission N in flight`:

```sh
railway logs | head
```

## Verifying the image before pushing it

Build and run it exactly as Railway will. This catches the whole class of
"works locally, 404 in production" without a deploy cycle:

```sh
docker build -f deploy/Dockerfile -t populace-test .
docker run --rm -e PORT=8080 -e POPULACE_N=200000 -p 8099:8080 populace-test

curl -o /dev/null -w '%{http_code} %{content_type}\n' localhost:8099/   # 200 text/html
curl -s localhost:8099/api/stats | head -c 120                          # real numbers
```

## Verifying, in the order that isolates failures

Each step fails differently, which is the point of the order.

```sh
# 1. Is the path up? (from anywhere)
curl -s -o /dev/null -w '%{http_code}\n' https://gateway.<domain>/v1/chat/completions \
  -X POST -H 'content-type: application/json' -d '{}'
#    401 -> tunnel + gateway both fine, and auth is working
#    502 -> something behind the edge is down. Measured: a dead cloudflared and
#           a dead gateway BOTH return 502, so this code does not tell you
#           which. Do not guess -- ask the machine:
#             systemctl --user is-active cloudflared populace-gateway
#    1033/530 -> the DNS record exists but no tunnel has ever connected to it,
#           which is a setup fault rather than an outage. Re-run setup-tunnel.sh.

# 2. Does auth work end to end?
curl -sS https://gateway.<domain>/v1/chat/completions \
  -H "Authorization: Bearer $LLM_TOKEN" \
  -H 'X-Populace-Lane: interactive' \
  -H 'content-type: application/json' \
  -d '{"model":"default","messages":[{"role":"user","content":"ping"}],"max_tokens":8}'

# 3. What does the deployed app think?
curl -s https://populace-production.up.railway.app/api/relay
```

Unlike the tailnet setup, **all of these work from the Spark itself**. In
userspace Tailscale a machine has no route to its own tailnet IP, so a
self-`curl` failed and proved nothing; the tunnel hostname resolves and works
from everywhere, including the origin.

## Self-healing, measured

Both halves were killed deliberately and timed against the public hostname:

| fault | response during | recovery |
|---|---|---|
| `kill -9` the tunnel | `502` | **~5 s**, systemd `Restart=always`, `NRestarts=1` |
| `systemctl stop` the gateway | `502` | **~3 s**, tunnel never dropped |

The second row is why the unit says `Wants=populace-gateway` and not
`Requires=`. On `Requires=`, stopping the gateway would tear the tunnel down
with it, and a tunnel has to re-establish its edge connection and re-advertise
the route — turning a three-second gateway restart into a multi-minute outage.
Keeping the tunnel up through an origin failure is what makes the outage as
short as the thing that actually broke.

## What happens when the Spark is offline

By design, not much. The simulation clock and the LLM queue are separate: ticks
keep advancing, cached archetype reactions keep being applied, and the UI
reports degraded status. Only novel `(archetype, event)` pairs block, and they
queue rather than fail. The server also probes the gateway once at startup and
logs the result — advisory only, because the simulation is the product and it
runs fine with nobody generating opinions.

`llm.Client` retries a gateway 503 with backoff, honouring `Retry-After`. A 503
is admission control working correctly, not an error — the right response is to
come back, not to give up or to hammer.
