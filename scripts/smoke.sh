#!/usr/bin/env bash
# scripts/smoke.sh — end to end check against a running dumpster server.
# Run it inside the dev container so 127.0.0.1 is the server:
#   ./scripts/dev.sh sh 'bash scripts/smoke.sh'
set -euo pipefail

APP_URL="${APP_URL:-http://127.0.0.1:8080}"
GRPC_ADDR="${GRPC_ADDR:-127.0.0.1:8081}"
TCP_ADDR="${TCP_ADDR:-127.0.0.1:8082}"
UDP_ADDR="${UDP_ADDR:-127.0.0.1:8083}"

say() { printf '\n== %s\n' "$*"; }

say "health"
curl -sS "$APP_URL/healthz" | grep -q '"ok":true'

say "rent a dumpster"
id=$(curl -sS -o /dev/null -w '%{redirect_url}' -X POST "$APP_URL/rent" | sed 's#.*/d/##')
[ "${#id}" -eq 36 ] || { echo "bad id: $id" >&2; exit 1; }
echo "dumpster $id"

say "http (json body decoded server side)"
curl -sS -X POST "$APP_URL/d/$id/anything" \
  -H 'Content-Type: application/json' -d '{"hello":"dumpster"}' | grep -q '"ok":true'

say "sse (three events)"
sse=$(curl -sSN "$APP_URL/d/$id/sse?count=3&interval_ms=1")
[ "$(grep -c 'event: dump' <<<"$sse")" -eq 3 ] || { echo "$sse" >&2; exit 1; }

say "tcp (id on the first line)"
printf '%s\nhello tcp\n' "$id" > "/dev/tcp/${TCP_ADDR%:*}/${TCP_ADDR##*:}"

say "udp (id then a pipe)"
printf '%s|hello udp' "$id" > "/dev/udp/${UDP_ADDR%:*}/${UDP_ADDR##*:}"

say "grpc (passthrough)"
go run ./cmd/grpcping -addr "$GRPC_ADDR" -id "$id" -msg hello | grep -q 'echo: hello'

say "websocket is covered by go test (TestWSIngest*)"

sleep 0.5
say "snapshot has every protocol"
snap=$(curl -sS "$APP_URL/d/$id/_records.json")
for kind in http sse tcp udp grpc; do
  grep -q "\"kind\":\"$kind\"" <<<"$snap" || { echo "missing kind: $kind" >&2; echo "$snap" >&2; exit 1; }
done
echo "recorded: http sse tcp udp grpc"

say "rate limit bites"
codes=""
for _ in $(seq 1 12); do
  codes="$codes $(curl -sS -o /dev/null -w '%{http_code}' -X POST "$APP_URL/rent")"
done
grep -q 429 <<<"$codes" || { echo "expected a 429, got:$codes" >&2; exit 1; }

say "OK"
