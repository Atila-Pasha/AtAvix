FROM golang:1.26.11-alpine AS build
RUN apk add --no-cache build-base
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
COPY internal ./internal
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w -linkmode external -extldflags -static" -o /out/atavix .

FROM alpine:3.23 AS xray
ARG TARGETARCH
RUN apk add --no-cache ca-certificates curl unzip
RUN set -eu; \
    case "$TARGETARCH" in \
        amd64) asset=Xray-linux-64.zip; sha=b651f7e1d9966ee2c1b1ee1bf8b52c72771824d3065dce96bbc8bcc19ed95f82 ;; \
        arm64) asset=Xray-linux-arm64-v8a.zip; sha=64ed6858982a7f5eb85d88b811089063bd4f17561f4c8f44e0f1dc8e6f6430be ;; \
        *) echo "Unsupported architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    curl -fsSLo /tmp/xray.zip --retry 3 "https://github.com/XTLS/Xray-core/releases/download/v26.11.9/$asset"; \
    echo "$sha  /tmp/xray.zip" | sha256sum -c -; \
    unzip -q /tmp/xray.zip xray -d /out; \
    chmod 0755 /out/xray

FROM alpine:3.23
RUN apk add --no-cache ca-certificates && \
    addgroup -g 10001 atavix && adduser -D -H -u 10001 -G atavix atavix && \
    mkdir -p /data /run/secrets && touch /data/.keep /run/secrets/atavix-admin-password && \
    chown -R atavix:atavix /data
COPY --from=build /out/atavix /usr/local/bin/atavix
COPY --from=xray /out/xray /usr/local/bin/xray
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/atavix"]
