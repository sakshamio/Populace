# Railway image: the Populace app, and nothing else.
#
# This image used to also carry a userspace Tailscale client, because reaching
# the gateway meant reaching a 100.x address and a Railway container has no TUN
# device -- which cost a tailscaled process, a SOCKS5 proxy, ALL_PROXY plumbing,
# and an auth key that expired and took the model relay down with it on every
# redeploy. The gateway is now published at a stable public hostname by a
# Cloudflare Tunnel running on the Spark (see deploy/setup-tunnel.sh), so all of
# that moved to the machine that has no such constraints, and what is left here
# is an HTTP client with a bearer token. Keep it that way.
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
ARG TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
# Named explicitly: the module has three commands, and a builder left to guess
# between them picks the gateway, which serves no static files and answers
# every path with "404 page not found". That is exactly what a misconfigured
# deploy looks like from a browser.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o /out/populace ./cmd/populace

FROM alpine:3.20
# ca-certificates is not optional now that the gateway is reached over HTTPS:
# without it every request fails x509 verification, which reads as the Spark
# being offline rather than as a missing trust store.
RUN apk add --no-cache ca-certificates
COPY --from=build /out/populace /usr/local/bin/populace
COPY web /app/web
WORKDIR /app

# A container that cannot find its own assets should say so at start rather
# than answering 404 for every request and leaving the reader to guess.
RUN test -f /app/web/index.html || (echo "web assets missing from image" && exit 1)

EXPOSE 8080
CMD ["/usr/local/bin/populace", "-web", "/app/web"]
