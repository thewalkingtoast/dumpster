<p align="center">
  <img src="internal/web/static/icon-192.png" width="96" height="96" alt="Neon dumpster sign">
</p>

# Dumpster

**Throw requests at it and watch them land.** One link accepts HTTP, gRPC,
WebSocket, SSE, raw TCP and raw UDP, and shows everything in a single live,
newest-first list. Dumpsters burn themselves after 3 hours. No accounts, no
signup — **the UUID in the URL is the only secret**.

## ▶ Try it now: [dumpster.toastsnacks.dev](https://dumpster.toastsnacks.dev)

Rent one, paste the link into anything that speaks a protocol, and watch the row
appear. That instance is the same binary this repo builds.

---

## What it is

A debugging sink for things that are hard to watch: webhooks you can't see, a
device that won't tell you what it sent, a gRPC call whose payload you can't
inspect, a WebSocket frame you're sure you're sending. Instead of a packet
capture and a shrug, you get one page with every request in order, each one
expandable: headers, timings, remote address, raw body, and a decoded view when
the content type is JSON, form-urlencoded, multipart or plain text.

One Go process, four listeners, no database, no JavaScript build step.

| Port | Speaks |
| --- | --- |
| `8080` | HTTP, WebSocket, SSE, and the web UI |
| `8081` | gRPC (h2c, or TLS with a certificate) |
| `8082` | raw TCP |
| `8083` | raw UDP |

Anything sent to any of them lands in the same list, tagged with its protocol.

## Quick start

```bash
docker build -t dumpster:local .
docker run -d --name dumpster \
  -p 8080:8080 -p 8081:8081 -p 8082:8082 -p 8083:8083/udp \
  dumpster:local
# → http://localhost:8080
```

Or straight from source, no container:

```bash
go run ./cmd/dumpster          # http://localhost:8080
```

> **`8083` must be published as `8083:8083/udp`.** A bare `-p 8083:8083` publishes
> TCP only, and every datagram is silently dropped by the host before it reaches
> the process. The page just stays empty and nothing anywhere reports an error.

## Using it

Rent a dumpster from the landing page — you get `http://host:8080/d/<uuid>`.

| Protocol | How to reach it |
| --- | --- |
| HTTP | any method, any path under `/d/<uuid>/…` |
| WebSocket | `ws://host:8080/d/<uuid>/ws` — every frame recorded and echoed |
| SSE | `GET /d/<uuid>/sse` listens, `POST /d/<uuid>/sse/emit` makes it send |
| gRPC | `host:8081`, method `/d<uuid-without-dashes>/Anything`, or any method with `x-dumpster-id` metadata |
| TCP | `host:8082` — first line is the uuid, everything after is payload |
| UDP | `host:8083/udp` — first 36 bytes are the uuid, then optionally a pipe character (`0x7C`) |

```bash
ID=<your-uuid>

# http
curl -X POST "http://localhost:8080/d/$ID/anything" \
  -H 'Content-Type: application/json' -d '{"hello":"dumpster"}'

# websocket (needs websocat)
websocat "ws://localhost:8080/d/$ID/ws"

# sse — the terminal listens, the POST makes the dumpster send
curl -N "http://localhost:8080/d/$ID/sse" &
curl -X POST "http://localhost:8080/d/$ID/sse/emit?count=5&data=hello"
wait

# grpc (needs grpcurl; use -insecure instead of -plaintext when the server runs with -tls)
grpcurl -plaintext -import-path examples -proto dumpster.proto \
  -H "x-dumpster-id: $ID" -d '{"payload":"aGVsbG8="}' \
  localhost:8081 dumpster.Ingest/Anything

# tcp
printf '%s\nhello\n' "$ID" | nc localhost 8082

# udp — -u is mandatory, and nothing prints; the row appears in the page
printf '%s|hello' "$ID" | nc -u -w1 localhost 8083

# the whole dumpster as JSON (gzipped if you ask for it)
curl -s --compressed "http://localhost:8080/d/$ID/_records.json" | python3 -m json.tool
```

SSE is deliberately two halves. The dashboard **posts** to emit rather than
opening an `EventSource` itself: a page that both listened and emitted would
receive its own events as rows *and* as streamed messages, and every row would
appear twice.

### The page

- Newest first, prepended live over a WebSocket that reconnects by itself if it drops.
- Every row shows protocol, time to the millisecond, and a self-updating age.
- Click any detail block to copy it. Works on plain HTTP too, where
  `navigator.clipboard` is unavailable and a textarea fallback is used.
- A dark/light switch sits in the header of every page. The choice is remembered per browser
  and applied before the first paint (a blocking `theme.js`, since the CSP forbids
  inline scripts), so there is no flash of the wrong palette.
- Each protocol's copy-paste example is generated for *this* dumpster and follows
  your TLS setting (`wss://` vs `ws://`, `grpcurl -insecure` vs `-plaintext`).

## Configuration

Precedence: **flags → `DUMPSTER_*` environment → defaults**. Every knob has an
environment variable, so nothing needs a config file.

### Listeners

| Flag | Env | Default | Meaning |
| --- | --- | --- | --- |
| `-http` | `DUMPSTER_HTTP_ADDR` | `:8080` | HTTP, WebSocket, SSE and the UI |
| `-grpc` | `DUMPSTER_GRPC_ADDR` | `:8081` | gRPC listener |
| `-tcp` | `DUMPSTER_TCP_ADDR` | `:8082` | raw TCP listener |
| `-udp` | `DUMPSTER_UDP_ADDR` | `:8083` | raw UDP listener |

### Addresses shown to users

Important behind a proxy: the bound address is rarely the address a human should
type. If these are unset, the app derives them from the bind port, which is wrong
the moment something proxies it.

| Flag | Env | Default | Meaning |
| --- | --- | --- | --- |
| `-base-url` | `DUMPSTER_BASE_URL` | derived | external origin for links, e.g. `https://dumpster.example` |
| `-public-grpc` | `DUMPSTER_PUBLIC_GRPC` | derived | address printed in the gRPC example |
| `-public-tcp` | `DUMPSTER_PUBLIC_TCP` | derived | address printed in the TCP example |
| `-public-udp` | `DUMPSTER_PUBLIC_UDP` | derived | address printed in the UDP example |

### TLS

| Flag | Env | Default | Meaning |
| --- | --- | --- | --- |
| `-tls` | `DUMPSTER_TLS` | off | This app is reachable over TLS. Makes gRPC **require** a certificate and makes the page advertise `wss://` |
| `-grpc-tls-cert` | `DUMPSTER_GRPC_TLS_CERT` | unset | PEM certificate for the gRPC listener |
| `-grpc-tls-key` | `DUMPSTER_GRPC_TLS_KEY` | unset | PEM key for the gRPC listener |

`-tls` is the single statement of intent, so the listener and the page cannot
disagree. Three configurations are refused at startup rather than guessed at:
`-tls` with no certificate, a certificate without `-tls`, and half a cert/key pair.
A TLS story that disagrees with itself is worse than a plaintext one that is
honest about it.

### Lifetimes and sizes

| Flag | Env | Default | Meaning |
| --- | --- | --- | --- |
| `-ttl` | `DUMPSTER_TTL` | `3h` | how long a dumpster lives before it is destroyed |
| `-max-body` | `DUMPSTER_MAX_BODY` | `1 MiB` | bytes of body kept per request |
| `-max-bytes-per-dumpster` | `DUMPSTER_MAX_BYTES_PER_DUMPSTER` | `8 MiB` | retained records per dumpster; oldest evicted first |
| `-max-bytes` | `DUMPSTER_MAX_BYTES` | `4 GiB` | retained records across the whole board |
| — | `DUMPSTER_MAX_RECORDS` | `500` | records per dumpster |
| — | `DUMPSTER_MAX_EVENTS` | `500` | events kept per record (SSE messages, WebSocket frames) |
| — | `DUMPSTER_REAP_EVERY` | `1m` | how often expired dumpsters are swept |
| — | `DUMPSTER_CONN_IDLE` | `90s` | idle timeout for raw TCP/WebSocket connections |

### Abuse ceilings

There is no authentication, so every knob here exists so one caller cannot ruin
the party for everyone else. All are per client address.

| Flag | Env | Default | Meaning |
| --- | --- | --- | --- |
| `-max` | `DUMPSTER_MAX_DUMPSTERS` | `50000` | live dumpsters across the whole board |
| `-max-per-ip` | `DUMPSTER_MAX_PER_IP` | `500` | live dumpsters per address |
| `-ingest-per-minute` | `DUMPSTER_INGEST_PER_MINUTE` | `600` | requests/frames/datagrams per address per minute |
| `-max-conns-per-ip` | `DUMPSTER_MAX_CONNS_PER_IP` | `64` | concurrent TCP/WebSocket/SSE sessions per address |
| — | `DUMPSTER_RENT_PER_HOUR` | `10` | how many dumpsters one address may rent per hour |

A rate limit and a connection cap are not the same control, which is why both
exist: a caller who opens connections slowly never trips a rate limit, and a
caller who opens thousands at once is only stopped by the count.

### Client address and proxies

| Flag | Env | Default | Meaning |
| --- | --- | --- | --- |
| `-trust-proxy` | `DUMPSTER_TRUST_PROXY` | off | believe the first `X-Forwarded-For` hop |
| `-trust-real-ip` | `DUMPSTER_TRUST_REAL_IP` | off | believe `X-Real-IP` |

Both are off by default because both are spoofable if the app is reachable
directly. Turn on **exactly one**, and only if the proxy in front overwrites the
header rather than appending to it — see [Deploying](#deploying).

### Forms and CSRF tokens

Renting and emitting SSE are gated by a token embedded in the page, because both
spend a resource on someone's behalf. The token is a timestamped nonce signed with
a secret — no session storage needed.

| Flag | Env | Default | Meaning |
| --- | --- | --- | --- |
| `-csrf-secret` | `DUMPSTER_CSRF_SECRET` | random per process | secret used to sign CSRF tokens; at least 16 bytes |

Leave it unset and every restart invalidates the tokens in open tabs, which shows
up as *"That request did not come from this page"* until the page is reloaded.
Set it when you redeploy often, and **it is required for more than one instance**:
tokens signed by instance A must be accepted by instance B, or every form breaks
the moment a load balancer sends the POST somewhere else.

Pages carrying a token are served `Cache-Control: no-store`, so a reload always
gets a token the running process will accept.

### Storage

| Flag | Env | Default | Meaning |
| --- | --- | --- | --- |
| `-redis` | `DUMPSTER_REDIS_URL` | unset | use Redis instead of in-memory storage |
| — | `DUMPSTER_INSTANCE_ID` | random | instance identity for Pub/Sub self-echo filtering |

In-memory is the default: fast, no dependencies, and gone when the process stops.
Redis buys two things — survival across restarts (only with `appendonly yes`) and
multiple app instances behind a load balancer, since dashboard updates are relayed
over Pub/Sub. `/healthz` returns `503` with the reason when the backend is
unreachable; a dead Redis otherwise makes every dumpster read as "gone".

## Deploying

One binary, four ports. Put a reverse proxy in front of `8080` for TLS, and
publish `8081`–`8083` directly if you want those protocols — no HTTP proxy will
carry them.

1. **Reverse proxy in front of 8080.** Terminate TLS there. WebSocket needs the
   usual `Upgrade`/`Connection` headers passed through (nginx:
   `proxy_http_version 1.1` plus `proxy_set_header Upgrade $http_upgrade;`).
2. **Raise the proxy's idle timeout.** nginx's default `proxy_read_timeout` is
   60 s, which cuts SSE streams and idle WebSockets at exactly a minute. The app
   sends an SSE keep-alive comment every 15 s, so SSE survives; WebSockets need
   the timeout raised (`proxy_read_timeout 3600s;`).
3. **Tell it what it is.** `-tls` (with a certificate for the gRPC port) so the
   page advertises `wss://` and gRPC enforces TLS; `-base-url` and the
   `-public-*` addresses so the examples show the real hostname.
4. **Choose your header.** If the proxy *overwrites* `X-Forwarded-For` with the
   socket address (nginx's `proxy_set_header X-Forwarded-For $remote_addr;`,
   which is what many PaaS nginx templates do), set `-trust-proxy`. If it
   *appends* (the common `$proxy_add_x_forwarded_for`), do **not** trust it — a
   client can prepend its own hop. With nginx, either overwrite it or set
   `X-Real-IP $remote_addr` and use `-trust-real-ip`.
5. **Publish UDP properly.** `8083:8083/udp`. See the warning above; this is the
   single most common "it doesn't work" report.
6. **Keep the container non-root and the certificate readable.** The image runs
   as uid 10001. A certificate mounted `600` and owned by another user is
   unreadable, and the process exits at startup with a clear error rather than
   serving plaintext. Use an ACL or group ownership rather than `chmod 644`.

**If using Cloudflare, ensure your domain is not proxied** (grey cloud / "DNS
only"). Two reasons: the proxy only carries HTTP and HTTPS, so raw TCP and UDP
silently stop working; and it rewrites the client address, so every record shows a
Cloudflare edge IP and your per-IP limits throttle the whole internet as a single
caller. Cloudflare's idle timeout would also cut silent SSE streams.

Health check: `GET /healthz` → `200 {"ok":true}`, or `503` with a reason.

Static assets are requested as `/static/app.js?v=<build>`, and the build stamp is
compiled in. That makes the URLs immutable, so they are served
`Cache-Control: public, max-age=31536000, immutable` and a new binary can never be
paired with the previous stylesheet. Requests without a stamp revalidate
(`no-cache`) rather than being trusted.

## Troubleshooting

| Symptom | Cause | Fix |
| --- | --- | --- |
| UDP rows never appear, no error anywhere | port published TCP-only | publish `8083:8083/udp` |
| Every SSE stream dies after ~60 s | proxy idle timeout | raise the proxy's read timeout; the app already sends keep-alives |
| WebSocket closes when nobody is talking | same idle timeout | raise `proxy_read_timeout` |
| `wss://` fails from an HTTPS page | page advertises `ws://` | run with `-tls` |
| gRPC: `error reading server preface` | TLS client against a plaintext listener | drop `-insecure`, or start the server with `-tls` |
| gRPC: `authentication handshake failed: first record does not look like a TLS handshake` | plaintext client against a TLS listener | add `-insecure` (grpcurl) or `-tls` (`cmd/grpcping`) |
| Process exits: *"-tls is on but grpc needs both a cert and a key"* | TLS on, no certificate | supply both, or turn `-tls` off |
| Process exits: *"grpc tls cert/key set but -tls is off"* | certificate with the switch off | add `-tls` or remove the certificate |
| Process exits: *"grpc tls: no such file"* | container can't read the cert | fix ownership/ACL for uid 10001 |
| `403` from `/sse/emit` | missing or stale CSRF token | use the token from that dumpster's page |
| `403` "That request did not come from this page" on Rent | page is older than the running process, so its token was signed by a previous build | reload; set `DUMPSTER_CSRF_SECRET` so tokens survive deploys |
| Rent works on one instance, 403s behind a load balancer | per-process CSRF secret | set the same `DUMPSTER_CSRF_SECRET` everywhere |
| Rent 403s in Firefox but works in Chrome/curl | the request carried `Origin: null`, which Firefox sends when the page is served `Referrer-Policy: no-referrer` | keep the app's `same-origin` policy; do not override it in the proxy |
| `429` "too many requests" with `Retry-After` | `-ingest-per-minute` exceeded | wait, or raise the limit |
| `429` "Your yard is full" | `-max-per-ip` reached | expire old dumpsters or raise the limit |
| `429` on the landing page | `DUMPSTER_RENT_PER_HOUR` | wait an hour |
| A record has `body_bytes` but `body_skipped: true` | board byte budget | raise `-max-bytes` |
| Body is present but `truncated: true` | 64 KiB preview / `-max-body` cap | raise `-max-body` |
| Every dumpster reads "gone" after a redeploy | in-memory backend | that is the design; use Redis to persist |
| The wrong client IP is recorded | proxy header trust | see step 4 above; check `remote.forwarded_for` and `remote.x_real_ip` |
| Cross-origin `fetch` gets no records | wildcard CORS is scoped to `/d/<uuid>/<subpath>` | request a subpath, not the HTML page |
| Page renders but assets 404 after an upgrade | stale browser cache | hard-refresh (Cmd/Ctrl-Shift-R) |

## Limits, on purpose

- Body and decoded preview are each capped at 64 KiB per record; the true sizes
  are always recorded, and `truncated: true` marks a sample rather than the whole.
  Without the cap a single 1 MiB request cost 2.9 MB resident; with it, ~130 KB.
- Counts do not bound memory — they multiply. Hence the two byte budgets.
  Per-dumpster overrun drops the oldest records; board overrun keeps the record's
  shape (headers, timings, sizes) and skips the body, marked `body_skipped: true`.
  A debug tool that stops recording when things go wrong fails exactly when you
  need it.
- Sizes are approximated from retained strings rather than re-encoding every
  record: encoding on the ingest path would cost more than the memory it measures.

## Security model

There are no accounts. Anyone holding a dumpster URL can read everything sent to
it, and dumpsters are public by design — **do not point production credentials or
customer data at one.** TLS protects the wire in transit; it does not decide who
may look. The controls that matter are the TTL, the per-address ceilings, and
keeping the process behind a proxy you control.

CSRF tokens guard the actions that spend resources (renting, emitting SSE). The
origin check beside them compares a *named* origin against the request's host; an
opaque `Origin: null` is treated as unknown rather than hostile, because Firefox
sends exactly that for a legitimate same-origin form post when the page sets
`Referrer-Policy: no-referrer`. The token is the actual defence, and it is
required either way — which holds only because of one invariant: **a CSRF token is
never written to a response that another origin may read.** Machine paths under
`/d/<uuid>/…` grant wildcard CORS, so a refusal there is JSON, never the HTML
error page; the error page itself drops its token whenever CORS is in play. Without
that, any page on the internet could fetch a token off a 403 and spend it. The
landing page and the debug page cannot be framed (`X-Frame-Options: DENY` plus
`frame-ancestors 'none'`), and wildcard CORS is granted only to
`/d/<uuid>/<subpath>` — the ingest surface — never to the HTML pages that carry a
token.

## Development

```bash
make test     # go test ./... -count=1
make vet      # go vet ./...
make build    # go build ./...
make fmt      # go fmt ./...
make image    # docker build, stamped with the current git SHA
make smoke    # end-to-end against a running instance
make up/down  # local dev container (repo bind-mounted, module cache warm)
```

The icon set (`favicon.ico`, the PNGs, the web manifest) is generated from
`internal/web/static/icon.svg` — a neon dumpster sign drawn as bent tubing. Edit
the SVG, then:

```bash
python3 -m pip install cairosvg    # once
python3 scripts/make-icons.py     # rewrites every raster in static/
```

For visual checks there is `scripts/shots.py`, which photographs both themes at
desktop and phone widths against a running instance (`APP_URL=… python3
scripts/shots.py`). It needs Playwright; if its browser download fails, point
`CHROMIUM` at any Chrome on the machine.

Sizes under 32px render from a filter-free variant: at 16px the glow costs about
half a pixel and turns four tubes into a smear.

Handy companions in `cmd/`: `grpcping` (one gRPC message, no `.proto` needed —
`-tls` for a TLS listener) and `loadgen` (concurrent load across protocols).

The binary stamps its own release at build time; the startup log line reads
`msg="dumpster up" version=<sha>`, which is the fastest way to tell what is
actually running.

## License

MIT — see [LICENSE](LICENSE).
