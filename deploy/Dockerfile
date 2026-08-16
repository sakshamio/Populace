# Railway image: the Populace app plus a userspace Tailscale client, so the
# app can reach the gateway on the Spark without anything being exposed to the
# public internet.
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
ARG TARGETARCH
RUN apk add --no-cache ca-certificates iptables ip6tables curl
# Pin the Tailscale version: an image that silently tracks latest is an image
# whose networking can change under you between deploys. TARGETARCH rather than
# a hardcoded amd64 so the image also builds on an arm64 machine -- which is
# what this repo is developed on, so a hardcoded arch means the deploy can only
# ever be tested somewhere else.
ARG TS_VERSION=1.78.1
RUN arch="${TARGETARCH:-amd64}" \
 && curl -fsSL "https://pkgs.tailscale.com/stable/tailscale_${TS_VERSION}_${arch}.tgz" \
      | tar xzf - --strip-components=1 -C /usr/local/bin \
        "tailscale_${TS_VERSION}_${arch}/tailscale" \
        "tailscale_${TS_VERSION}_${arch}/tailscaled"
COPY --from=build /out/populace /usr/local/bin/populace
COPY web /app/web
COPY deploy/start.sh /usr/local/bin/start.sh
RUN chmod +x /usr/local/bin/start.sh
WORKDIR /app

# A container that cannot find its own assets should say so at start rather
# than answering 404 for every request and leaving the reader to guess.
RUN test -f /app/web/index.html || (echo "web assets missing from image" && exit 1)

EXPOSE 8080
CMD ["/usr/local/bin/start.sh"]
