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

cp "$REPO"/deploy/{populace,populace-gateway,qwen-sglang}.service "$UNITS/"
systemctl --user daemon-reload

# Without lingering the units die with the login session, which is the failure
# that looks like "it worked yesterday".
loginctl enable-linger "$USER"

systemctl --user enable --now qwen-sglang populace-gateway populace
systemctl --user status --no-pager qwen-sglang populace-gateway populace | head -40

# A unit that exhausted its restart budget stays down until the counter is
# cleared, so clear it on every install -- otherwise a redeploy after a bad
# night silently starts nothing.
systemctl --user reset-failed qwen-sglang populace-gateway populace 2>/dev/null || true
