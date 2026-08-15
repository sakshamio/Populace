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

## On Railway

Deploy `deploy/Dockerfile`. Required variables:

| Variable | Value |
|---|---|
| `TAILSCALE_AUTHKEY` | an **ephemeral, pre-authorised** key from the Tailscale admin console |
| `TAILSCALE_HOSTNAME` | `populace-railway` |
| `LLM_GATEWAY_URL` | `http://dgx-spark-k3:8080` (MagicDNS name) |
| `LLM_TOKEN` | the token half of the `-token` pair above |

Use an **ephemeral** auth key. Railway redeploys frequently, and a reusable
key leaves a dead node in the tailnet for every deploy until the list is
unusable.

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
