# Dumpster

Rent a bucket, throw requests at it, watch them land. One link accepts HTTP,
gRPC, WebSocket, SSE, raw TCP and raw UDP, and shows everything in one live,
newest-first list. Dumpsters delete themselves after 12 hours. There is no
account, and no authentication: **the UUID in the URL is the only secret**.

## Run

```bash
make up      # create the docker dev container (once)
make run     # http://localhost:8080
```

Everything runs in a container: `scripts/dev.sh` keeps a `dumpster-dev` container
with the repo bind-mounted and the module cache warm. `./scripts/dev.sh mode` tells
you what it found.

## Test

```bash
make test     # ./scripts/dev.sh go test ./... -count=1
make vet      # ./scripts/dev.sh go vet ./...
make smoke    # end-to-end against a running server inside the container
```

## How a dumpster is addressed

| Protocol | Address |
| --- | --- |
| HTTP | `POST http://host:8080/d/<uuid>/any/path` — any method, any path, body kept |
| SSE | `curl -N 'http://host:8080/d/<uuid>/sse?count=5&interval_ms=1000'` |
| WebSocket | `ws://host:8080/d/<uuid>/ws` — every frame recorded, every frame echoed |
| gRPC | `host:8081`, method `/d<uuid-without-dashes>/Anything`, or any method plus `x-dumpster-id` metadata |
| TCP | `:8082`, first line is the uuid, everything after is payload |
| UDP | `:8083`, first 36 bytes are the uuid, optionally followed by `\|` |

```bash
# http
curl -X POST "http://localhost:8080/d/$ID/anything" -H 'Content-Type: application/json' -d '{"hello":"dumpster"}'

# sse
curl -N "http://localhost:8080/d/$ID/sse?count=5&interval_ms=1000"

# grpc (needs grpcurl)
grpcurl -plaintext -import-path examples -proto dumpster.proto \
  -H "x-dumpster-id: $ID" -d '{"payload":"aGVsbG8="}' \
  localhost:8081 dumpster.Ingest/Anything

# tcp
printf '%s\nhello\n' "$ID" | nc localhost 8082

# udp
printf '%s|hello' "$ID" | nc -u -w1 localhost 8083

# read a dumpster as JSON
curl -s "http://localhost:8080/d/$ID/_records.json" | python3 -m json.tool
```

## Configuration

Flags wins over `DUMPSTER_*` env, env wins over defaults.

| Flag | Env | Default |
| --- | --- | --- |
| `-http` | `DUMPSTER_HTTP_ADDR` | `:8080` |
| `-grpc` | `DUMPSTER_GRPC_ADDR` | `:8081` |
| `-tcp` | `DUMPSTER_TCP_ADDR` | `:8082` |
| `-udp` | `DUMPSTER_UDP_ADDR` | `:8083` |
| `-ttl` | `DUMPSTER_TTL` | `12h` |
| `-max-body` | `DUMPSTER_MAX_BODY` | `1048576` |
| `-base-url` | `DUMPSTER_BASE_URL` | derived from the bound port |
| `-trust-proxy` | `DUMPSTER_TRUST_PROXY` | off |
| — | `DUMPSTER_MAX_DUMPSTERS` | `10000` |
| — | `DUMPSTER_RENT_PER_HOUR` | `10` |

## Limits, on purpose

- 1 MiB of body per request (anything longer is truncated and flagged, never rejected)
- 500 records per dumpster, 500 events per record — oldest dropped first
- 10 000 dumpsters globally, 10 rentals per hour per IP
- Everything is in memory: restarting the process empties the yard

Requests are public to anyone holding the link. Do not dump production secrets
into one.
