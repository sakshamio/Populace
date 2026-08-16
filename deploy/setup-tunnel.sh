#!/usr/bin/env bash
# Publish the model gateway at a stable public hostname via Cloudflare Tunnel.
#
# Why a tunnel rather than a tailnet: the Railway container has no TUN device,
# so reaching a 100.x address there costs a userspace tailscaled, a SOCKS5
# proxy, ALL_PROXY, and an auth key that expires. A tunnel moves all of that to
# this machine, which has none of those constraints, and leaves the Railway
# side as what it always was -- an HTTP client with a bearer token.
#
# Nothing here is inbound: cloudflared dials OUT to Cloudflare and the edge
# routes back down that connection. So this survives dynamic IPs, router
# reboots, and CGNAT, none of which a port forward survives.
#
#   ./deploy/setup-tunnel.sh example.com [subdomain]
#
# Idempotent: re-running it reuses an existing tunnel and rewrites the config.
set -euo pipefail

DOMAIN=${1:-}
SUB=${2:-gateway}
TUNNEL=populace-gateway
CFD=/home/sakshamio/.local/bin/cloudflared
CFDIR=/home/sakshamio/.cloudflared
UNITS=~/.config/systemd/user

if [ -z "$DOMAIN" ]; then
  echo "usage: $0 <domain-on-cloudflare> [subdomain, default: gateway]" >&2
  exit 2
fi
HOST="$SUB.$DOMAIN"

[ -x "$CFD" ] || { echo "cloudflared not at $CFD" >&2; exit 1; }

# 1. Authorise this machine against the Cloudflare account. Prints a URL to
#    open; the browser is where you pick which zone the tunnel may touch. The
#    resulting cert.pem is what authorises `create` and `route dns` below --
#    it is NOT what the running tunnel uses, so it is only needed at setup.
if [ ! -f "$CFDIR/cert.pem" ]; then
  echo "==> authorising with Cloudflare (open the URL it prints)"
  "$CFD" tunnel login
fi

# 2. Create the tunnel, or adopt the existing one. `create` is not idempotent --
#    it errors on a duplicate name -- so ask first.
ID=$("$CFD" tunnel list --output json 2>/dev/null \
     | python3 -c "import json,sys;print(next((t['id'] for t in json.load(sys.stdin) if t['name']=='$TUNNEL'),''))")
if [ -z "$ID" ]; then
  echo "==> creating tunnel $TUNNEL"
  "$CFD" tunnel create "$TUNNEL"
  ID=$("$CFD" tunnel list --output json \
       | python3 -c "import json,sys;print(next(t['id'] for t in json.load(sys.stdin) if t['name']=='$TUNNEL'))")
fi
echo "==> tunnel $TUNNEL is $ID"

# 3. Point the hostname at the tunnel. This writes a proxied CNAME to
#    <id>.cfargotunnel.com, so the origin IP is never in public DNS.
echo "==> routing https://$HOST -> $TUNNEL"
"$CFD" tunnel route dns --overwrite-dns "$TUNNEL" "$HOST"

# 4. Ingress. The catch-all 404 at the end is required -- cloudflared refuses to
#    start without a terminating rule, and the failure reads as a config parse
#    error rather than "you forgot the default".
cat > "$CFDIR/config.yml" <<YAML
tunnel: $ID
credentials-file: $CFDIR/$ID.json

# originRequest applies to every rule below. The gateway holds requests while
# the GPU generates -- a 450-archetype pass runs minutes -- so the default
# 30s origin timeout would cut long generations off mid-stream.
originRequest:
  connectTimeout: 30s
  noTLSVerify: false

ingress:
  - hostname: $HOST
    service: http://127.0.0.1:8091
  - service: http_status:404
YAML
chmod 600 "$CFDIR/config.yml"

# 5. Run it under systemd like everything else on this box.
mkdir -p "$UNITS"
cp /home/sakshamio/Populace/deploy/cloudflared.service "$UNITS/"
systemctl --user daemon-reload
systemctl --user reset-failed cloudflared 2>/dev/null || true
systemctl --user enable --now cloudflared
loginctl enable-linger "$USER" >/dev/null 2>&1 || true

# 6. Prove it end to end rather than declaring success from `systemctl status`.
#    An unauthenticated request MUST come back 401: that response travelling the
#    whole path is what shows the tunnel reaches the gateway and that the gateway
#    is still the thing deciding who gets in.
echo "==> waiting for the edge to pick up the route"
for i in $(seq 1 30); do
  code=$(curl -s -o /dev/null -w '%{http_code}' -m 10 \
         -X POST "https://$HOST/v1/chat/completions" \
         -H 'content-type: application/json' \
         -d '{"model":"q","messages":[{"role":"user","content":"hi"}],"max_tokens":1}' || true)
  if [ "$code" = "401" ]; then
    echo "==> https://$HOST is live and rejecting unauthenticated calls (401)"
    echo
    echo "Set these on Railway:"
    echo "  railway variables --set \"LLM_GATEWAY_URL=https://$HOST\" \\"
    echo "                    --set \"LLM_TOKEN=\$(sed -n 's/^LLM_TOKEN=//p' ~/.config/populace.env)\""
    exit 0
  fi
  sleep 5
done
echo "!! no 401 from https://$HOST after 150s (last code: ${code:-none})" >&2
echo "   check: journalctl --user -u cloudflared -n 50 --no-pager" >&2
echo "   and:   tail -50 /home/sakshamio/cloudflared.log" >&2
exit 1
