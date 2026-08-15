#!/bin/sh
# Bring up userspace Tailscale, then the app.
#
# Userspace mode is not a preference here, it is a requirement: Railway
# containers have no TUN device and no NET_ADMIN, so there is no kernel route
# to 100.x addresses. tailscaled instead offers a local SOCKS5 proxy, and
# ALL_PROXY is what makes Go's http.Transport use it. Without that variable
# every request to the gateway hangs until it times out, which looks exactly
# like the Spark being offline.
set -eu

if [ -n "${TAILSCALE_AUTHKEY:-}" ]; then
  /usr/local/bin/tailscaled \
      --tun=userspace-networking \
      --socks5-server=localhost:1055 \
      --outbound-http-proxy-listen=localhost:1055 \
      --state=mem: &

  # An ephemeral key means the node disappears from the tailnet when the
  # container stops, so Railway's redeploys do not accumulate dead machines.
  /usr/local/bin/tailscale up \
      --authkey="${TAILSCALE_AUTHKEY}" \
      --hostname="${TAILSCALE_HOSTNAME:-populace-railway}" \
      --accept-routes

  export ALL_PROXY=socks5://localhost:1055/
  export HTTP_PROXY=http://localhost:1055/
  export http_proxy=http://localhost:1055/
  echo "tailscale up as ${TAILSCALE_HOSTNAME:-populace-railway}; ALL_PROXY=$ALL_PROXY"
else
  echo "TAILSCALE_AUTHKEY unset -- starting without a tailnet." \
       "The app will run, but the gateway will be unreachable."
fi

exec /usr/local/bin/populace -web /app/web "$@"
