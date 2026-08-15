# Railway image: the Populace app plus a userspace Tailscale client, so the
# app can reach the gateway on the Spark without anything being exposed to the
# public internet.
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/populace ./cmd/populace

FROM alpine:3.20
RUN apk add --no-cache ca-certificates iptables ip6tables curl
# Pin the Tailscale version: an image that silently tracks latest is an image
# whose networking can change under you between deploys.
ARG TS_VERSION=1.78.1
RUN curl -fsSL "https://pkgs.tailscale.com/stable/tailscale_${TS_VERSION}_amd64.tgz" \
      | tar xzf - --strip-components=1 -C /usr/local/bin \
        tailscale_${TS_VERSION}_amd64/tailscale \
        tailscale_${TS_VERSION}_amd64/tailscaled
COPY --from=build /out/populace /usr/local/bin/populace
COPY web /app/web
COPY deploy/start.sh /usr/local/bin/start.sh
RUN chmod +x /usr/local/bin/start.sh
WORKDIR /app
EXPOSE 8080
CMD ["/usr/local/bin/start.sh"]
