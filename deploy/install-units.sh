#!/usr/bin/env bash
# Install the three user units. No root anywhere: these are --user units and
# lingering is what makes them survive logout and start at boot.
set -euo pipefail

REPO=/home/sakshamio/Populace
UNITS=~/.config/systemd/user

mkdir -p "$UNITS" "$REPO/bin"
go build -o "$REPO/bin/populace" ./cmd/populace
go build -o "$REPO/bin/gateway" ./cmd/gateway

# The gateway token is generated once and kept out of the repository.
ENVFILE=~/.config/populace-gateway.env
if [ ! -f "$ENVFILE" ]; then
  TOKEN=$(openssl rand -hex 24)
  printf 'GATEWAY_TOKENS=populace:%s\n' "$TOKEN" > "$ENVFILE"
  printf 'LLM_TOKEN=%s\n' "$TOKEN" >> ~/.config/populace.env
  chmod 600 "$ENVFILE" ~/.config/populace.env
  echo "generated a gateway token in $ENVFILE"
fi

cp "$REPO"/deploy/{populace,populace-gateway,qwen-sglang,tailscaled}.service "$UNITS/"
systemctl --user daemon-reload

# Without lingering the units die with the login session, which is the failure
# that looks like "it worked yesterday".
loginctl enable-linger "$USER"

systemctl --user enable --now tailscaled qwen-sglang populace-gateway populace

# Expose the gateway on the tailnet. Userspace tailscaled has no TUN device, so
# the tailnet IP is not a local interface and a 127.0.0.1 listener is NOT
# reachable from other nodes -- `serve` is what bridges the two. Without this
# the gateway is up, healthy, and invisible to everything except this machine.
TS=/home/sakshamio/bin/tailscale
SOCK=/home/sakshamio/kimi-k3-inference/.ts/tailscaled.sock
if [ -S "$SOCK" ]; then
  "$TS" --socket="$SOCK" serve --bg --http=8091 http://127.0.0.1:8091 || true
  "$TS" --socket="$SOCK" serve status || true
fi
systemctl --user status --no-pager tailscaled qwen-sglang populace-gateway populace | head -50

# A unit that exhausted its restart budget stays down until the counter is
# cleared, so clear it on every install -- otherwise a redeploy after a bad
# night silently starts nothing.
systemctl --user reset-failed tailscaled qwen-sglang populace-gateway populace 2>/dev/null || true
