# syntax=docker/dockerfile:1
FROM golang:1.27.3-alpine AS build
WORKDIR /src

# Dependencies before source, so editing a .go file does not re-resolve the module
# graph on every build.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off so the binary is static and can run in a libc-less base. -s -w drop the
# symbol table and DWARF data; VERSION lands in the startup log, which is how you
# tell a running container apart from the build you think is running.
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/dumpster ./cmd/dumpster

FROM alpine:3.22

# ca-certificates: anything the app dials over TLS (e.g. a rediss:// backend).
# tzdata: without a zoneinfo database, TZ is silently ignored and everything looks
# like UTC even when it is not.
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 -h /home/dumpster dumpster

COPY --from=build /out/dumpster /usr/local/bin/dumpster

USER dumpster

# 8080 http + websocket + sse | 8081 grpc | 8082 raw tcp | 8083 raw udp.
# The /udp suffix is load-bearing: a bare "8083" publishes TCP only and datagrams
# vanish without an error anywhere.
EXPOSE 8080 8081 8082
EXPOSE 8083/udp

# Many PaaS health checkers use their own probe and ignore this; docker and
# compose honour it.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/dumpster"]
