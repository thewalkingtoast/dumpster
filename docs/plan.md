# Dumpster — Multi-Protocol Request Dumpster Implementation Plan

> **For Hermes:** Use `delegated-plan-execution` to implement this plan phase-by-phase.

**Goal:** Build a single-binary Go webapp where one click rents an ephemeral "dumpster" (UUIDv7) that accepts HTTP, gRPC, WebSocket, SSE, TCP and UDP traffic and streams every request into one live, newest-first debug page, auto-destroyed after 12 hours.

**Architecture:** One process, four listeners (`:8080` HTTP + SSE + the browser's WebSocket, `:8081` gRPC, `:8082` raw TCP, `:8083` raw UDP) behind a single `store.Backend` — in-memory by default, Redis when configured — plus a per-dumpster fan-out hub that relays over Redis Pub/Sub when more than one instance runs. Every protocol handler converts its input into one `model.Record`, stores it, and publishes it to every dashboard watching that dumpster.

**Tech Stack:** Go 1.27.3 (stdlib `net/http`, `net`, `mime/multipart`), `github.com/google/uuid` v1.6.0 (UUIDv7), `github.com/coder/websocket` v1.8.15, `google.golang.org/grpc` v1.84.0, and — only for the optional backend — `github.com/redis/go-redis/v9` v9.23.0 (`miniredis` v2.40.0 in tests). Front-end is server-rendered `html/template` plus ~250 lines of vanilla JS: no build step, no framework, no npm.

---

## 1. Current context and assumptions

### 1.1 The repository

`/opt/data/dev/dumpster` (this workspace) is **empty** and is **not a git repository**. Greenfield: no code to read, nothing to port, no conventions to inherit. Every file below is created from scratch.

### 1.2 The machine (verified 2026-10-25 — do not re-derive)

| Fact | Value | Verified by |
| --- | --- | --- |
| Sandbox OS / arch | Linux `7.0.14-linuxkit`, **aarch64** | `uname -a` |
| Sandbox user | `hermes`, uid 10000, **no sudo** | `id` |
| Go toolchain in sandbox | **absent** | `go version` → `command not found` |
| Docker daemon | **present** — `DOCKER_HOST=tcp://docker:2376`, TLS in `/certs/client`, Docker **29.9.2** on Alpine aarch64, overlayfs | `env \| grep DOCKER`, `docker info` |
| Compose | **present** — v5.6.1 | `docker compose version` |
| Daemon sees this filesystem | **yes** — `docker run -v /opt/data/dev/dumpster:/src:ro alpine:3 ls /src` prints the repo | run it |
| `rsync` in the sandbox | **absent** (`/usr/bin/rsync`, `/bin/rsync`, `/usr/local/bin/rsync` all missing, no sudo to install) | `command -v rsync` → exit 1 |
| `tar` in the sandbox | present, `/usr/bin/tar` | `command -v tar` |
| Other docker hosts (unused) | `galadriel` (Docker 29.9.2, arm64, macOS) and `gandalf` (29.9.2, x86_64) both answer over SSH — not needed by any task | `ssh galadriel docker version` |
| `golang:1.27.3` image | exists on Docker Hub, multi-arch incl. arm64; `golang:1.27` also exists | Hub tags API |
| Present in sandbox | `curl`, `git`, `make`, `bash` | `command -v` |
| Absent in sandbox | `go`, `nc`, `ncat`, `socat`, `grpcurl`, `jq`, `sqlite3`, `/usr/bin/time` | `command -v` |

**The consequence that shapes every task:** all Go commands run **inside a Docker container**, never on the sandbox. `scripts/dev.sh` (Task 1) is the single entrypoint; every verification step below starts with `./scripts/dev.sh go …`. Never type bare `go` — it does not exist here.

**One wrapper, one mode.** `scripts/dev.sh` talks to the Docker daemon directly:
it runs a long-lived `dumpster-dev` container with the repo bind-mounted at `/src`,
module and build caches on named volumes, and ports 8080–8083 published. Nothing is
copied, `go.mod`/`go.sum` are edited in place, and `./scripts/dev.sh mode` prints
`socket`.

**There is no `rsync` in the sandbox**, so no task may use it, and no task may reach
for a remote daemon: every `docker`/`docker compose` command in this plan runs
locally. (Both `galadriel` and `gandalf` still answer over SSH if you ever need a
linux/amd64 check, but nothing here depends on them.)

### 1.3 Library versions (verified against proxy.golang.org and pkg.go.dev)

| Module | Version | API depended on | Verified |
| --- | --- | --- | --- |
| `github.com/google/uuid` | v1.6.0 | `uuid.NewV7() (UUID, error)`; `uuid.Parse` accepts dashed **and** bare 32-hex | pkg.go.dev index lists `NewV7` |
| `github.com/coder/websocket` | v1.8.15 | `Accept(w,r,*AcceptOptions)`, `Dial`, `wsjson.Write`, `Conn.Read/Write/CloseRead/SetReadLimit`, `CloseStatus(err)`, `Conn.Subprotocol()` | pkg.go.dev index + README |
| `google.golang.org/grpc` | v1.84.0 | `ForceServerCodec(encoding.Codec) ServerOption`, `UnknownServiceHandler(StreamHandler) ServerOption`, `MethodFromServerStream(stream)`, `NewServer().Serve(lis)` | pkg.go.dev: all present |
| `golang.org/x/net` | v0.61.0 | **indirect only** (grpc dependency) — never imported by us | proxy.golang.org |

`grpc.Server.ServeHTTP` exists but its doc states: *"The provided HTTP request must have arrived on an HTTP/2 connection. When using the Go standard library's server, practically this means that the Request must also have arrived over TLS."* Hence gRPC gets its own listener (§2.4).

### 1.4 Decisions taken without asking (veto now, not after Task 22)

| # | Question | Decision | Why |
| --- | --- | --- | --- |
| D1 | Language | **Go** | stdlib covers HTTP/1.1, raw TCP, raw UDP in one binary; goroutines give per-connection concurrency with no configuration; gRPC and WebSocket are one import each. Node rejected: needs `ws` + `@grpc/grpc-js` + raw `net`/`dgram` and a build step for zero concurrency benefit. Elixir/Rails/Rust rejected: raw TCP/UDP/gRPC in-process is far more work and slower to iterate. |
| D2 | Persistence | **In-memory by default; Redis optional via `-redis`** | The default keeps a single dependency-free binary and is what `go test` and the dev stack use. Redis is the documented path when dumpsters must outlive a redeploy (`appendonly yes`) or be shared across replicas (the pub/sub relay). The `store.Backend` interface (Task 39) is the seam; nothing in the HTTP layer knows which backend it got. |
| D3 | gRPC transport | **Own listener `:8081`, plain `grpc.Server.Serve(lis)`** | Zero experimental APIs, zero `h2c`/`cmux` dependency, standard path. Cost: one extra port. |
| D4 | "Raw" capture | **Reconstruct from the parsed request; do not tee the socket** | We dump remote addr/port, TLS state + SNI, proto, host, method, full multi-value headers, declared vs actual body length, and the exact raw body bytes. Byte-exact wire capture is impossible over TLS and meaningless without per-request boundaries on keep-alive. Listed as not-done in §7. |
| D5 | TCP addressing | **First `\n`-terminated line of the connection is the dumpster id** | One port, no per-dumpster listeners, trivially scriptable. |
| D6 | UDP addressing | **First 36 bytes of each datagram are the dumpster id**, then an optional `\|`, `\r`, `\n` or space | Datagrams are self-describing, so many dumpsters share one port. |
| D7 | SSE semantics | **Inbound**: `GET /d/{id}/sse?count=&interval_ms=` streams events back to the caller and records the connection plus every emitted event. The debug page's **Trigger SSE** button opens a real `EventSource` | Satisfies "SSE must be triggered from the app" while keeping SSE a dumped protocol, not a special case. |
| D8 | Abuse controls | **No auth** (as specified), but: 10 rents/hour/IP, 10 000 dumpsters globally, 1 MiB body cap, 500 records/dumpster, 500 events/record | A public unauthenticated endpoint that allocates memory on `POST` is an invitation; these caps are the minimum to keep it alive. |
| D9 | Front-end | **Vanilla JS, no framework, no build step** | One page, one list, one socket. React/Vite triples the file count for nothing. |
| D10 | Reverse proxy | `-trust-proxy` flag (default **off**): when on, the first `X-Forwarded-For` hop becomes `remote.ip`, while the raw `RemoteAddr` and all headers are still dumped | Deployed behind a proxy; without this every remote IP reads the proxy's address and the tool lies. |

---

## 2. Architecture

### 2.1 One sentence

Four listeners feed one `*store.Store` (mutex-sharded map of dumpsters → ring-buffered records) and one `*hub.Hub` (per-dumpster subscriber fan-out); the browser holds one dashboard WebSocket per open debug page and renders whatever the hub pushes.

### 2.2 Diagram

```
                      ┌──────────────────────────────────────────────┐
  curl/fetch ────────►│ :8080  net/http                              │
  browser  ──────────►│  ├ GET  /              index                 │
  EventSource ───────►│  ├ POST /rent          → 303 /d/{id}         │
  browser WS ────────►│  ├ GET  /d/{id}        debug page (shell)    │
                      │  ├ GET  /d/{id}/_live  dashboard WebSocket   │
                      │  ├ GET  /d/{id}/sse    SSE ingest (D7)       │
                      │  ├ *   /d/{id}/ws      WS ingest             │
                      │  └ *   /d/{id}/...     HTTP ingest           │
  grpcurl ───────────►│ :8081  grpc.Server (raw passthrough codec)   │
  nc/netcat ─────────►│ :8082  raw TCP  (id on first line)           │
  nc -u / datagram ──►│ :8083  raw UDP  (id in first 36 bytes)       │
                      └───────────────┬──────────────────────────────┘
                                      ▼
                    ┌────────────────────────────────────┐
                    │  app.record(kind, …) → model.Record │
                    └────────────┬───────────────────────┘
                                 ▼
              ┌──────────────────┴───────────────────┐
              ▼                                      ▼
   store.Store (records, TTL)                 hub.Hub (fan-out)
   map[id]*entry{mu, records[]}                map[id]map[chan][]byte
              │                                      │
              ▼                                      ▼
   /_records.json snapshot                 dashboard WebSocket pushes
```

### 2.3 Routing table (exact)

| Method | Path | Handler | Behaviour |
| --- | --- | --- | --- |
| GET | `/` | `handleIndex` | Landing page, single CTA "Rent a dumpster" |
| POST | `/rent` | `handleRent` | Rate-limit → `store.Create()` → `303 See Other` to `/d/{id}` |
| GET | `/healthz` | `handleHealth` | `{"ok":true}` |
| GET | `/static/…` | `http.FileServer` | Embedded `app.js`, `style.css` |
| GET | `/d/{id}` | `handleDumpsterPage` | Debug page shell (404 page if gone) |
| GET | `/d/{id}/_records.json` | `handleRecordsJSON` | `{"dumpster":{…},"records":[…]}` newest-first |
| GET | `/d/{id}/_live` | `handleLive` | Dashboard WS: `snapshot`, then `new`/`update`/`expired` |
| GET | `/d/{id}/sse` | `handleSSEIngest` | `text/event-stream`; records connection + each event |
| any | `/d/{id}/ws` | `handleWSIngest` | WebSocket upgrade; records session + every received frame, echoes frames back |
| any | `/d/{id}/…` (not the four reserved suffixes) | `handleHTTPIngest` | Dump any method/path/body |
| gRPC | `/d{hex}/{Method}` on `:8081` | `handleGRPCStream` | Raw passthrough; id also accepted via `x-dumpster-id` metadata |
| TCP | any bytes | `handleTCPConn` | First `\n`-terminated line = dumpster id |
| UDP | any datagram | `handleUDPDatagram` | First 36 bytes = dumpster id |

**Reserved subpaths under `/d/{id}`:** `ws`, `sse`, `_live`, `_records.json`. A client cannot dump an HTTP request at exactly those four paths — everything else is fair game, including `/`, `/anything`, `/v1/orders/42`, and any query string.

### 2.4 Rejected: one port for gRPC

`grpc.Server.ServeHTTP` would let gRPC share `:8080` behind `if r.ProtoMajor == 2 && strings.HasPrefix(ct, "application/grpc")`. Rejected because (a) the API is marked `Experimental`, (b) plaintext HTTP/2 requires wrapping the handler in `h2c.NewHandler` — an extra import and a subtle failure mode where HTTP/1.1 clients hitting a gRPC port get a confusing 505, and (c) it buys nothing: the tool already owns four ports, and a dedicated `:8081` is ~12 lines of boring code.

### 2.5 Record model — what one row means

Mechanical rule, so the UI never guesses: **connection-oriented protocols produce one record per connection; message-oriented protocols produce one record per message.**

| Kind | Rows | `events[]` holds |
| --- | --- | --- |
| `http` | one per request | empty |
| `grpc` | one per RPC | one event per received message |
| `sse` | one per stream | one event per emitted `event:` |
| `ws` | one per connection | one event per received frame |
| `tcp` | one per connection | one event per read chunk |
| `udp` | **one per datagram** | exactly one event (the payload) |

### 2.6 Lifecycle

1. `POST /rent` → `store.Create()` → `model.Dumpster{ID, CreatedAt, ExpiresAt = now+12h}` under a fresh UUIDv7.
2. Ingest → `store.Add(id, rec)` assigns a per-dumpster monotonic `Seq`, trims to 500 records, returns the stored record → `hub.Publish(id, {type:"new",record})`.
3. Long-lived protocols → `store.AppendEvent(id, seq, ev)` → `hub.Publish(id, {type:"update",record})` (whole record, so the browser re-renders one row without diffing).
4. `reapLoop` ticks each minute → `store.DeleteExpired(now)` → `hub.Close(id)`, closing every subscriber channel; each dashboard socket sees `!ok`, sends `{"type":"expired"}`, closes.
5. `store.Get` independently refuses expired ids, so a dumpster is invisible to clients the instant `ExpiresAt` passes, even before the reaper ticks.

**Shared mode (Redis, §13A).** The same five steps with the store swapped: `LPUSH`+`LTRIM` replace the slice, `INCR` replaces the counter, and `EXPIREAT` — not the sweeper — is what actually deletes. `reapLoop` stays, but only to notice that a key has gone and close its subscribers. `hub.Publish` additionally relays over Pub/Sub, so a page open on *any* instance receives the row.

### 2.7 File tree (every file created)

```
.
├── .gitignore
├── Makefile
├── README.md
├── Dockerfile
├── compose.yaml                  # runnable stack: app + redis (Task 35, extended by 43)
├── compose.dev.yaml              # local stack: redis + mounted toolchain container (35b)
├── go.mod
├── go.sum
├── scripts/
│   ├── dev.sh                    # THE dev loop (docker socket | ssh | native)
│   └── smoke.sh                  # end-to-end, curl + bash only
├── cmd/
│   ├── dumpster/main.go          # flags/env → app.Run
│   ├── grpcping/main.go          # one-shot gRPC client (verification)
│   └── loadgen/main.go           # concurrency proof
├── examples/
│   └── dumpster.proto            # for grpcurl
└── internal/
    ├── app/
    │   ├── app.go                # App struct, Start/Shutdown, listeners
    │   ├── config.go             # Config + DefaultConfig + env parsing
    │   ├── routes.go             # ServeMux wiring + catch-all dispatch
    │   ├── pages.go              # index/debug/error rendering, /rent
    │   ├── live.go               # dashboard WebSocket
    │   ├── ingest_http.go        # HTTP/any-method ingest
    │   ├── ingest_sse.go         # SSE ingest
    │   ├── ingest_ws.go          # WebSocket ingest
    │   ├── ingest_grpc.go        # raw passthrough gRPC
    │   ├── ingest_tcp.go         # raw TCP
    │   ├── ingest_udp.go         # raw UDP
    │   ├── reaper.go             # 12 h sweeper
    │   ├── http_util.go          # remote/timing/body/JSON helpers, headers
    │   ├── app_test.go
    │   ├── ingest_http_test.go
    │   ├── ingest_sse_test.go
    │   ├── ingest_ws_test.go
    │   ├── ingest_grpc_test.go
    │   ├── ingest_tcp_test.go
    │   ├── ingest_udp_test.go
    │   ├── live_test.go
    │   └── reaper_test.go
    ├── decode/{decode.go,decode_test.go}
    ├── hub/{hub.go,hub_test.go,redis.go,redis_test.go}
    ├── id/{id.go,id_test.go}
    ├── model/model.go
    ├── rawproto/rawproto.go      # shared Message + Codec (app + tests + cmd)
    ├── ratelimit/{ratelimit.go,ratelimit_test.go}
    ├── redisstore/{redisstore.go,redisstore_test.go}   # optional backend (Task 40)
    ├── store/{store.go,backend.go,backend_test.go,store_test.go}
    └── web/
        ├── web.go                # embed.FS + template set
        ├── templates/{index.html,debug.html,error.html}
        └── static/{app.js,style.css}
```

---

## 3. Conventions for the implementer

### 3.1 Every Go command goes through the wrapper

```bash
./scripts/dev.sh mode                        # prints: socket
./scripts/dev.sh up                          # create the dev container (once per machine)
./scripts/dev.sh go version                  # → go version go1.27.3 linux/arm64
```

There is no `go` binary on this sandbox. A bare `go` fails with `command not found`; that is not a bug, it is the environment.

### 3.2 Test, build, vet recipes

```bash
./scripts/dev.sh go build ./...              # prints nothing
./scripts/dev.sh go vet ./...                # prints nothing
./scripts/dev.sh go test ./... -count=1      # → ok  <pkg>  0.00Xs   (one line per package)
```

`-count=1` defeats the test cache. Never quote a `(cached)` line as proof of anything.

### 3.3 Where the app runs, and which URL to use

The dev container publishes `8080–8083` to the Mac.

| Who | URL |
| --- | --- |
| A local browser | `http://localhost:8080` |
| Anything run *inside* the dev container (tests, `scripts/smoke.sh`) | `http://127.0.0.1:8080` |
| The sandbox shell | not guaranteed — published ports land on the **daemon's** host, which is not necessarily this sandbox. Run the check inside the container instead (`dev.sh sh 'curl …'`) or `docker exec` into it. |

Default to **running verification inside the container** — `./scripts/dev.sh sh 'bash scripts/smoke.sh'` — which needs no cross-host networking and is deterministic.

### 3.4 Commit style

One commit per task, conventional prefixes, imperative mood:

```
chore: add docker dev wrapper and module skeleton
feat(store): add uuidv7 dumpster store with record caps
feat(app): dump http requests with decoded bodies
test(app): cover sse stream recording
```

Git identity: after `git init`, run `git config user.email`. If empty, set it **locally**: `git config user.email "you@example.com"` and `git config user.name "Adam"`.

### 3.5 House rules

- **No `init()`, no package-level mutable state**, except the two `//go:embed` vars in `internal/web/web.go`.
- **No panics** outside `template.Must` at startup.
- **`textContent`, never `innerHTML`,** for anything derived from a request. A dumpster is an XSS delivery mechanism by design.
- **Every handler writes its own status.** No middleware that rewrites status codes afterwards.
- Functions stay under ~60 lines; where this plan needs more, it already defines the helper.

---
## 4. Phase 0 — Dev loop and repo skeleton

**Phase gate:** `./scripts/dev.sh go version` prints `go version go1.27.3 linux/arm64`, and `git log --oneline` shows **three** commits — Task 1's script lands inside Task 2's commit, exactly as Task 1 instructs, so Tasks 1-4 produce three commits, not four.

---

### Task 1 — `scripts/dev.sh`, the single dev entrypoint

**Objective:** One script that runs any Go command inside a container against the local daemon (or natively, if a Go toolchain ever appears).

**Files:**
- Create: `scripts/dev.sh`

**Step 1: Write the script**

```bash
#!/usr/bin/env bash
# scripts/dev.sh — the only way to run Go in this repo.
#
#   ./scripts/dev.sh mode                        # socket | native
#   ./scripts/dev.sh up                          # create the dev container (once)
#   ./scripts/dev.sh down                        # remove it (and the compose stack)
#   ./scripts/dev.sh go version
#   ./scripts/dev.sh go test ./... -count=1
#   ./scripts/dev.sh sh 'bash scripts/smoke.sh'  # shell inside the container
#
# Mode, auto-detected (override with DEV_MODE=socket|native):
#   socket  a docker daemon is reachable -> bind-mount this repo into golang:*
#   native  a local `go` binary          -> run it
set -euo pipefail
cd "$(dirname "$0")/.."

MODE="${DEV_MODE:-auto}"
GO_IMAGE="${GO_IMAGE:-golang:1.27.3}"
NAME="${DEV_CONTAINER:-dumpster-dev}"
# The repo path as the DOCKER DAEMON sees it. The daemon here shares this
# filesystem, so the shell's own pwd is right; override only for a remote one.
SRC_DIR="${SRC_DIR:-$PWD}"
PORT_MAP="${PORT_MAP:--p 8080:8080 -p 8081:8081 -p 8082:8082 -p 8083:8083/udp}"

log() { printf '[dev] %s\n' "$*" >&2; }

if [ "$MODE" = auto ]; then
  if docker info >/dev/null 2>&1; then
    MODE=socket
  elif command -v go >/dev/null 2>&1; then
    MODE=native
  else
    log "no docker daemon and no go binary — cannot build"
    exit 1
  fi
fi

case "${1:-}" in
  mode) echo "$MODE"; exit 0 ;;

  up)
    case "$MODE" in
      native)
        log "native go: $(go version | awk '{print $3}')"; exit 0 ;;
      socket)
        # Once compose.dev.yaml exists it owns the stack (and brings Redis with it).
        if [ -f compose.dev.yaml ]; then
          SRC_DIR="$SRC_DIR" docker compose -f compose.dev.yaml up -d
          log "dev stack up (compose)"
          exit 0
        fi
        if docker inspect "$NAME" >/dev/null 2>&1; then
          docker start "$NAME" >/dev/null 2>&1 || true
          log "container $NAME already exists"
          exit 0
        fi
        docker volume create dumpster-mod >/dev/null
        docker volume create dumpster-gocache >/dev/null
        # Every -v must precede the image name. Anything after the image is the
        # container's command, so a trailing -v makes `sleep` die with
        # "invalid time interval '-v'" and the container exits before you look.
        docker run -d --name "$NAME" $PORT_MAP \
          -w /src \
          -v "$SRC_DIR:/src" \
          -v dumpster-mod:/go/pkg/mod \
          -v dumpster-gocache:/root/.cache/go-build \
          -e CGO_ENABLED=0 -e GOFLAGS=-mod=mod \
          "$GO_IMAGE" sleep infinity >/dev/null
        log "created $NAME from $GO_IMAGE (src=$SRC_DIR)"
        exit 0 ;;
    esac ;;

  down)
    case "$MODE" in
      native)
        log "nothing to tear down in native mode"; exit 0 ;;
      socket)
        if [ -f compose.dev.yaml ] && [ -n "$(docker compose -f compose.dev.yaml ps -q 2>/dev/null)" ]; then
          # no -v: the go-mod/go-cache volumes are the build cache, and wiping
          # them makes the next up re-download every module.
          SRC_DIR="$SRC_DIR" docker compose -f compose.dev.yaml down
        fi
        docker rm -f "$NAME" >/dev/null 2>&1 || true
        log "removed $NAME"
        exit 0 ;;
    esac ;;

  go)
    shift
    case "$MODE" in
      native) exec go "$@" ;;
      socket) exec docker exec -w /src "$NAME" go "$@" ;;
    esac ;;

  sh)
    shift
    cmd="${1:-bash}"
    # -t only when we actually have a terminal: `docker exec -it` refuses to run
    # from a non-interactive caller, which is how scripts and CI invoke this.
    tty=""
    [ -t 0 ] && tty="-t"
    case "$MODE" in
      native) exec bash -c "$cmd" ;;
      socket) exec docker exec -i $tty -w /src "$NAME" bash -c "$cmd" ;;
    esac ;;

  *)
    sed -n '2,14p' "$0"; exit 2 ;;
esac
```

**Step 2: Make it executable and detect the mode**

```bash
chmod +x scripts/dev.sh
./scripts/dev.sh mode
```

Expected: exactly one word, `socket`. The sandbox has a reachable Docker daemon
(`DOCKER_HOST=tcp://docker:2376`, TLS, Docker 29.9.2 on Alpine aarch64) that shares
this filesystem, so a bind mount of `$PWD` is visible to it.

**Step 3: Create the container**

```bash
./scripts/dev.sh up
```

Expected (the `golang:1.27.3` layers may already be cached; if not, allow 60 s):

```
[dev] created dumpster-dev from golang:1.27.3 (src=/opt/data/dev/dumpster)
```

Second run prints `[dev] container dumpster-dev already exists`. If instead the
container is listed by `docker ps -a` as `Exited (1)`, the `-v` flags drifted after
the image name — see the comment in the `up` branch.

**Step 4: Prove the toolchain**

```bash
./scripts/dev.sh go version
```

Expected:

```
go version go1.27.3 linux/arm64
```

If it says `manifest unknown` or `not found`, retry with
`GO_IMAGE=golang:1.27 ./scripts/dev.sh up` — but `golang:1.27.3` resolves today,
verified by running `docker run --rm golang:1.27.3 go version`.

**Do not commit yet** — Task 2 puts this commit together with the repo skeleton, so the very first commit is a working checkout rather than a loose script.

---

### Task 2 — `.gitignore`, `git init`, module

**Objective:** A git repo whose first commit is a buildable module.

**Files:**
- Create: `.gitignore`
- Create: `go.mod` (generated)

**Step 1: Write `.gitignore`**

```gitignore
# build output
/dumpster
/bin/
*.test
*.out

# local plan/scratch — never shipped
.hermes/

# macOS
.DS_Store
```

**Step 2: Init the repo and the module**

```bash
git init -b main
git config user.email  # if this prints nothing:
# git config user.email "you@example.com" && git config user.name "Adam"
./scripts/dev.sh go mod init github.com/adam/dumpster
```

**Step 3: Verify `go.mod` came back to the sandbox**

```bash
cat go.mod
```

Expected (the `go` directive may read `1.27.3` or `1.27`):

```
module github.com/adam/dumpster

go 1.27.3
```

No copy step is involved: the file is written inside the container onto the bind
mount, so it appears here the instant `go mod init` returns. If it is missing, the
container is not mounting the repo — check with
`docker inspect dumpster-dev --format '{{json .HostConfig.Binds}}'` and expect
`["…/dumpster:/src", …]`.

**Step 4: Commit both tasks**

```bash
git add .gitignore go.mod scripts/dev.sh
git commit -m "chore: add docker dev wrapper and module skeleton"
```

---

### Task 3 — `Makefile`

**Objective:** Short names for the four commands used constantly.

**Files:**
- Create: `Makefile`

**Step 1: Write it (recipe lines must start with a real TAB, not spaces)**

```make
SHELL := /bin/bash
DEV   := ./scripts/dev.sh

.PHONY: up test vet build run fmt tidy smoke

up:    ; $(DEV) up
test:  ; $(DEV) go test ./... -count=1
vet:   ; $(DEV) go vet ./...
build: ; $(DEV) go build ./...
run:   ; $(DEV) go run ./cmd/dumpster
fmt:   ; $(DEV) go fmt ./...
tidy:  ; $(DEV) go mod tidy
smoke: ; $(DEV) sh 'APP_URL=http://127.0.0.1:8080 bash scripts/smoke.sh'
```

**Step 2: Verify the wiring without running anything**

```bash
make -n test
```

Expected:

```
./scripts/dev.sh go test ./... -count=1
```

`make -n` prints without executing, so it works even before any `.go` file exists.

**Step 3: Commit**

```bash
git add Makefile
git commit -m "chore: add make targets for the docker dev loop"
```

---

### Task 4 — Pin the three dependencies

**Objective:** `go.mod` requires exactly the versions verified in §1.3.

**Files:**
- Modify: `go.mod`, `go.sum`

**Step 1: Add the modules**

```bash
./scripts/dev.sh go get \
  github.com/google/uuid@v1.6.0 \
  github.com/coder/websocket@v1.8.15 \
  google.golang.org/grpc@v1.84.0
```

Expected: no output on success (module lines already exist), and `go.sum` appears locally.

**Step 2: Verify**

```bash
./scripts/dev.sh go list -m all | grep -E 'coder/websocket|google/uuid|google.golang.org/grpc '
```

Expected (three lines; grpc's transitive deps are also listed by `list -m all`, which is why the grep is anchored):

```
github.com/coder/websocket v1.8.15
github.com/google/uuid v1.6.0
google.golang.org/grpc v1.84.0
```

**Step 3: Commit**

```bash
git add go.mod go.sum
git commit -m "chore: pin uuidv7, websocket and grpc dependencies"
```

---

## 5. Phase 1 — Domain core (model, ids, store, hub, decode)

**Phase gate:** `./scripts/dev.sh go test ./internal/... -count=1` prints `ok` for `store`, `id`, `hub`, `decode`.

> Every task here is a pure library package with no HTTP in sight. That is deliberate: the protocol handlers in Phases 2–8 are thin precisely because these five packages already own the tricky parts.

---

### Task 5 — `internal/model/model.go`

**Objective:** One shared data shape for all six protocols.

**Files:**
- Create: `internal/model/model.go`
- Test: `internal/model/model_test.go`

**Step 1: Write the failing test**

```go
package model

import "testing"

func TestKindValid(t *testing.T) {
	for _, k := range []Kind{KindHTTP, KindGRPC, KindWS, KindSSE, KindTCP, KindUDP} {
		if !k.Valid() {
			t.Fatalf("%q should be valid", k)
		}
	}
	if Kind("smtp").Valid() {
		t.Fatal("smtp is not a supported kind")
	}
}
```

**Step 2: Run it, expect failure**

```bash
./scripts/dev.sh go test ./internal/model -count=1
```

Expected: `FAIL	github.com/adam/dumpster/internal/model [setup failed]` / `undefined: Kind`.

**Step 3: Write the implementation**

```go
// Package model defines the wire and storage shape shared by every protocol.
package model

import "time"

// Kind is the protocol a record arrived on.
type Kind string

const (
	KindHTTP Kind = "http"
	KindGRPC Kind = "grpc"
	KindWS   Kind = "ws"
	KindSSE  Kind = "sse"
	KindTCP  Kind = "tcp"
	KindUDP  Kind = "udp"
)

// Valid reports whether k is one of the six protocols this app dumps.
func (k Kind) Valid() bool {
	switch k {
	case KindHTTP, KindGRPC, KindWS, KindSSE, KindTCP, KindUDP:
		return true
	}
	return false
}

// Dumpster is one rented bucket. Its id is the only access control there is.
type Dumpster struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Remote is who connected and how.
type Remote struct {
	Addr      string `json:"addr"` // "ip:port" exactly as the listener saw it
	IP        string `json:"ip"`
	Port      int    `json:"port"`
	TLS       bool   `json:"tls"`
	TLSSNI    string `json:"tls_sni,omitempty"`
	Forwarded string `json:"forwarded_for,omitempty"` // raw X-Forwarded-For, always
}

// Timing is microseconds at the points named.
type Timing struct {
	ConnWaitUS int64 `json:"conn_wait_us"` // accept -> handler entry; 0 where unknown
	BodyUS     int64 `json:"body_us"`
	TotalUS    int64 `json:"total_us"`
}

// Decoded is a body re-rendered from its Content-Type. Value is JSON-safe.
type Decoded struct {
	Format string `json:"format"` // json | form | multipart | text
	Value  any    `json:"value,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Request is the envelope each protocol fills in as far as it can.
type Request struct {
	Protocol    string              `json:"protocol"`
	Method      string              `json:"method,omitempty"`
	Path        string              `json:"path,omitempty"`
	Subpath     string              `json:"subpath,omitempty"` // path minus the "/d/{id}" prefix
	Query       string              `json:"query,omitempty"`
	Host        string              `json:"host,omitempty"`
	HTTPVersion string              `json:"http_version,omitempty"`
	Headers     map[string][]string `json:"headers,omitempty"`
	ContentLen  int64               `json:"content_length,omitempty"`
	BodyBytes   int                 `json:"body_bytes"`
	Truncated   bool                `json:"truncated,omitempty"`
	BodyText    string              `json:"body_text,omitempty"`
	BodyBase64  string              `json:"body_base64,omitempty"`
	Decoded     *Decoded            `json:"decoded,omitempty"`
}

// Event is one frame, chunk, datagram or emitted SSE event inside a record.
type Event struct {
	At     time.Time `json:"at"`
	Dir    string    `json:"dir"`  // in | out
	Type   string    `json:"type"` // frame | chunk | datagram | message | event
	Bytes  int       `json:"bytes"`
	Text   string    `json:"text,omitempty"`
	Base64 string    `json:"base64,omitempty"`
}

// Record is one row in the debug page.
type Record struct {
	Seq           uint64            `json:"seq"`
	Kind          Kind              `json:"kind"`
	ReceivedAt    time.Time         `json:"received_at"`
	Remote        Remote            `json:"remote"`
	Req           *Request          `json:"req,omitempty"`
	Meta          map[string]string `json:"meta,omitempty"`
	Timing        Timing            `json:"timing"`
	Events        []Event           `json:"events"`
	DroppedEvents int               `json:"dropped_events,omitempty"`
	Closed        bool              `json:"closed,omitempty"`
	CloseNote     string            `json:"close_note,omitempty"`
}
```

**Step 4: Run to verify pass**

```bash
./scripts/dev.sh go test ./internal/model -count=1
```

Expected: `ok  	github.com/adam/dumpster/internal/model	0.00Xs`

**Step 5: Commit**

```bash
git add internal/model
git commit -m "feat(model): add shared record envelope for all protocols"
```

---

### Task 6 — `internal/id/id.go` (UUIDv7 + normalisation)

**Objective:** Mint time-ordered UUIDv7 ids and accept every plausible spelling of one.

**Files:**
- Create: `internal/id/id.go`
- Test: `internal/id/id_test.go`

**Step 1: Write the failing test**

```go
package id

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestNewIsOrderedV7(t *testing.T) {
	a, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a >= b {
		t.Fatalf("uuidv7 is not time-ordered: %q >= %q", a, b)
	}
	u, err := uuid.Parse(a)
	if err != nil {
		t.Fatalf("parse %q: %v", a, err)
	}
	if u.Version() != 7 {
		t.Fatalf("version = %d, want 7", u.Version())
	}
	if len(a) != 36 || strings.Count(a, "-") != 4 {
		t.Fatalf("id %q is not dashed 36-char", a)
	}
}

func TestCanonicalAcceptsEveryForm(t *testing.T) {
	want, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, in := range []string{want, Hex(want), "d" + Hex(want), strings.ToUpper(want)} {
		got, err := Canonical(in)
		if err != nil {
			t.Fatalf("Canonical(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("Canonical(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "nope", "d", strings.Repeat("z", 32)} {
		if _, err := Canonical(bad); err == nil {
			t.Fatalf("Canonical(%q) should have failed", bad)
		}
	}
}
```

**Step 2: Run it, expect failure**

```bash
./scripts/dev.sh go test ./internal/id -count=1
```

Expected: `undefined: New` / `[setup failed]`.

**Step 3: Write the implementation**

```go
// Package id mints and normalises dumpster identifiers.
package id

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// New returns a fresh UUIDv7 string. v7 is time-ordered, so dumpsters sort by
// rental time and any future SQL store keeps index locality.
func New() (string, error) {
	u, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("generate uuidv7: %w", err)
	}
	return u.String(), nil
}

// Canonical accepts a dashed UUID, a bare 32-char hex UUID, or the gRPC path
// form "d<hex>", and returns the canonical dashed lowercase string.
//
// The "d" prefix is only stripped when the remainder parses, so a legitimate
// id that happens to start with the hex digit d is not mangled.
func Canonical(s string) (string, error) {
	if u, err := uuid.Parse(s); err == nil {
		return u.String(), nil
	}
	if len(s) == 33 && (s[0] == 'd' || s[0] == 'D') {
		if u, err := uuid.Parse(s[1:]); err == nil {
			return u.String(), nil
		}
	}
	return "", fmt.Errorf("invalid dumpster id %q", s)
}

// Hex strips the dashes, for use in a gRPC method path.
func Hex(canonical string) string { return strings.ReplaceAll(canonical, "-", "") }
```

**Step 4: Run to verify pass**

```bash
./scripts/dev.sh go test ./internal/id -count=1
```

Expected: `ok  	github.com/adam/dumpster/internal/id	0.00Xs`

**Step 5: Commit**

```bash
git add internal/id
git commit -m "feat(id): mint uuidv7 dumpster ids and normalise every spelling"
```

---

### Task 7 — `internal/store`: create and get

**Objective:** A dumpster can be created, found, and is 12 h old at birth.

**Files:**
- Create: `internal/store/store.go`
- Test: `internal/store/store_test.go`

**Step 1: Write the failing test**

```go
package store

import (
	"testing"
	"time"
)

func TestCreateAndGet(t *testing.T) {
	s := New(Limits{MaxRecords: 3, MaxEvents: 2, MaxDumpsters: 2}, time.Hour)
	d, err := s.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(d.ID) != 36 {
		t.Fatalf("id = %q, want a dashed 36-char uuid", d.ID)
	}
	if got := d.ExpiresAt.Sub(d.CreatedAt); got != time.Hour {
		t.Fatalf("ttl = %v, want 1h", got)
	}
	got, ok := s.Get(d.ID)
	if !ok {
		t.Fatal("Get: fresh dumpster not found")
	}
	if got.ID != d.ID {
		t.Fatalf("Get returned %q, want %q", got.ID, d.ID)
	}
}

func TestMaxDumpsters(t *testing.T) {
	s := New(Limits{MaxDumpsters: 2}, time.Hour)
	if _, err := s.Create(); err != nil {
		t.Fatalf("Create 1: %v", err)
	}
	if _, err := s.Create(); err != nil {
		t.Fatalf("Create 2: %v", err)
	}
	if _, err := s.Create(); err != ErrFull {
		t.Fatalf("Create 3 err = %v, want ErrFull", err)
	}
}
```

**Step 2: Run it, expect failure**

```bash
./scripts/dev.sh go test ./internal/store -count=1
```

Expected: `undefined: New`, `undefined: Limits`.

**Step 3: Write the implementation**

```go
// Package store holds dumpsters and their records in memory.
package store

import (
	"errors"
	"sync"
	"time"

	"github.com/adam/dumpster/internal/id"
	"github.com/adam/dumpster/internal/model"
)

var (
	// ErrNotFound means the dumpster never existed, or has expired.
	ErrNotFound = errors.New("dumpster not found")
	// ErrFull means the global dumpster cap is reached.
	ErrFull = errors.New("too many dumpsters")
)

// Limits are the caps that keep an unauthenticated service alive.
type Limits struct {
	MaxRecords   int
	MaxEvents    int
	MaxDumpsters int // 0 = unlimited
}

// Store is a map of dumpsters, each behind its own mutex, so ingestion into one
// dumpster never blocks ingestion into another.
type Store struct {
	limits Limits
	ttl    time.Duration
	now    func() time.Time // overridable in tests

	mu    sync.RWMutex
	dumps map[string]*entry
}

type entry struct {
	mu      sync.Mutex
	dump    model.Dumpster
	records []*model.Record // oldest first; List reverses
	nextSeq uint64
}

// New returns an empty store. Non-positive record/event caps fall back to 500.
func New(limits Limits, ttl time.Duration) *Store {
	if limits.MaxRecords <= 0 {
		limits.MaxRecords = 500
	}
	if limits.MaxEvents <= 0 {
		limits.MaxEvents = 500
	}
	return &Store{
		limits: limits,
		ttl:    ttl,
		now:    time.Now,
		dumps:  make(map[string]*entry),
	}
}

// Create mints a new dumpster and returns it.
func (s *Store) Create() (model.Dumpster, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.limits.MaxDumpsters > 0 && len(s.dumps) >= s.limits.MaxDumpsters {
		return model.Dumpster{}, ErrFull
	}
	now := s.now()
	for attempt := 0; attempt < 5; attempt++ {
		sid, err := id.New()
		if err != nil {
			return model.Dumpster{}, err
		}
		if _, clash := s.dumps[sid]; clash {
			continue
		}
		d := model.Dumpster{ID: sid, CreatedAt: now, ExpiresAt: now.Add(s.ttl)}
		s.dumps[sid] = &entry{dump: d}
		return d, nil
	}
	return model.Dumpster{}, errors.New("could not mint a unique dumpster id")
}

// Get returns the dumpster if it exists and has not expired.
func (s *Store) Get(sid string) (model.Dumpster, bool) {
	e, ok := s.entry(sid)
	if !ok {
		return model.Dumpster{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.dump.ExpiresAt.After(s.now()) {
		return model.Dumpster{}, false
	}
	return e.dump, true
}

// Len is the number of dumpsters held, including those the reaper has not
// reached yet.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.dumps)
}

func (s *Store) entry(sid string) (*entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.dumps[sid]
	return e, ok
}
```

**Step 4: Run to verify pass**

```bash
./scripts/dev.sh go test ./internal/store -count=1
```

Expected: `ok  	github.com/adam/dumpster/internal/store	0.00Xs`

**Step 5: Commit**

```bash
git add internal/store
git commit -m "feat(store): create and look up dumpsters with a ttl"
```

---

### Task 8 — `internal/store`: add records, trim, list newest-first

**Objective:** Records get a monotonic `Seq` and the page can read them newest-first.

**Files:**
- Modify: `internal/store/store.go`
- Modify: `internal/store/store_test.go`

**Step 1: Append the failing test**

```go
func TestAddAssignsSeqTrimsAndListsNewestFirst(t *testing.T) {
	s := New(Limits{MaxRecords: 3}, time.Hour)
	d, err := s.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := s.Add(d.ID, &model.Record{Kind: model.KindHTTP}); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	recs, ok := s.List(d.ID)
	if !ok {
		t.Fatal("List: not found")
	}
	if len(recs) != 3 {
		t.Fatalf("len = %d, want 3 (MaxRecords)", len(recs))
	}
	if recs[0].Seq != 5 || recs[2].Seq != 3 {
		t.Fatalf("seqs = %d..%d, want 5..3", recs[0].Seq, recs[2].Seq)
	}
	if recs[0].Events == nil {
		t.Fatal("Events is nil; JSON would serialise null instead of []")
	}
	if _, err := s.Add("does-not-exist", &model.Record{}); err != ErrNotFound {
		t.Fatalf("Add on unknown dumpster = %v, want ErrNotFound", err)
	}
}
```

Add `"github.com/adam/dumpster/internal/model"` to the test imports.

**Step 2: Run it, expect failure**

```bash
./scripts/dev.sh go test ./internal/store -count=1
```

Expected: `s.Add undefined` / `s.List undefined`.

**Step 3: Write the implementation** (append to `internal/store/store.go`)

```go
// Add assigns Seq, stores a copy of rec, trims to MaxRecords, and returns the
// stored record. Events is never nil, so callers can marshal it directly.
func (s *Store) Add(sid string, rec *model.Record) (model.Record, error) {
	e, ok := s.entry(sid)
	if !ok {
		return model.Record{}, ErrNotFound
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.dump.ExpiresAt.After(s.now()) {
		return model.Record{}, ErrNotFound
	}
	e.nextSeq++
	stored := *rec
	stored.Seq = e.nextSeq
	if stored.ReceivedAt.IsZero() {
		stored.ReceivedAt = s.now()
	}
	if stored.Events == nil {
		stored.Events = []model.Event{}
	}
	e.records = append(e.records, &stored)
	if over := len(e.records) - s.limits.MaxRecords; over > 0 {
		e.records = append([]*model.Record(nil), e.records[over:]...)
	}
	return stored, nil
}

// List returns copies of every record, newest first.
func (s *Store) List(sid string) ([]model.Record, bool) {
	e, ok := s.entry(sid)
	if !ok {
		return nil, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]model.Record, 0, len(e.records))
	for i := len(e.records) - 1; i >= 0; i-- {
		out = append(out, *e.records[i])
	}
	return out, true
}
```

**Step 4: Run to verify pass**

```bash
./scripts/dev.sh go test ./internal/store -count=1
```

Expected: `ok  	github.com/adam/dumpster/internal/store	0.00Xs` (3 tests).

**Step 5: Commit**

```bash
git add internal/store
git commit -m "feat(store): append records with sequence numbers and a ring cap"
```

---

### Task 9 — `internal/store`: events and closing a record

**Objective:** Long-lived protocols can append frames to an existing row and mark it closed.

**Files:**
- Modify: `internal/store/store.go`
- Modify: `internal/store/store_test.go`

**Step 1: Append the failing test**

```go
func TestAppendEventCapsAndCountsDrops(t *testing.T) {
	s := New(Limits{MaxRecords: 5, MaxEvents: 2}, time.Hour)
	d, _ := s.Create()
	rec, err := s.Add(d.ID, &model.Record{Kind: model.KindTCP})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	for i := 0; i < 4; i++ {
		if _, err := s.AppendEvent(d.ID, rec.Seq, model.Event{Type: "chunk"}); err != nil {
			t.Fatalf("AppendEvent %d: %v", i, err)
		}
	}
	recs, _ := s.List(d.ID)
	if len(recs[0].Events) != 2 {
		t.Fatalf("events = %d, want 2 (MaxEvents)", len(recs[0].Events))
	}
	if recs[0].DroppedEvents != 2 {
		t.Fatalf("dropped = %d, want 2", recs[0].DroppedEvents)
	}
}

func TestCloseMarksRecord(t *testing.T) {
	s := New(Limits{}, time.Hour)
	d, _ := s.Create()
	rec, _ := s.Add(d.ID, &model.Record{Kind: model.KindWS})
	closed, err := s.Close(d.ID, rec.Seq, "closed by peer")
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !closed.Closed || closed.CloseNote != "closed by peer" {
		t.Fatalf("closed = %+v", closed)
	}
}
```

**Step 2: Run it, expect failure**

```bash
./scripts/dev.sh go test ./internal/store -count=1
```

Expected: `s.AppendEvent undefined`, `s.Close undefined`.

**Step 3: Write the implementation** (append to `internal/store/store.go`)

```go
// AppendEvent appends one event to the record with this Seq and returns the
// updated record. Once MaxEvents is reached the event is dropped and counted,
// so the UI can show the loss instead of pretending it never happened.
func (s *Store) AppendEvent(sid string, seq uint64, ev model.Event) (model.Record, error) {
	e, ok := s.entry(sid)
	if !ok {
		return model.Record{}, ErrNotFound
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.find(seq)
	if r == nil {
		return model.Record{}, ErrNotFound
	}
	if len(r.Events) >= s.limits.MaxEvents {
		r.DroppedEvents++
		return *r, nil
	}
	r.Events = append(r.Events, ev)
	return *r, nil
}

// Close marks the record finished and records why.
func (s *Store) Close(sid string, seq uint64, note string) (model.Record, error) {
	e, ok := s.entry(sid)
	if !ok {
		return model.Record{}, ErrNotFound
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.find(seq)
	if r == nil {
		return model.Record{}, ErrNotFound
	}
	r.Closed = true
	r.CloseNote = note
	return *r, nil
}

// find scans newest-first, which is where live protocols look.
func (e *entry) find(seq uint64) *model.Record {
	for i := len(e.records) - 1; i >= 0; i-- {
		if e.records[i].Seq == seq {
			return e.records[i]
		}
	}
	return nil
}
```

**Step 4: Run to verify pass**

```bash
./scripts/dev.sh go test ./internal/store -count=1
```

Expected: `ok` (5 tests).

**Step 5: Commit**

```bash
git add internal/store
git commit -m "feat(store): append events to a record and close it"
```

---

### Task 10 — `internal/store`: expiry and the reaper query

**Objective:** An expired dumpster is invisible to clients and removable by the sweeper.

**Files:**
- Modify: `internal/store/store.go` (already contains the checks — this task adds the sweeper query)
- Modify: `internal/store/store_test.go`

**Step 1: Append the failing test**

```go
func TestExpiryHidesAndDeletes(t *testing.T) {
	s := New(Limits{MaxRecords: 3}, 50*time.Millisecond)
	d, err := s.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, ok := s.Get(d.ID); !ok {
		t.Fatal("fresh dumpster should be visible")
	}
	time.Sleep(70 * time.Millisecond)
	if _, ok := s.Get(d.ID); ok {
		t.Fatal("expired dumpster is still visible")
	}
	if _, err := s.Add(d.ID, &model.Record{}); err != ErrNotFound {
		t.Fatalf("Add on expired = %v, want ErrNotFound", err)
	}
	gone := s.DeleteExpired(time.Now())
	if len(gone) != 1 || gone[0] != d.ID {
		t.Fatalf("DeleteExpired = %v, want [%s]", gone, d.ID)
	}
	if s.Len() != 0 {
		t.Fatalf("Len = %d, want 0", s.Len())
	}
	if len(s.DeleteExpired(time.Now())) != 0 {
		t.Fatal("second DeleteExpired should find nothing")
	}
}
```

**Step 2: Run it, expect failure**

```bash
./scripts/dev.sh go test ./internal/store -count=1
```

Expected: `s.DeleteExpired undefined`.

**Step 3: Add the import, then the implementation**

`DeleteExpired` is the only user of `sort`, so add it to the import block now —
adding it in Task 7 is what breaks that task's build with `"sort" imported and not used`.

```go
import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/adam/dumpster/internal/id"
	"github.com/adam/dumpster/internal/model"
)
```

Then append to `internal/store/store.go`:

```go
// DeleteExpired removes every dumpster whose TTL has passed and returns the
// removed ids, sorted, so the caller can close each one's subscribers.
func (s *Store) DeleteExpired(now time.Time) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var gone []string
	for sid, e := range s.dumps {
		e.mu.Lock()
		expired := !e.dump.ExpiresAt.After(now)
		e.mu.Unlock()
		if expired {
			delete(s.dumps, sid)
			gone = append(gone, sid)
		}
	}
	sort.Strings(gone)
	return gone
}
```

**Step 4: Run to verify pass**

```bash
./scripts/dev.sh go test ./internal/store -count=1
```

Expected: `ok` (6 tests).

**Step 5: Commit**

```bash
git add internal/store
git commit -m "feat(store): expire dumpsters and sweep them"
```

---

### Task 11 — `internal/hub/hub.go` (subscriber fan-out)

**Objective:** N browser sockets per dumpster, publishing never blocks an ingester.

**Files:**
- Create: `internal/hub/hub.go`
- Test: `internal/hub/hub_test.go`

**Step 1: Write the failing test**

```go
package hub

import (
	"testing"
	"time"
)

func TestPublishFansOutAndCloseEndsIt(t *testing.T) {
	h := New(4)
	ch, cancel := h.Subscribe("abc")
	defer cancel()

	h.Publish("abc", map[string]string{"type": "new"})
	select {
	case b := <-ch:
		if string(b) != `{"type":"new"}` {
			t.Fatalf("message = %s", b)
		}
	case <-time.After(time.Second):
		t.Fatal("no message received")
	}

	h.Close("abc")
	select {
	case _, open := <-ch:
		if open {
			t.Fatal("channel still open after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not close the subscriber")
	}
}

func TestTwoSubscribersBothReceive(t *testing.T) {
	h := New(4)
	a, ca := h.Subscribe("abc")
	defer ca()
	b, cb := h.Subscribe("abc")
	defer cb()

	h.Publish("abc", 1)
	for name, ch := range map[string]<-chan []byte{"a": a, "b": b} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("subscriber %s got nothing", name)
		}
	}
}

func TestPublishNeverBlocksOnFullSubscriber(t *testing.T) {
	h := New(1)
	_, cancel := h.Subscribe("abc")
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			h.Publish("abc", i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on a full subscriber")
	}
}

func TestCancelIsIdempotent(t *testing.T) {
	h := New(1)
	_, cancel := h.Subscribe("abc")
	cancel()
	cancel() // must not panic on double close
}
```

**Step 2: Run it, expect failure**

```bash
./scripts/dev.sh go test ./internal/hub -count=1
```

Expected: `undefined: New`.

**Step 3: Write the implementation**

```go
// Package hub fans records out to every debug page watching one dumpster.
package hub

import (
	"encoding/json"
	"sync"
)

// Hub keeps one subscriber set per dumpster. Publishing never blocks: a browser
// that falls behind drops messages rather than stalling an ingester.
type Hub struct {
	buf int

	mu   sync.Mutex
	subs map[string]map[chan []byte]struct{}
}

// New returns a hub whose subscriber channels buffer `buffer` messages.
func New(buffer int) *Hub {
	if buffer <= 0 {
		buffer = 64
	}
	return &Hub{buf: buffer, subs: make(map[string]map[chan []byte]struct{})}
}

// Subscribe returns a channel of pre-marshalled JSON plus a cancel func that is
// safe to call more than once.
func (h *Hub) Subscribe(dumpsterID string) (<-chan []byte, func()) {
	ch := make(chan []byte, h.buf)
	h.mu.Lock()
	if h.subs[dumpsterID] == nil {
		h.subs[dumpsterID] = make(map[chan []byte]struct{})
	}
	h.subs[dumpsterID][ch] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			set, ok := h.subs[dumpsterID]
			if !ok {
				return
			}
			if _, live := set[ch]; live {
				delete(set, ch)
				close(ch)
			}
			if len(set) == 0 {
				delete(h.subs, dumpsterID)
			}
		})
	}
	return ch, cancel
}

// Publish marshals msg once and offers it to every subscriber of dumpsterID.
func (h *Hub) Publish(dumpsterID string, msg any) {
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[dumpsterID] {
		select {
		case ch <- b:
		default: // subscriber is behind; drop rather than block the ingest
		}
	}
}

// Close ends every subscription for dumpsterID (the dumpster was reaped).
// Channels are closed exactly once, under the same lock Publish holds, so a
// send can never land on a closed channel.
func (h *Hub) Close(dumpsterID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set, ok := h.subs[dumpsterID]
	if !ok {
		return
	}
	for ch := range set {
		close(ch)
	}
	delete(h.subs, dumpsterID)
}
```

**Step 4: Run to verify pass**

```bash
./scripts/dev.sh go test ./internal/hub -count=1
```

Expected: `ok` (4 tests).

**Step 5: Commit**

```bash
git add internal/hub
git commit -m "feat(hub): non-blocking per-dumpster subscriber fan-out"
```

---

### Task 12 — `internal/decode`: JSON, form, text

**Objective:** A body is re-rendered according to its `Content-Type`, or reported as undecodable.

**Files:**
- Create: `internal/decode/decode.go`
- Test: `internal/decode/decode_test.go`

**Step 1: Write the failing test**

```go
package decode

import (
	"net/url"
	"testing"
)

func TestJSON(t *testing.T) {
	got := Body("application/json; charset=utf-8", []byte(`{"a":[1,2]}`))
	if got == nil || got.Format != "json" || got.Error != "" {
		t.Fatalf("got %+v", got)
	}
	m, ok := got.Value.(map[string]any)
	if !ok {
		t.Fatalf("value type = %T", got.Value)
	}
	if _, ok := m["a"]; !ok {
		t.Fatalf("missing key a in %+v", m)
	}
}

func TestJSONSubtypeAndError(t *testing.T) {
	if got := Body("application/vnd.api+json", []byte(`{"x":1}`)); got == nil || got.Format != "json" {
		t.Fatalf("+json subtype: %+v", got)
	}
	got := Body("application/json", []byte(`{"a":`))
	if got == nil || got.Error == "" {
		t.Fatalf("broken json should report an error, got %+v", got)
	}
}

func TestForm(t *testing.T) {
	got := Body("application/x-www-form-urlencoded", []byte("a=1&b=2&b=3"))
	if got == nil || got.Format != "form" {
		t.Fatalf("got %+v", got)
	}
	vals, ok := got.Value.(url.Values)
	if !ok {
		t.Fatalf("value type = %T", got.Value)
	}
	if len(vals["b"]) != 2 {
		t.Fatalf("b = %v, want two values", vals["b"])
	}
}

func TestTextAndBinary(t *testing.T) {
	if got := Body("text/plain", []byte("hello")); got == nil || got.Format != "text" {
		t.Fatalf("text: %+v", got)
	}
	if got := Body("application/octet-stream", []byte{0xff, 0xfe, 0x00}); got != nil {
		t.Fatalf("binary should not decode, got %+v", got)
	}
	if got := Body("application/json", nil); got == nil || got.Error == "" {
		t.Fatalf("empty json body should be an error, got %+v", got)
	}
}
```

**Step 2: Run it, expect failure**

```bash
./scripts/dev.sh go test ./internal/decode -count=1
```

Expected: `undefined: Body`.

**Step 3: Write the implementation**

```go
// Package decode re-renders a request body according to its Content-Type.
package decode

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/adam/dumpster/internal/model"
)

const (
	maxParts    = 50      // multipart parts examined per request
	maxPartByte = 1 << 20 // bytes read per part
	previewByte = 512     // bytes of a file part shown as text
)

// Body classifies raw bytes by media type. It returns nil when nothing sensible
// can be decoded, which the UI shows as "not decodable" rather than an error.
func Body(contentType string, body []byte) *model.Decoded {
	mt, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		mt = strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	}
	switch {
	case mt == "application/json" || strings.HasSuffix(mt, "+json"):
		var v any
		if err := json.Unmarshal(body, &v); err != nil {
			return &model.Decoded{Format: "json", Error: err.Error()}
		}
		return &model.Decoded{Format: "json", Value: v}
	case mt == "application/x-www-form-urlencoded":
		vals, err := url.ParseQuery(string(body))
		if err != nil {
			return &model.Decoded{Format: "form", Error: err.Error()}
		}
		return &model.Decoded{Format: "form", Value: vals}
	case mt == "multipart/form-data":
		return multipartBody(body, params["boundary"])
	case len(body) > 0 && utf8.Valid(body):
		return &model.Decoded{Format: "text", Value: string(body)}
	}
	return nil
}

// multipartBody splits fields from files. Files are described, not retained:
// name, media type, size and sha256 are enough to identify an upload.
func multipartBody(body []byte, boundary string) *model.Decoded {
	if boundary == "" {
		return &model.Decoded{Format: "multipart", Error: "missing boundary parameter"}
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	fields := map[string][]string{}
	files := []map[string]any{}
	for i := 0; i < maxParts; i++ {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return &model.Decoded{Format: "multipart", Error: err.Error()}
		}
		data, readErr := io.ReadAll(io.LimitReader(p, maxPartByte))
		p.Close()
		if readErr != nil {
			return &model.Decoded{Format: "multipart", Error: readErr.Error()}
		}
		if name := p.FileName(); name != "" {
			sum := sha256.Sum256(data)
			files = append(files, map[string]any{
				"field":        p.FormName(),
				"filename":     name,
				"content_type": p.Header.Get("Content-Type"),
				"size":         len(data),
				"sha256":       hex.EncodeToString(sum[:]),
				"preview":      preview(data),
			})
			continue
		}
		fields[p.FormName()] = append(fields[p.FormName()], string(data))
	}
	return &model.Decoded{Format: "multipart", Value: map[string]any{
		"fields": fields,
		"files":  files,
	}}
}

// preview renders the first previewByte bytes as text when they are UTF-8.
func preview(b []byte) string {
	if len(b) > previewByte {
		b = b[:previewByte]
	}
	if !utf8.Valid(b) {
		return ""
	}
	return string(b)
}
```

**Step 4: Run to verify pass**

```bash
./scripts/dev.sh go test ./internal/decode -count=1
```

Expected: `ok` (4 tests).

**Step 5: Commit**

```bash
git add internal/decode
git commit -m "feat(decode): decode json, form and text bodies"
```

---

### Task 13 — `internal/decode`: multipart

**Objective:** `multipart/form-data` fields are readable and files are fingerprinted.

**Files:**
- Modify: `internal/decode/decode_test.go` (the implementation already ships; this task proves it)

**Step 1: Append the failing test**

```go
func TestMultipartFieldsAndFiles(t *testing.T) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("colour", "purple"); err != nil {
		t.Fatalf("WriteField: %v", err)
	}
	fw, err := w.CreateFormFile("shot", "x.png")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write([]byte("fake-png-bytes")); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	got := Body(w.FormDataContentType(), buf.Bytes())
	if got == nil || got.Format != "multipart" {
		t.Fatalf("got %+v", got)
	}
	val, ok := got.Value.(map[string]any)
	if !ok {
		t.Fatalf("value type = %T", got.Value)
	}
	fields := val["fields"].(map[string][]string)
	if got := fields["colour"]; len(got) != 1 || got[0] != "purple" {
		t.Fatalf("fields = %v", fields)
	}
	files := val["files"].([]map[string]any)
	if len(files) != 1 {
		t.Fatalf("files = %v, want 1", files)
	}
	if files[0]["filename"] != "x.png" {
		t.Fatalf("filename = %v", files[0]["filename"])
	}
	if files[0]["size"] != 14 {
		t.Fatalf("size = %v, want 14", files[0]["size"])
	}
}

func TestMultipartMissingBoundary(t *testing.T) {
	got := Body("multipart/form-data", []byte("garbage"))
	if got == nil || got.Error == "" {
		t.Fatalf("want a boundary error, got %+v", got)
	}
}
```

Add `"bytes"` and `"mime/multipart"` to the test imports.

**Step 2: Run it, expect failure or pass**

```bash
./scripts/dev.sh go test ./internal/decode -count=1
```

Expected: **PASS on the first run** (Task 12 shipped `multipartBody` in full). That is fine — this task converts untested code into tested code. If it fails, the multipart branch in `multipartBody` is wrong; fix it there, not in the test.

**Step 3: Commit**

```bash
git add internal/decode
git commit -m "test(decode): cover multipart fields, files and missing boundary"
```

**Phase gate**

```bash
./scripts/dev.sh go vet ./...
./scripts/dev.sh go test ./internal/... -count=1
```

Expected: `vet` prints nothing; five `ok` lines — one each for `decode`, `hub`, `id`, `model` and `store`.

---
## 6. Phase 2 — App skeleton and HTTP ingest

**Phase gate:** `./scripts/dev.sh go test ./internal/app -count=1` prints `ok`, and `./scripts/dev.sh go vet ./...` prints nothing.

At the end of this phase the server binds **only `:8080`** (plus the reaper). The gRPC, TCP and UDP listeners are bolted on in Phases 6–8 with small, explicit patches to `internal/app/app.go`. That is deliberate: each listener lands with its own test instead of shipping four untested accept loops at once.

---

### Task 14 — `internal/app/config.go`

**Objective:** Every tunable in one struct with defaults and `DUMPSTER_*` overrides.

**Files:**
- Create: `internal/app/config.go`
- Test: `internal/app/config_test.go`

**Step 1: Write the failing test**

```go
package app

import (
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	c := DefaultConfig()
	if c.HTTPAddr != ":8080" || c.GRPCAddr != ":8081" || c.TCPAddr != ":8082" || c.UDPAddr != ":8083" {
		t.Fatalf("addrs = %+v", c)
	}
	if c.TTL != 12*time.Hour {
		t.Fatalf("TTL = %v, want 12h", c.TTL)
	}
	if c.MaxBody != 1<<20 {
		t.Fatalf("MaxBody = %d, want 1 MiB", c.MaxBody)
	}
	if c.RentPerHour != 10 || c.MaxRecords != 500 {
		t.Fatalf("limits = %+v", c)
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("DUMPSTER_TTL", "90m")
	t.Setenv("DUMPSTER_TRUST_PROXY", "1")
	t.Setenv("DUMPSTER_MAX_BODY", "2048")
	t.Setenv("DUMPSTER_BASE_URL", "https://dumpster.example")

	c := DefaultConfig().FromEnv()
	if c.TTL != 90*time.Minute {
		t.Fatalf("TTL = %v, want 90m", c.TTL)
	}
	if !c.TrustProxy {
		t.Fatal("TrustProxy should be on")
	}
	if c.MaxBody != 2048 {
		t.Fatalf("MaxBody = %d, want 2048", c.MaxBody)
	}
	if c.BaseURL != "https://dumpster.example" {
		t.Fatalf("BaseURL = %q", c.BaseURL)
	}
}

func TestFromEnvIgnoresGarbage(t *testing.T) {
	t.Setenv("DUMPSTER_TTL", "not-a-duration")
	t.Setenv("DUMPSTER_MAX_BODY", "bananas")
	c := DefaultConfig().FromEnv()
	if c.TTL != 12*time.Hour || c.MaxBody != 1<<20 {
		t.Fatalf("garbage env should be ignored, got %+v", c)
	}
}
```

**Step 2: Run it, expect failure**

```bash
./scripts/dev.sh go test ./internal/app -count=1
```

Expected: `undefined: DefaultConfig`.

**Step 3: Write the implementation**

```go
package app

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is everything tunable. Every field has a sane default; environment
// variables and flags override it.
type Config struct {
	HTTPAddr string
	GRPCAddr string
	TCPAddr  string
	UDPAddr  string

	BaseURL string // external origin for links, e.g. https://dumpster.example

	PublicGRPC string // what a human should type; defaults to BaseURL host + port
	PublicTCP  string
	PublicUDP  string

	TTL       time.Duration
	ReapEvery time.Duration

	MaxBody      int
	MaxRecords   int
	MaxEvents    int
	MaxDumpsters int
	RentPerHour  int
	ConnIdle     time.Duration
	TrustProxy   bool
}

// DefaultConfig is the configuration the plan's expected outputs assume.
func DefaultConfig() Config {
	return Config{
		HTTPAddr: ":8080",
		GRPCAddr: ":8081",
		TCPAddr:  ":8082",
		UDPAddr:  ":8083",

		TTL:       12 * time.Hour,
		ReapEvery: time.Minute,

		MaxBody:      1 << 20,
		MaxRecords:   500,
		MaxEvents:    500,
		MaxDumpsters: 10000,
		RentPerHour:  10,
		ConnIdle:     90 * time.Second,
	}
}

// FromEnv overlays DUMPSTER_* variables. Invalid values are ignored, so a typo
// can never stop the service from starting.
func (c Config) FromEnv() Config {
	if v := os.Getenv("DUMPSTER_HTTP_ADDR"); v != "" {
		c.HTTPAddr = v
	}
	if v := os.Getenv("DUMPSTER_GRPC_ADDR"); v != "" {
		c.GRPCAddr = v
	}
	if v := os.Getenv("DUMPSTER_TCP_ADDR"); v != "" {
		c.TCPAddr = v
	}
	if v := os.Getenv("DUMPSTER_UDP_ADDR"); v != "" {
		c.UDPAddr = v
	}
	if v := os.Getenv("DUMPSTER_BASE_URL"); v != "" {
		c.BaseURL = v
	}
	if v := os.Getenv("DUMPSTER_PUBLIC_GRPC"); v != "" {
		c.PublicGRPC = v
	}
	if v := os.Getenv("DUMPSTER_PUBLIC_TCP"); v != "" {
		c.PublicTCP = v
	}
	if v := os.Getenv("DUMPSTER_PUBLIC_UDP"); v != "" {
		c.PublicUDP = v
	}

	c.TTL = durationEnv("DUMPSTER_TTL", c.TTL)
	c.ReapEvery = durationEnv("DUMPSTER_REAP_EVERY", c.ReapEvery)
	c.ConnIdle = durationEnv("DUMPSTER_CONN_IDLE", c.ConnIdle)

	if v, ok := intEnv("DUMPSTER_MAX_BODY"); ok {
		c.MaxBody = v
	}
	if v, ok := intEnv("DUMPSTER_MAX_RECORDS"); ok {
		c.MaxRecords = v
	}
	if v, ok := intEnv("DUMPSTER_MAX_EVENTS"); ok {
		c.MaxEvents = v
	}
	if v, ok := intEnv("DUMPSTER_MAX_DUMPSTERS"); ok {
		c.MaxDumpsters = v
	}
	if v, ok := intEnv("DUMPSTER_RENT_PER_HOUR"); ok {
		c.RentPerHour = v
	}
	if v := os.Getenv("DUMPSTER_TRUST_PROXY"); v != "" {
		c.TrustProxy = v == "1" || strings.EqualFold(v, "true")
	}
	return c
}

func durationEnv(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func intEnv(key string) (int, bool) {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n, true
		}
	}
	return 0, false
}
```

**Step 4: Run to verify pass**

```bash
./scripts/dev.sh go test ./internal/app -count=1
```

Expected: `ok  	github.com/adam/dumpster/internal/app	0.00Xs`

**Step 5: Commit**

```bash
git add internal/app
git commit -m "feat(app): add configuration with env overrides"
```

---

### Task 15 — `internal/ratelimit` (per-IP token bucket)

**Objective:** Ten rents an hour per IP, without a database.

**Files:**
- Create: `internal/ratelimit/ratelimit.go`
- Test: `internal/ratelimit/ratelimit_test.go`

**Step 1: Write the failing test**

```go
package ratelimit

import (
	"testing"
	"time"
)

func TestAllowConsumesBurst(t *testing.T) {
	l := New(3, time.Hour)
	for i := 0; i < 3; i++ {
		if !l.Allow("1.2.3.4") {
			t.Fatalf("call %d should be allowed", i)
		}
	}
	if l.Allow("1.2.3.4") {
		t.Fatal("fourth call should be denied")
	}
	if !l.Allow("5.6.7.8") {
		t.Fatal("a different key must be independent")
	}
}

func TestRefillsOverTime(t *testing.T) {
	l := New(2, time.Hour)
	now := time.Now()
	l.now = func() time.Time { return now }

	if !l.Allow("k") || !l.Allow("k") {
		t.Fatal("burst of 2 should be allowed")
	}
	if l.Allow("k") {
		t.Fatal("third call should be denied")
	}
	now = now.Add(30 * time.Minute) // half a window => one token back
	if !l.Allow("k") {
		t.Fatal("one token should have refilled")
	}
	if l.Allow("k") {
		t.Fatal("only one token should have refilled")
	}
}

func TestPruneDropsIdleBuckets(t *testing.T) {
	l := New(2, time.Minute)
	now := time.Now()
	l.now = func() time.Time { return now }
	l.Allow("k")
	now = now.Add(2 * time.Minute)
	l.Prune()

	l.mu.Lock()
	n := len(l.state)
	l.mu.Unlock()
	if n != 0 {
		t.Fatalf("state = %d buckets, want 0", n)
	}
}
```

**Step 2: Run it, expect failure**

```bash
./scripts/dev.sh go test ./internal/ratelimit -count=1
```

Expected: `undefined: New`.

**Step 3: Write the implementation**

```go
// Package ratelimit is a per-key token bucket, refilled continuously.
package ratelimit

import (
	"sync"
	"time"
)

// Limiter allows `burst` actions per `window` for each key. Keys are IPs.
type Limiter struct {
	burst  int
	window time.Duration
	now    func() time.Time // overridable in tests

	mu    sync.Mutex
	state map[string]*bucket
}

type bucket struct {
	remaining float64
	updated   time.Time
}

// New returns a limiter. Non-positive arguments fall back to 1 per hour.
func New(burst int, window time.Duration) *Limiter {
	if burst <= 0 {
		burst = 1
	}
	if window <= 0 {
		window = time.Hour
	}
	return &Limiter{burst: burst, window: window, now: time.Now, state: make(map[string]*bucket)}
}

// Allow consumes one token for key and reports whether it was available.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, seen := l.state[key]
	if !seen {
		l.state[key] = &bucket{remaining: float64(l.burst) - 1, updated: now}
		return true
	}
	refill := float64(now.Sub(b.updated)) / float64(l.window) * float64(l.burst)
	if refill > 0 {
		b.remaining = min(float64(l.burst), b.remaining+refill)
		b.updated = now
	}
	if b.remaining < 1 {
		return false
	}
	b.remaining--
	return true
}

// Prune drops buckets untouched for a whole window, so the map cannot grow
// without bound. Call it from the reaper.
func (l *Limiter) Prune() {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := l.now().Add(-l.window)
	for k, b := range l.state {
		if b.updated.Before(cutoff) {
			delete(l.state, k)
		}
	}
}
```

**Step 4: Run to verify pass**

```bash
./scripts/dev.sh go test ./internal/ratelimit -count=1
```

Expected: `ok` (3 tests).

**Step 5: Commit**

```bash
git add internal/ratelimit
git commit -m "feat(ratelimit): per-ip token bucket for renting"
```

---

### Task 16 — `internal/web`: embedded templates and assets

**Objective:** A template set and a static filesystem compiled into the binary.

**Files:**
- Create: `internal/web/web.go`
- Create: `internal/web/templates/index.html`
- Create: `internal/web/templates/error.html`
- Create: `internal/web/static/style.css`
- Test: `internal/web/web_test.go`

**Step 1: Write the failing test**

```go
package web

import (
	"bytes"
	"io/fs"
	"strings"
	"testing"
)

func TestIndexRendersWithOneCTA(t *testing.T) {
	var buf bytes.Buffer
	err := Templates.ExecuteTemplate(&buf, "index.html", map[string]any{
		"Active": 3,
		"TTL":    "12h",
	})
	if err != nil {
		t.Fatalf("execute index.html: %v", err)
	}
	if got := strings.Count(buf.String(), "Rent a dumpster"); got != 1 {
		t.Fatalf("CTA appears %d times, want exactly 1", got)
	}
}

func TestStaticAssetsExist(t *testing.T) {
	for _, name := range []string{"style.css"} {
		if _, err := fs.Stat(Static, name); err != nil {
			t.Fatalf("static/%s: %v", name, err)
		}
	}
}
```

**Step 2: Run it, expect failure**

```bash
./scripts/dev.sh go test ./internal/web -count=1
```

Expected: `no Go files in .../internal/web` (nothing to compile yet).

**Step 3: Write `internal/web/web.go`**

```go
// Package web holds the templates and static assets embedded in the binary.
package web

import (
	"embed"
	"html/template"
	"io/fs"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Templates is the parsed template set. A malformed template panics at startup,
// which is the only place a panic is allowed.
var Templates = template.Must(template.New("dumpster").ParseFS(templateFS, "templates/*.html"))

// Static is the root of /static/.
var Static = mustSub(staticFS, "static")

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}
```

**Step 4: Write `internal/web/templates/index.html`**

```html
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Dumpster</title>
  <link rel="stylesheet" href="/static/style.css">
</head>
<body class="home">
  <main class="hero">
    <h1>Dumpster</h1>
    <p class="lede">Throw requests at it and watch them land. HTTP, gRPC, WebSocket,
      SSE, TCP, UDP — one link, one live list, twelve hours.</p>
    <form method="post" action="/rent">
      <button class="cta" type="submit">Rent a dumpster</button>
    </form>
    <p class="fine">{{.Active}} in the yard &middot; every dumpster burns itself after {{.TTL}}
      &middot; anyone with a link can read it</p>
  </main>
</body>
</html>
```

**Step 5: Write `internal/web/templates/error.html`**

```html
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{.Title}} &middot; Dumpster</title>
  <link rel="stylesheet" href="/static/style.css">
</head>
<body class="home">
  <main class="hero">
    <h1>{{.Title}}</h1>
    <p class="lede">{{.Message}}</p>
    <p><a class="cta ghost" href="/">Rent a dumpster</a></p>
  </main>
</body>
</html>
```

**Step 6: Write `internal/web/static/style.css`**

```css
:root{
  --bg:#0c0b10; --panel:#15131c; --ink:#e8e4ef; --muted:#9a93ab; --dim:#6d6680;
  --line:#2a2536; --accent:#b48ce0; --sage:#8fbf9f; --warn:#e2a04a; --err:#d9705f;
  --mono:ui-monospace,"SF Mono",Menlo,Consolas,monospace;
  --sans:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;
}
*{box-sizing:border-box}
html,body{margin:0;min-height:100%}
body{
  background:radial-gradient(1200px 620px at 50% -12%,#1b1628 0%,var(--bg) 62%);
  color:var(--ink);font:16px/1.55 var(--sans);
}
a{color:var(--accent)}
.mono{font-family:var(--mono);font-size:.86em;letter-spacing:-.01em}
main{max-width:1180px;margin:0 auto;padding:44px 20px 80px}
.hero{max-width:660px;padding-top:11vh}
h1{font-size:clamp(2.1rem,5.5vw,3.2rem);line-height:1.05;margin:0 0 .45em;letter-spacing:-.02em}
.lede{color:var(--muted);font-size:1.05rem;margin:0 0 2.2em;max-width:52ch}
.fine{color:var(--dim);font-size:.8rem;margin-top:3em;line-height:1.7}
.cta{
  display:inline-block;background:var(--accent);color:#150f1f;border:0;border-radius:10px;
  padding:16px 30px;font:600 1.05rem/1 var(--sans);cursor:pointer;text-decoration:none;
  transition:filter .15s ease,transform .15s ease;
}
.cta:hover{filter:brightness(1.12);transform:translateY(-1px)}
.cta.ghost{background:transparent;color:var(--accent);border:1px solid var(--line)}
/* ---------- debug page ---------- */
.bar{
  display:flex;gap:20px;align-items:flex-start;justify-content:space-between;
  flex-wrap:wrap;padding:20px 24px;border-bottom:1px solid var(--line);
  background:rgba(12,11,16,.72);backdrop-filter:blur(6px);position:sticky;top:0;z-index:5;
}
.bar h1{font-size:1rem;margin:0 0 4px;display:flex;gap:10px;align-items:baseline}
.bar .sub{margin:0;color:var(--dim);font-size:.78rem}
.status{
  font-family:var(--mono);font-size:.72rem;text-transform:uppercase;letter-spacing:.12em;
  color:var(--sage);border:1px solid var(--line);border-radius:999px;padding:6px 12px;
}
.grid{display:grid;grid-template-columns:minmax(0,1.75fr) minmax(0,1fr);gap:28px;align-items:start}
@media (max-width:900px){.grid{grid-template-columns:1fr}}
h2{font-size:.78rem;text-transform:uppercase;letter-spacing:.14em;color:var(--muted);margin:0 0 14px}
.count{color:var(--accent);font-family:var(--mono)}
.empty{color:var(--dim);font-size:.9rem}
.records{list-style:none;margin:0;padding:0;display:flex;flex-direction:column;gap:6px}
.rec{background:var(--panel);border:1px solid var(--line);border-radius:10px;overflow:hidden}
.rec.is-closed{opacity:.75}
.rec.open{border-color:#3a3350}
.row{
  display:grid;grid-template-columns:64px 1fr auto;gap:12px;align-items:center;width:100%;
  background:none;border:0;color:inherit;font:inherit;text-align:left;padding:11px 14px;cursor:pointer;
}
.row:hover{background:#191623}
.badge{
  font-family:var(--mono);font-size:.66rem;letter-spacing:.08em;text-align:center;
  padding:4px 0;border-radius:5px;background:#211d2e;color:var(--muted);
}
.badge.k-http{background:#1f2436;color:#8fb0e8}
.badge.k-grpc{background:#251f33;color:#b48ce0}
.badge.k-ws{background:#1e2a26;color:var(--sage)}
.badge.k-sse{background:#2b2418;color:var(--warn)}
.badge.k-tcp{background:#2a1f22;color:#d99a8f}
.badge.k-udp{background:#1c2a2c;color:#82c4c9}
.sum{font-family:var(--mono);font-size:.83rem;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.when{color:var(--dim);font-size:.72rem;font-family:var(--mono);white-space:nowrap}
.detail{border-top:1px solid var(--line);padding:12px 16px 18px;font-size:.82rem}
.detail h4{
  margin:16px 0 6px;font-size:.66rem;text-transform:uppercase;letter-spacing:.14em;color:var(--dim);
}
.detail pre{
  margin:0;padding:10px 12px;background:#100e17;border:1px solid var(--line);border-radius:7px;
  font-family:var(--mono);font-size:.76rem;line-height:1.5;overflow-x:auto;white-space:pre-wrap;
  word-break:break-word;max-height:340px;
}
.events{list-style:none;margin:0;padding:0;font-family:var(--mono);font-size:.74rem}
.ev{display:grid;grid-template-columns:70px 78px 1fr;gap:8px;padding:4px 0;border-bottom:1px dashed #201c2b}
.ev.out{color:var(--sage)}
.ev-at{color:var(--dim)}
.ev-body{white-space:pre-wrap;word-break:break-word}
/* ---------- send panel ---------- */
.send h3{
  font-size:.72rem;text-transform:uppercase;letter-spacing:.12em;color:var(--muted);
  margin:20px 0 8px;display:flex;gap:8px;align-items:center;flex-wrap:wrap;
}
.send .hint{text-transform:none;letter-spacing:0;color:var(--dim);font-size:.72rem}
pre.cmd{
  margin:0;padding:10px 12px;background:#100e17;border:1px solid var(--line);border-radius:8px;
  font-family:var(--mono);font-size:.72rem;line-height:1.55;white-space:pre-wrap;word-break:break-all;
  cursor:copy;transition:border-color .15s ease;
}
pre.cmd:hover{border-color:#3a3350}
pre.cmd.copied{border-color:var(--sage);color:var(--sage)}
.mini{
  margin-left:auto;background:#211d2e;color:var(--ink);border:1px solid var(--line);
  border-radius:6px;padding:4px 10px;font:600 .68rem/1.4 var(--sans);cursor:pointer;
}
.mini:hover{border-color:var(--accent);color:var(--accent)}
.banner{
  margin:0 0 18px;padding:14px 16px;border:1px solid var(--err);border-radius:10px;
  background:#221317;color:#f0c9c2;font-size:.9rem;
}
```

**Step 7: Run to verify pass**

```bash
./scripts/dev.sh go test ./internal/web -count=1
```

Expected: `ok  	github.com/adam/dumpster/internal/web	0.00Xs` (2 tests).

**Step 8: Commit**

```bash
git add internal/web
git commit -m "feat(web): embed templates and stylesheet"
```

---

### Task 17 — `internal/app`: App struct, listeners, reaper, healthz

**Objective:** A server that binds `:8080`, answers `/healthz`, sweeps expired dumpsters, and shuts down cleanly.

**Files:**
- Create: `internal/app/app.go`
- Create: `internal/app/routes.go`
- Create: `internal/app/reaper.go`
- Create: `internal/app/http_util.go` (only `withHeaders` + `withConnAcceptedAt` in this task)
- Test: `internal/app/app_test.go`

**Step 1: Write the failing test** (`internal/app/app_test.go`)

```go
package app

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

// newTestApp starts an App on ephemeral ports and stops it when the test ends.
func newTestApp(t *testing.T, tweak ...func(*Config)) *App {
	t.Helper()
	cfg := DefaultConfig()
	cfg.HTTPAddr = "127.0.0.1:0"
	cfg.GRPCAddr = "127.0.0.1:0"
	cfg.TCPAddr = "127.0.0.1:0"
	cfg.UDPAddr = "127.0.0.1:0"
	cfg.ReapEvery = 20 * time.Millisecond
	for _, f := range tweak {
		f(&cfg)
	}
	a := New(cfg)
	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := a.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	return a
}

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return res, string(body)
}

func TestHealthz(t *testing.T) {
	a := newTestApp(t)
	res, body := get(t, a.BaseURL()+"/healthz")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if body != "{\"ok\":true}\n" {
		t.Fatalf("body = %q", body)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("content-type = %q", ct)
	}
}

func TestStartBindsHTTP(t *testing.T) {
	a := newTestApp(t)
	if a.Addrs().HTTP == "" {
		t.Fatal("no HTTP address recorded")
	}
	if got := a.BaseURL(); got[:7] != "http://" {
		t.Fatalf("BaseURL = %q, want http:// prefix", got)
	}
}

func TestReaperRemovesExpired(t *testing.T) {
	a := newTestApp(t, func(c *Config) {
		c.TTL = 40 * time.Millisecond
		c.ReapEvery = 15 * time.Millisecond
	})
	d, err := a.store.Create()
	if err != nil {
		t.Fatalf("store.Create: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if a.store.Len() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("dumpster %s still in the store after its ttl", d.ID)
}

func TestSecurityHeaders(t *testing.T) {
	a := newTestApp(t)
	res, _ := get(t, a.BaseURL()+"/healthz")
	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Robots-Tag":           "noindex, nofollow",
		"Access-Control-Allow-Origin": "*",
	} {
		if got := res.Header.Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
}
```

**Step 2: Run it, expect failure**

```bash
./scripts/dev.sh go test ./internal/app -count=1
```

Expected: `undefined: New`, `undefined: DefaultConfig` is already fixed — specifically `undefined: New`.

**Step 3: Write `internal/app/http_util.go`** (this task's slice only)

```go
package app

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"time"
)

type connAcceptedKey struct{}

// withConnAcceptedAt stashes the accept time so a handler can report how long a
// keep-alive connection sat before this request arrived.
func withConnAcceptedAt(ctx context.Context, c net.Conn) context.Context {
	return context.WithValue(ctx, connAcceptedKey{}, time.Now())
}

func connAcceptedAt(r *http.Request) (time.Time, bool) {
	t, ok := r.Context().Value(connAcceptedKey{}).(time.Time)
	return t, ok
}

// withHeaders stamps every response with the headers an unauthenticated debug
// endpoint needs: no sniffing, no indexing, and CORS open so a browser on any
// origin can dump into a dumpster.
func (a *App) withHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Robots-Tag", "noindex, nofollow")
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "*")
		h.Set("Access-Control-Allow-Headers", "*")
		h.Set("Access-Control-Expose-Headers", "*")
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self'; "+
				"script-src 'self'; connect-src 'self' ws: wss:; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

// writeJSON is the only JSON writer; it keeps errors unescaped so dumps of HTML
// stay readable.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
```

**Step 4: Write `internal/app/app.go`**

```go
// Package app wires listeners, storage and the web UI together.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/adam/dumpster/internal/hub"
	"github.com/adam/dumpster/internal/ratelimit"
	"github.com/adam/dumpster/internal/store"
)

// Addrs are the addresses the listeners actually bound to.
type Addrs struct {
	HTTP string
}

// App owns every listener and the state behind them.
type App struct {
	cfg   Config
	store *store.Store
	hub   *hub.Hub
	rl    *ratelimit.Limiter
	log   *slog.Logger

	httpSrv *http.Server
	httpLis net.Listener

	mu      sync.Mutex
	addrs   Addrs
	baseURL string

	ctx    context.Context
	cancel context.CancelFunc
}

// New builds an App. Nothing is bound until Start.
func New(cfg Config) *App {
	ctx, cancel := context.WithCancel(context.Background())
	return &App{
		cfg: cfg,
		store: store.New(store.Limits{
			MaxRecords:   cfg.MaxRecords,
			MaxEvents:    cfg.MaxEvents,
			MaxDumpsters: cfg.MaxDumpsters,
		}, cfg.TTL),
		hub:    hub.New(128),
		rl:     ratelimit.New(cfg.RentPerHour, time.Hour),
		log:    slog.Default(),
		ctx:    ctx,
		cancel: cancel,
	}
}

// Start binds the HTTP listener and serves it. It returns once the port is
// bound, so callers can read Addrs immediately.
func (a *App) Start() error {
	hl, err := net.Listen("tcp", a.cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen http: %w", err)
	}
	a.httpLis = hl

	base := strings.TrimRight(a.cfg.BaseURL, "/")
	if base == "" {
		base = "http://" + hl.Addr().String()
	}
	a.mu.Lock()
	a.addrs = Addrs{HTTP: hl.Addr().String()}
	a.baseURL = base
	a.mu.Unlock()

	a.httpSrv = &http.Server{
		Handler:           a.withHeaders(a.routes()),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       75 * time.Second,
		MaxHeaderBytes:    1 << 20,
		ConnContext:       withConnAcceptedAt,
		ErrorLog:          log.New(io.Discard, "", 0),
	}

	go func() { _ = a.httpSrv.Serve(hl) }()
	go a.reapLoop()
	return nil
}

// Shutdown stops accepting, drains the HTTP server, and ends the reaper.
func (a *App) Shutdown(ctx context.Context) error {
	a.cancel()
	if a.httpSrv == nil {
		return nil
	}
	if err := a.httpSrv.Shutdown(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// Addrs returns the bound addresses.
func (a *App) Addrs() Addrs {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.addrs
}

// BaseURL is the origin used for links and for the command hints.
func (a *App) BaseURL() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.baseURL
}

// Log exposes the app logger to handlers.
func (a *App) Log() *slog.Logger { return a.log }
```

**Step 5: Write `internal/app/routes.go`** (healthz only, for now)

```go
package app

import (
	"net/http"
)

// routes builds the mux. The "/" pattern is a catch-all: everything the mux does
// not claim (the debug page, its snapshots, and all ingest paths) lands there.
func (a *App) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.handleHealth)
	mux.HandleFunc("/", a.handleCatchAll)
	return mux
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleCatchAll will grow the debug page and the ingest dispatch; until then it
// is an honest 404.
func (a *App) handleCatchAll(w http.ResponseWriter, r *http.Request) {
	http.NotFound(w, r)
}
```

**Step 6: Write `internal/app/reaper.go`**

```go
package app

import "time"

// reapLoop deletes expired dumpsters and closes their subscribers. store.Get
// already hides expired ids, so this loop only reclaims memory.
func (a *App) reapLoop() {
	ticker := time.NewTicker(a.cfg.ReapEvery)
	defer ticker.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case now := <-ticker.C:
			for _, sid := range a.store.DeleteExpired(now) {
				a.hub.Close(sid)
				a.log.Info("dumpster reaped", "id", sid)
			}
			a.rl.Prune()
		}
	}
}
```

**Step 7: Run to verify pass**

```bash
./scripts/dev.sh go test ./internal/app -count=1
```

Expected: `ok  	github.com/adam/dumpster/internal/app	0.00Xs` (7 tests = 3 from Task 14's `config_test.go` + 4 here). Every count below is the whole `internal/app` package, config tests included.

**Step 8: Commit**

```bash
git add internal/app
git commit -m "feat(app): bind http, sweep expired dumpsters, serve healthz"
```

---

### Task 18 — `internal/app`: remote/timing/body helpers

**Objective:** The five helpers every ingest path shares, so no handler reinvents them.

**Files:**
- Modify: `internal/app/http_util.go`
- Test: `internal/app/http_util_test.go`

**Step 1: Write the failing test**

```go
package app

import (
	"encoding/base64"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/adam/dumpster/internal/model"
)

func TestReadBodyTruncates(t *testing.T) {
	data, trunc := readBody(strings.NewReader("0123456789"), 4)
	if string(data) != "0123" || !trunc {
		t.Fatalf("data = %q trunc = %v", data, trunc)
	}
	data, trunc = readBody(strings.NewReader("abc"), 4)
	if string(data) != "abc" || trunc {
		t.Fatalf("data = %q trunc = %v", data, trunc)
	}
}

func TestRemoteFromProxy(t *testing.T) {
	r := httptest.NewRequest("POST", "/d/x", nil)
	r.RemoteAddr = "10.0.0.9:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")

	direct := remoteFrom(r, false)
	if direct.IP != "10.0.0.9" || direct.Port != 5555 {
		t.Fatalf("direct = %+v", direct)
	}
	if direct.Forwarded == "" {
		t.Fatal("the raw X-Forwarded-For must always be kept")
	}

	proxied := remoteFrom(r, true)
	if proxied.IP != "203.0.113.7" {
		t.Fatalf("proxied ip = %q, want 203.0.113.7", proxied.IP)
	}
	if proxied.Addr != "10.0.0.9:5555" {
		t.Fatalf("proxied addr = %q, want the real socket address", proxied.Addr)
	}
}

func TestFillBodyTextJSONAndBinary(t *testing.T) {
	req := &model.Request{}
	fillBody(req, "application/json", []byte(`{"a":1}`))
	if req.BodyText == "" || req.Decoded == nil || req.Decoded.Format != "json" {
		t.Fatalf("json req = %+v", req)
	}
	if req.BodyBytes != 7 {
		t.Fatalf("BodyBytes = %d", req.BodyBytes)
	}

	bin := &model.Request{}
	fillBody(bin, "application/octet-stream", []byte{0xff, 0x00, 0xfe})
	if bin.BodyText != "" {
		t.Fatal("binary body must not be rendered as text")
	}
	want := base64.StdEncoding.EncodeToString([]byte{0xff, 0x00, 0xfe})
	if bin.BodyBase64 != want {
		t.Fatalf("BodyBase64 = %q, want %q", bin.BodyBase64, want)
	}
}

func TestTimingFromWithoutConn(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	start := time.Now().Add(-3 * time.Millisecond)
	got := timingFrom(r, start)
	if got.TotalUS < 2000 {
		t.Fatalf("TotalUS = %d, want >= 2000", got.TotalUS)
	}
	if got.ConnWaitUS != 0 {
		t.Fatalf("ConnWaitUS = %d, want 0 when the context has no accept time", got.ConnWaitUS)
	}
}
```

The test imports `model` because `Request` lives there — production code must not grow an alias just to shorten a test.

**Step 2: Run it, expect failure**

```bash
./scripts/dev.sh go test ./internal/app -count=1
```

Expected: `undefined: readBody`, `undefined: remoteFrom`, `undefined: fillBody`, `undefined: timingFrom`.

**Step 3: Write the implementation** (append to `internal/app/http_util.go`)

```go
// maxInlineText caps how much body text is inlined into a record; the JSON
// snapshot would otherwise carry megabytes.
const maxInlineText = 64 << 10

// readBody reads at most max bytes and reports whether it truncated.
func readBody(body io.Reader, max int) (data []byte, truncated bool) {
	data, err := io.ReadAll(io.LimitReader(body, int64(max)+1))
	if err != nil {
		return data, false
	}
	if len(data) > max {
		return data[:max], true
	}
	return data, false
}

// remoteFrom describes the caller. With trustProxy set, the first
// X-Forwarded-For hop becomes the reported IP, but the raw socket address and
// the header itself are always preserved.
func remoteFrom(r *http.Request, trustProxy bool) model.Remote {
	rem := model.Remote{Addr: r.RemoteAddr, IP: r.RemoteAddr}
	if host, port, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		rem.IP = host
		if p, convErr := strconv.Atoi(port); convErr == nil {
			rem.Port = p
		}
	}
	if r.TLS != nil {
		rem.TLS = true
		rem.TLSSNI = r.TLS.ServerName
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		rem.Forwarded = xff
		if trustProxy {
			first := strings.TrimSpace(strings.SplitN(xff, ",", 2)[0])
			if net.ParseIP(first) != nil {
				rem.IP = first
			}
		}
	}
	return rem
}

// remoteFromAddr is the net.Addr flavour, for TCP and UDP.
func remoteFromAddr(addr net.Addr) model.Remote {
	rem := model.Remote{Addr: addr.String(), IP: addr.String()}
	if host, port, err := net.SplitHostPort(addr.String()); err == nil {
		rem.IP = host
		if p, convErr := strconv.Atoi(port); convErr == nil {
			rem.Port = p
		}
	}
	return rem
}

// timingFrom measures a request against handler entry and, when the connection
// carried an accept time, against connection accept.
func timingFrom(r *http.Request, start time.Time) model.Timing {
	t := model.Timing{TotalUS: time.Since(start).Microseconds()}
	if accepted, ok := connAcceptedAt(r); ok {
		t.ConnWaitUS = start.Sub(accepted).Microseconds()
	}
	return t
}

// fillBody records the raw body (text when UTF-8, base64 when not) and attaches
// the decoded view.
func fillBody(req *model.Request, contentType string, body []byte) {
	req.BodyBytes = len(body)
	if len(body) == 0 {
		return
	}
	if utf8.Valid(body) {
		text := string(body)
		if len(text) > maxInlineText {
			text = text[:maxInlineText]
		}
		req.BodyText = text
	} else {
		req.BodyBase64 = base64.StdEncoding.EncodeToString(body)
	}
	req.Decoded = decode.Body(contentType, body)
}

// requestEnvelope builds the shared Request from an HTTP request, cloning the
// header map because the record outlives the request.
func requestEnvelope(r *http.Request, subpath string) *model.Request {
	headers := make(map[string][]string, len(r.Header))
	for k, v := range r.Header {
		headers[k] = append([]string(nil), v...)
	}
	return &model.Request{
		Protocol:    "http",
		Method:      r.Method,
		Path:        r.URL.Path,
		Subpath:     subpath,
		Query:       r.URL.RawQuery,
		Host:        r.Host,
		HTTPVersion: r.Proto,
		Headers:     headers,
		ContentLen:  r.ContentLength,
	}
}

// subOrRoot renders an empty subpath as "/".
func subOrRoot(sub string) string {
	if sub == "" {
		return "/"
	}
	return sub
}
```

Add these imports to `http_util.go`:

```go
import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/adam/dumpster/internal/decode"
	"github.com/adam/dumpster/internal/model"
)
```

**Step 4: Run to verify pass**

```bash
./scripts/dev.sh go test ./internal/app -count=1
```

Expected: `ok` (11 tests: 3 config + 4 `app_test.go` + 4 `http_util_test.go`).

**Step 5: Commit**

```bash
git add internal/app
git commit -m "feat(app): share remote, timing and body helpers across protocols"
```

---

### Task 19 — `internal/app/pages.go`: landing page and the CTA

**Objective:** `GET /` renders the single CTA; `POST /rent` mints a dumpster and redirects.

**Files:**
- Create: `internal/app/pages.go`
- Modify: `internal/app/routes.go` (register `/` and `/rent`, serve `/static/`)
- Modify: `internal/app/app_test.go` (add rent helpers + tests)

**Step 1: Append the failing tests** (`internal/app/app_test.go` — and add `"strings"` to that file's import block; it is not there yet)

```go
// noRedirect keeps 303s visible so tests can read the Location header.
var noRedirect = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func rent(t *testing.T, a *App) string {
	t.Helper()
	res, err := noRedirect.Post(a.BaseURL()+"/rent", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatalf("POST /rent: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", res.StatusCode)
	}
	loc := res.Header.Get("Location")
	if !strings.HasPrefix(loc, "/d/") {
		t.Fatalf("Location = %q, want /d/<id>", loc)
	}
	return strings.TrimPrefix(loc, "/d/")
}

func TestIndexHasExactlyOneCTA(t *testing.T) {
	a := newTestApp(t)
	res, body := get(t, a.BaseURL()+"/")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if got := strings.Count(body, "Rent a dumpster"); got != 1 {
		t.Fatalf("CTA appears %d times, want exactly 1", got)
	}
	if !strings.Contains(body, `action="/rent"`) {
		t.Fatal("the CTA must post to /rent")
	}
}

func TestRentMintsDumpster(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)
	if len(id) != 36 {
		t.Fatalf("id = %q, want a 36-char uuid", id)
	}
	if _, ok := a.store.Get(id); !ok {
		t.Fatalf("store has no dumpster %q after renting", id)
	}
}

func TestRentIsRateLimited(t *testing.T) {
	a := newTestApp(t, func(c *Config) { c.RentPerHour = 2 })
	rent(t, a)
	rent(t, a)

	res, err := noRedirect.Post(a.BaseURL()+"/rent", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatalf("third rent: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", res.StatusCode)
	}
	if res.Header.Get("Retry-After") == "" {
		t.Fatal("429 must carry Retry-After")
	}
}

func TestStaticServesStylesheet(t *testing.T) {
	a := newTestApp(t)
	res, body := get(t, a.BaseURL()+"/static/style.css")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if !strings.Contains(body, "--accent") {
		t.Fatal("stylesheet looks empty")
	}
}
```

**Step 2: Run it, expect failure**

```bash
./scripts/dev.sh go test ./internal/app -count=1
```

Expected: `status = 404, want 200` (the catch-all is still an honest 404).

**Step 3: Write `internal/app/pages.go`**

```go
package app

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/adam/dumpster/internal/store"
	"github.com/adam/dumpster/internal/web"
)

// render executes a template into a buffer first, so a template error cannot
// emit half a page with a 200 already on the wire.
func (a *App) render(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := web.Templates.ExecuteTemplate(&buf, name, data); err != nil {
		a.log.Error("render template", "template", name, "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// renderError is the one HTML error page.
func (a *App) renderError(w http.ResponseWriter, status int, title, message string) {
	a.render(w, status, "error.html", map[string]any{"Title": title, "Message": message})
}

// handleIndex renders the landing page: one heading, one CTA, nothing else.
func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	a.render(w, http.StatusOK, "index.html", map[string]any{
		"Active": a.store.Len(),
		"TTL":    humanDuration(a.cfg.TTL),
	})
}

// handleRent is the single call to action: mint, then redirect to the dumpster.
func (a *App) handleRent(w http.ResponseWriter, r *http.Request) {
	if !a.rl.Allow(a.clientIP(r)) {
		w.Header().Set("Retry-After", "600")
		a.renderError(w, http.StatusTooManyRequests, "Slow down",
			"You have rented too many dumpsters this hour. They expire on their own; try again shortly.")
		return
	}
	d, err := a.store.Create()
	switch {
	case errors.Is(err, store.ErrFull):
		a.renderError(w, http.StatusServiceUnavailable, "The yard is full",
			"Every dumpster is currently rented. They empty themselves after "+humanDuration(a.cfg.TTL)+".")
	case err != nil:
		a.log.Error("create dumpster", "err", err)
		a.renderError(w, http.StatusInternalServerError, "Could not rent", "Something broke in the yard.")
	default:
		http.Redirect(w, r, "/d/"+d.ID, http.StatusSeeOther)
	}
}

// humanDuration renders 12h0m0s as "12h", and 500ms as "500ms".
func humanDuration(d time.Duration) string {
	if d >= time.Hour && d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return d.String()
}

// clientIP is the address used for rate limiting.
func (a *App) clientIP(r *http.Request) string {
	return remoteFrom(r, a.cfg.TrustProxy).IP
}
```

**Step 4: Replace `internal/app/routes.go`**

```go
package app

import (
	"net/http"

	"github.com/adam/dumpster/internal/web"
)

// routes builds the mux. The "/" pattern is the catch-all: the debug page, its
// snapshots and every ingest path land there, dispatched by handleCatchAll.
func (a *App) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.handleIndex)
	mux.HandleFunc("POST /rent", a.handleRent)
	mux.HandleFunc("GET /healthz", a.handleHealth)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(web.Static))))
	mux.HandleFunc("/", a.handleCatchAll)
	return mux
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleCatchAll grows the debug page and ingest dispatch in later tasks; until
// then it is an honest 404.
func (a *App) handleCatchAll(w http.ResponseWriter, r *http.Request) {
	http.NotFound(w, r)
}
```

**Step 5: Run to verify pass**

```bash
./scripts/dev.sh go test ./internal/app -count=1
```

Expected: `ok` (15 tests: 11 from the earlier files + 4 new).

**Step 6: Commit**

```bash
git add internal/app
git commit -m "feat(app): rent a dumpster from a single call to action"
```

---

### Task 20 — `internal/app/ingest_http.go`: dump any HTTP request

**Objective:** Every request under `/d/{id}/…` becomes a record with headers, remote, timings and body.

**Files:**
- Create: `internal/app/ingest_http.go`
- Modify: `internal/app/routes.go` (dispatch to the ingester)
- Test: `internal/app/ingest_http_test.go`

**Step 1: Write the failing test**

```go
package app

import (
	"net/http"
	"strings"
	"testing"

	"github.com/adam/dumpster/internal/model"
)

func records(t *testing.T, a *App, id string) []model.Record {
	t.Helper()
	recs, ok := a.store.List(id)
	if !ok {
		t.Fatalf("store has no dumpster %q", id)
	}
	return recs
}

func TestHTTPIngestDumpsRequest(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	req, err := http.NewRequest(http.MethodPost,
		a.BaseURL()+"/d/"+id+"/v1/orders?page=2", strings.NewReader(`{"sku":"A1"}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Trace", "abc123")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}

	recs := records(t, a, id)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Kind != model.KindHTTP {
		t.Fatalf("kind = %q", rec.Kind)
	}
	if rec.Seq != 1 {
		t.Fatalf("seq = %d, want 1", rec.Seq)
	}
	if rec.Req.Method != http.MethodPost || rec.Req.Subpath != "/v1/orders" {
		t.Fatalf("req = %+v", rec.Req)
	}
	if rec.Req.Query != "page=2" {
		t.Fatalf("query = %q", rec.Req.Query)
	}
	if got := rec.Req.Headers["X-Trace"]; len(got) != 1 || got[0] != "abc123" {
		t.Fatalf("X-Trace header = %v", got)
	}
	if rec.Req.BodyText != `{"sku":"A1"}` {
		t.Fatalf("body text = %q", rec.Req.BodyText)
	}
	if rec.Req.Decoded == nil || rec.Req.Decoded.Format != "json" {
		t.Fatalf("decoded = %+v", rec.Req.Decoded)
	}
	if rec.Remote.IP != "127.0.0.1" {
		t.Fatalf("remote ip = %q, want 127.0.0.1", rec.Remote.IP)
	}
	if rec.Timing.TotalUS <= 0 {
		t.Fatalf("timing = %+v", rec.Timing)
	}
}

func TestHTTPIngestUnknownDumpster(t *testing.T) {
	a := newTestApp(t)
	res, _ := get(t, a.BaseURL()+"/d/00000000-0000-7000-8000-000000000000/anything")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
}

func TestHTTPIngestGarbageID(t *testing.T) {
	a := newTestApp(t)
	res, _ := get(t, a.BaseURL()+"/d/not-a-uuid/anything")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
}

func TestHTTPIngestAcceptsEveryMethod(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req, _ := http.NewRequest(method, a.BaseURL()+"/d/"+id+"/x", nil)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", method, res.StatusCode)
		}
	}
	if got := len(records(t, a, id)); got != 4 {
		t.Fatalf("records = %d, want 4", got)
	}
}
```

Add `"strings"` to the test file's imports (it is already imported by Task 19's tests — verify, do not duplicate).

**Step 2: Run it, expect failure**

```bash
./scripts/dev.sh go test ./internal/app -count=1
```

Expected: `records = 0, want 1` — the catch-all still 404s.

**Step 3: Write `internal/app/ingest_http.go`**

```go
package app

import (
	"net/http"
	"time"

	"github.com/adam/dumpster/internal/id"
	"github.com/adam/dumpster/internal/model"
)

// handleHTTPIngest records any request aimed at /d/{id}/… . The endpoint is a
// sink: it always answers 200 with the sequence number it assigned.
func (a *App) handleHTTPIngest(w http.ResponseWriter, r *http.Request, sid, sub string) {
	start := time.Now()
	d, ok := a.mustDumpster(w, sid)
	if !ok {
		return
	}

	// A CORS preflight is still traffic worth dumping, but it has no body and
	// the browser expects 204.
	if r.Method == http.MethodOptions {
		if _, err := a.recordHTTP(d, r, sub, start, nil, false, 0); err != nil {
			a.log.Error("record preflight", "err", err)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	bodyStart := time.Now()
	body, truncated := readBody(r.Body, a.cfg.MaxBody)
	bodyUS := time.Since(bodyStart).Microseconds()

	seq, err := a.recordHTTP(d, r, sub, start, body, truncated, bodyUS)
	if err != nil {
		a.log.Error("record http", "err", err)
		http.Error(w, "could not record", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"dumpster":       d.ID,
		"seq":            seq,
		"received_bytes": len(body),
		"truncated":      truncated,
	})
}

// recordHTTP builds, stores and broadcasts one HTTP record.
func (a *App) recordHTTP(d model.Dumpster, r *http.Request, sub string, start time.Time, body []byte, truncated bool, bodyUS int64) (uint64, error) {
	req := requestEnvelope(r, subOrRoot(sub))
	req.Truncated = truncated
	fillBody(req, r.Header.Get("Content-Type"), body)

	timing := timingFrom(r, start)
	rec := &model.Record{
		Kind:       model.KindHTTP,
		ReceivedAt: start,
		Remote:     remoteFrom(r, a.cfg.TrustProxy),
		Req:        req,
		Timing: model.Timing{
			ConnWaitUS: timing.ConnWaitUS,
			BodyUS:     bodyUS,
			TotalUS:    time.Since(start).Microseconds(),
		},
	}
	stored, err := a.store.Add(d.ID, rec)
	if err != nil {
		return 0, err
	}
	a.publishNew(d.ID, &stored)
	return stored.Seq, nil
}

// mustDumpster resolves an id or writes a plain-text 404. Ingest callers are
// scripts, not browsers, so an HTML error page would be noise.
func (a *App) mustDumpster(w http.ResponseWriter, sid string) (model.Dumpster, bool) {
	canonical, err := id.Canonical(sid)
	if err != nil {
		http.Error(w, "not a dumpster url", http.StatusNotFound)
		return model.Dumpster{}, false
	}
	d, ok := a.store.Get(canonical)
	if !ok {
		http.Error(w, "unknown or expired dumpster", http.StatusNotFound)
		return model.Dumpster{}, false
	}
	return d, true
}
```

**Step 4: Write `internal/app/live.go`** (message type + publish helpers only; the socket lands in Task 24)

```go
package app

import "github.com/adam/dumpster/internal/model"

// wsMsg is the single shape pushed to dashboard sockets and snapshots.
type wsMsg struct {
	Type     string          `json:"type"` // snapshot | new | update | expired
	Record   *model.Record   `json:"record,omitempty"`
	Records  []model.Record  `json:"records,omitempty"`
	Dumpster *model.Dumpster `json:"dumpster,omitempty"`
}

// publishNew tells every open debug page that a row was added.
func (a *App) publishNew(sid string, rec *model.Record) {
	a.hub.Publish(sid, wsMsg{Type: "new", Record: rec})
}

// publishUpdate re-sends a whole record after an event was appended or it was
// closed, so the browser can replace one row without diffing.
func (a *App) publishUpdate(sid string, rec *model.Record) {
	a.hub.Publish(sid, wsMsg{Type: "update", Record: rec})
}
```

**Step 5: Replace `handleCatchAll` in `internal/app/routes.go`** and add the path splitter

```go
// handleCatchAll dispatches everything the mux does not claim. Reserved
// suffixes under a dumpster are added by later phases.
func (a *App) handleCatchAll(w http.ResponseWriter, r *http.Request) {
	sid, sub, ok := splitDumpsterPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	a.handleHTTPIngest(w, r, sid, sub)
}

// splitDumpsterPath turns "/d/<id>/a/b" into ("<id>", "/a/b", true).
func splitDumpsterPath(p string) (sid, sub string, ok bool) {
	if len(p) < 4 || p[:3] != "/d/" {
		return "", "", false
	}
	tail := p[3:]
	if i := strings.IndexByte(tail, '/'); i >= 0 {
		if i == 0 {
			return "", "", false
		}
		return tail[:i], tail[i:], true
	}
	return tail, "", true
}
```

Add `"strings"` to `routes.go` imports.

**Step 6: Run to verify pass**

```bash
./scripts/dev.sh go test ./internal/app -count=1
```

Expected: `ok` (19 tests: 15 + the 4 ingest tests).

**Step 7: Commit**

```bash
git add internal/app
git commit -m "feat(app): dump http requests with headers, body and timings"
```

---

### Task 21 — Body edge cases: binary, form, multipart, truncation

**Objective:** Prove the body pipeline end-to-end through a real HTTP request, not just the decoder's unit tests.

**Files:**
- Test: `internal/app/ingest_http_test.go` (no production code — Task 20 already routes through `fillBody`)

**Step 1: Append the tests**

```go
func TestHTTPIngestBinaryBodyIsBase64(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	payload := []byte{0x00, 0xff, 0xfe, 0x10}
	req, _ := http.NewRequest(http.MethodPost, a.BaseURL()+"/d/"+id+"/bin", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/octet-stream")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	res.Body.Close()

	rec := records(t, a, id)[0]
	if rec.Req.BodyText != "" {
		t.Fatalf("binary body leaked into text: %q", rec.Req.BodyText)
	}
	want := base64.StdEncoding.EncodeToString(payload)
	if rec.Req.BodyBase64 != want {
		t.Fatalf("base64 = %q, want %q", rec.Req.BodyBase64, want)
	}
	if rec.Req.Decoded != nil {
		t.Fatalf("binary body should not decode, got %+v", rec.Req.Decoded)
	}
}

func TestHTTPIngestFormBody(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	res, err := http.PostForm(a.BaseURL()+"/d/"+id+"/form", url.Values{"a": {"1"}, "b": {"2", "3"}})
	if err != nil {
		t.Fatalf("PostForm: %v", err)
	}
	res.Body.Close()

	rec := records(t, a, id)[0]
	if rec.Req.Decoded == nil || rec.Req.Decoded.Format != "form" {
		t.Fatalf("decoded = %+v", rec.Req.Decoded)
	}
	// Assert the rendered shape, not the Go type: the memory backend keeps
	// url.Values while the redis backend round-trips through JSON into
	// map[string]any. Marshalling is the one view both agree on.
	encoded, err := json.Marshal(rec.Req.Decoded.Value)
	if err != nil {
		t.Fatalf("marshal decoded: %v", err)
	}
	if !strings.Contains(string(encoded), `"b":["2","3"]`) {
		t.Fatalf("decoded value = %s, want b to carry two values", encoded)
	}
}

func TestHTTPIngestTruncatesHugeBody(t *testing.T) {
	a := newTestApp(t, func(c *Config) { c.MaxBody = 16 })
	id := rent(t, a)

	req, _ := http.NewRequest(http.MethodPost, a.BaseURL()+"/d/"+id+"/big",
		strings.NewReader(strings.Repeat("x", 500)))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()

	var ack struct {
		Received int  `json:"received_bytes"`
		Trunc    bool `json:"truncated"`
	}
	if err := json.NewDecoder(res.Body).Decode(&ack); err != nil {
		t.Fatalf("decode ack: %v", err)
	}
	if ack.Received != 16 || !ack.Trunc {
		t.Fatalf("ack = %+v, want 16 bytes truncated", ack)
	}
	rec := records(t, a, id)[0]
	if !rec.Req.Truncated || rec.Req.BodyBytes != 16 {
		t.Fatalf("record = %+v", rec.Req)
	}
}
```

Add `"bytes"`, `"encoding/base64"`, `"encoding/json"`, `"net/url"` to the test imports.

**Step 2: Run it**

```bash
./scripts/dev.sh go test ./internal/app -count=1 -run 'TestHTTPIngest' -v
```

Expected: **all pass** (`--- PASS` for each). If `TestHTTPIngestFormBody` fails on the type assertion, the record was copied through the store and `url.Values` became a `map[string]any` — that is expected; the assertion above already accounts for it.

**Step 3: Commit**

```bash
git add internal/app
git commit -m "test(app): cover binary, form and truncated bodies"
```

---

### Task 22 — `GET /d/{id}/_records.json` and the debug page shell

**Objective:** A JSON snapshot of the dumpster, plus the HTML shell the JS will populate.

**Files:**
- Modify: `internal/app/routes.go` (register `GET /d/{id}`)
- Modify: `internal/app/pages.go` (page + snapshot handlers)
- Create: `internal/web/templates/debug.html`
- Test: `internal/app/ingest_http_test.go`

**Step 1: Append the failing tests**

```go
func TestRecordsJSONNewestFirst(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	for _, path := range []string{"/one", "/two", "/three"} {
		res, err := http.Post(a.BaseURL()+"/d/"+id+path, "text/plain", strings.NewReader("hi"))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		res.Body.Close()
	}

	res, body := get(t, a.BaseURL()+"/d/"+id+"/_records.json")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var snap struct {
		Dumpster model.Dumpster `json:"dumpster"`
		Records  []model.Record `json:"records"`
	}
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode snapshot: %v\n%s", err, body)
	}
	if snap.Dumpster.ID != id {
		t.Fatalf("dumpster id = %q, want %q", snap.Dumpster.ID, id)
	}
	if len(snap.Records) != 3 {
		t.Fatalf("records = %d, want 3", len(snap.Records))
	}
	if snap.Records[0].Seq != 3 {
		t.Fatalf("first seq = %d, want 3 (newest first)", snap.Records[0].Seq)
	}
}

func TestDebugPageShell(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	res, body := get(t, a.BaseURL()+"/d/"+id)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	for _, want := range []string{
		`data-id="` + id + `"`,
		`data-grpc="`,
		`data-tcp="`,
		`data-udp="`,
		`/static/app.js`,
		`id="records"`,
		`Rent a dumpster`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("debug page is missing %q", want)
		}
	}
}

func TestDebugPageGoneDumpster(t *testing.T) {
	a := newTestApp(t)
	res, body := get(t, a.BaseURL()+"/d/00000000-0000-7000-8000-000000000000")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
	if !strings.Contains(body, "gone") && !strings.Contains(body, "emptied") {
		t.Fatalf("404 page should explain the dumpster is gone, got:\n%s", body)
	}
}
```

**Step 2: Run it, expect failure**

```bash
./scripts/dev.sh go test ./internal/app -count=1 -run 'TestRecordsJSON|TestDebugPage'
```

Expected: `status = 404, want 200`.

**Step 3: Write `internal/web/templates/debug.html`**

```html
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Dumpster {{.ID}}</title>
  <link rel="stylesheet" href="/static/style.css">
  <script src="/static/app.js" defer></script>
</head>
<body class="debug"
      data-id="{{.ID}}"
      data-origin="{{.Origin}}"
      data-grpc="{{.PublicGRPC}}"
      data-tcp="{{.PublicTCP}}"
      data-udp="{{.PublicUDP}}"
      data-expires="{{.ExpiresAt}}">
  <header class="bar">
    <div class="who">
      <h1>Dumpster <span class="mono">{{.ID}}</span></h1>
      <p class="sub">burns in <strong id="ttl">&mdash;</strong> &middot; anyone with this link can read it
        &middot; <a href="/">Rent a dumpster</a></p>
    </div>
    <div class="status" id="status" aria-live="polite">connecting&hellip;</div>
  </header>

  <main class="grid">
    <section class="feed">
      <h2>Traffic <span class="count" id="count">0</span></h2>
      <p class="empty" id="empty">Nothing yet. Send something &mdash; rows appear the moment they land.</p>
      <ul class="records" id="records"></ul>
    </section>

    <aside class="send">
      <h2>How to send</h2>

      <h3>HTTP <span class="hint">any method, any path</span></h3>
      <pre class="cmd" data-cmd="http"></pre>

      <h3>SSE <span class="hint">streams back to you</span>
        <button class="mini" id="btn-sse" type="button">Trigger SSE</button></h3>
      <pre class="cmd" data-cmd="sse"></pre>

      <h3>gRPC <span class="hint">path or x-dumpster-id</span></h3>
      <pre class="cmd" data-cmd="grpc"></pre>

      <h3>WebSocket <span class="hint">echoes your frames</span></h3>
      <pre class="cmd" data-cmd="ws"></pre>

      <h3>TCP <span class="hint">id on the first line</span></h3>
      <pre class="cmd" data-cmd="tcp"></pre>

      <h3>UDP <span class="hint">id in the first 36 bytes</span></h3>
      <pre class="cmd" data-cmd="udp"></pre>

      <p class="fine">Click any command to copy it.</p>
    </aside>
  </main>
</body>
</html>
```

**Step 4: Append to `internal/app/pages.go`**

```go
// handleDumpsterPage renders the shell; the JS fills the list from
// /_records.json and then from the live socket.
func (a *App) handleDumpsterPage(w http.ResponseWriter, r *http.Request) {
	canonical, err := id.Canonical(r.PathValue("id"))
	if err != nil {
		a.renderError(w, http.StatusNotFound, "Not a dumpster", "That link is not a dumpster at all.")
		return
	}
	d, ok := a.store.Get(canonical)
	if !ok {
		a.renderError(w, http.StatusNotFound, "This dumpster is gone",
			"It was emptied after "+humanDuration(a.cfg.TTL)+". Rent a new one and keep dumping.")
		return
	}
	a.render(w, http.StatusOK, "debug.html", map[string]any{
		"ID":        d.ID,
		"Origin":    a.BaseURL(),
		"ExpiresAt": d.ExpiresAt.UTC().Format(time.RFC3339),
		// The gRPC/TCP/UDP listeners do not exist until Phases 6-8, so advertise
		// the configured ports; Task 31 swaps these for the bound addresses.
		"PublicGRPC": a.publicAddr(a.cfg.PublicGRPC, a.cfg.GRPCAddr),
		"PublicTCP":  a.publicAddr(a.cfg.PublicTCP, a.cfg.TCPAddr),
		"PublicUDP":  a.publicAddr(a.cfg.PublicUDP, a.cfg.UDPAddr),
	})
}

// handleRecordsJSON returns the snapshot the page loads before the socket opens,
// and the thing to curl when you want to read a dumpster from a script.
func (a *App) handleRecordsJSON(w http.ResponseWriter, r *http.Request) {
	canonical, err := id.Canonical(r.PathValue("id"))
	if err != nil {
		http.Error(w, "not a dumpster url", http.StatusNotFound)
		return
	}
	d, ok := a.store.Get(canonical)
	if !ok {
		http.Error(w, "unknown or expired dumpster", http.StatusNotFound)
		return
	}
	recs, _ := a.store.List(d.ID)
	writeJSON(w, http.StatusOK, wsMsg{Type: "snapshot", Dumpster: &d, Records: recs})
}

// publicAddr renders the address a human types into nc or grpcurl: the explicit
// override if configured, otherwise the BaseURL host with the bound port.
func (a *App) publicAddr(explicit, bound string) string {
	if explicit != "" {
		return explicit
	}
	u, err := url.Parse(a.BaseURL())
	if err != nil || u.Hostname() == "" {
		return bound
	}
	_, port, err := net.SplitHostPort(bound)
	if err != nil || port == "" {
		return bound
	}
	return net.JoinHostPort(u.Hostname(), port)
}
```

Add `"net"`, `"net/url"` and `"github.com/adam/dumpster/internal/id"` to `pages.go` imports.

**Step 5: Register the page route** — in `internal/app/routes.go`, add one line before the catch-all:

```go
	mux.HandleFunc("GET /d/{id}", a.handleDumpsterPage)
```

and extend the catch-all's reserved suffixes:

```go
func (a *App) handleCatchAll(w http.ResponseWriter, r *http.Request) {
	sid, sub, ok := splitDumpsterPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if sub == "/_records.json" {
		r.SetPathValue("id", sid)
		a.handleRecordsJSON(w, r)
		return
	}
	a.handleHTTPIngest(w, r, sid, sub)
}
```

**Step 6: Run to verify pass**

```bash
./scripts/dev.sh go test ./internal/app -count=1
```

Expected: `ok` (25 tests: 19 + 3 body edge cases from Task 21 + 3 snapshot/page tests).

**Step 7: Commit**

```bash
git add internal/app internal/web
git commit -m "feat(app): serve the debug page shell and its json snapshot"
```

---
## 7. Phase 3 — Live dashboard

**Phase gate:** `./scripts/dev.sh go test ./internal/app -count=1 -run TestLive` prints `ok`, and the page opens in a browser with rows appearing as you fill it.

---

### Task 23 — `internal/app/live.go`: the dashboard socket

**Objective:** One WebSocket per open debug page: snapshot first, then `new`/`update`/`expired`.

**Files:**
- Modify: `internal/app/live.go` (add the handler below the message type)
- Modify: `internal/app/routes.go` (reserve `/_live`)
- Test: `internal/app/live_test.go`

**Step 1: Write the failing test**

```go
package app

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func wsURL(a *App, path string) string {
	return "ws" + strings.TrimPrefix(a.BaseURL(), "http") + path
}

func TestLiveSendsSnapshotThenNewRecords(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, wsURL(a, "/d/"+id+"/_live"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	var first wsMsg
	if err := wsjson.Read(ctx, c, &first); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if first.Type != "snapshot" {
		t.Fatalf("first message type = %q, want snapshot", first.Type)
	}
	if first.Dumpster == nil || first.Dumpster.ID != id {
		t.Fatalf("snapshot dumpster = %+v", first.Dumpster)
	}
	if len(first.Records) != 0 {
		t.Fatalf("snapshot has %d records, want 0", len(first.Records))
	}

	res, err := http.Post(a.BaseURL()+"/d/"+id+"/hello", "text/plain", strings.NewReader("yo"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	res.Body.Close()

	var next wsMsg
	if err := wsjson.Read(ctx, c, &next); err != nil {
		t.Fatalf("read new: %v", err)
	}
	if next.Type != "new" || next.Record == nil {
		t.Fatalf("got %+v", next)
	}
	if next.Record.Req.BodyText != "yo" {
		t.Fatalf("body = %q, want yo", next.Record.Req.BodyText)
	}
}

func TestLiveSendsExpiredWhenReaped(t *testing.T) {
	a := newTestApp(t, func(c *Config) {
		c.TTL = 400 * time.Millisecond
		c.ReapEvery = 20 * time.Millisecond
	})
	id := rent(t, a)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, wsURL(a, "/d/"+id+"/_live"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	var snap wsMsg
	if err := wsjson.Read(ctx, c, &snap); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}

	var got wsMsg
	if err := wsjson.Read(ctx, c, &got); err != nil {
		t.Fatalf("expected an expired message before the socket closed, got %v", err)
	}
	if got.Type != "expired" {
		t.Fatalf("type = %q, want expired", got.Type)
	}
}
```

**Step 2: Run it, expect failure**

```bash
./scripts/dev.sh go test ./internal/app -count=1 -run TestLive
```

Expected: `dial: failed to WebSocket dial: expected handshake response status code 101 but got 200` — an unreserved `/_live` falls through to `handleHTTPIngest`, which answers 200 to everything. The test is still correctly red: it wanted a socket.

**Step 3: Append the handler to `internal/app/live.go`**

Add these imports to the file:

```go
import (
	"context"
	"net/http"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/adam/dumpster/internal/id"
	"github.com/adam/dumpster/internal/model"
)
```

```go
// handleLive streams the dumpster to one browser socket. The client is
// write-only: frames it sends are discarded by CloseRead, which also gives us a
// context cancelled the moment the tab closes.
func (a *App) handleLive(w http.ResponseWriter, r *http.Request, sid string) {
	canonical, err := id.Canonical(sid)
	if err != nil {
		http.Error(w, "not a dumpster url", http.StatusNotFound)
		return
	}
	d, ok := a.store.Get(canonical)
	if !ok {
		http.Error(w, "unknown or expired dumpster", http.StatusNotFound)
		return
	}

	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()

	ctx := c.CloseRead(context.Background())

	// Subscribe before the snapshot: a record that lands in between is sent as
	// "new" and deduplicated by seq in the browser, which is harmless. The
	// reverse order would silently lose it.
	sub, cancel := a.hub.Subscribe(d.ID)
	defer cancel()

	recs, _ := a.store.List(d.ID)
	if err := wsjson.Write(ctx, c, wsMsg{Type: "snapshot", Dumpster: &d, Records: recs}); err != nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case raw, open := <-sub:
			if !open {
				_ = wsjson.Write(ctx, c, wsMsg{Type: "expired"})
				_ = c.Close(websocket.StatusGoingAway, "dumpster expired")
				return
			}
			if err := c.Write(ctx, websocket.MessageText, raw); err != nil {
				return
			}
		}
	}
}
```

**Step 4: Reserve the path** — in `internal/app/routes.go`, inside `handleCatchAll`, add a case:

```go
	if sub == "/_live" {
		a.handleLive(w, r, sid)
		return
	}
```

Place it above the `/_records.json` case.

**Step 5: Run to verify pass**

```bash
./scripts/dev.sh go test ./internal/app -count=1 -run TestLive -v
```

Expected: two `--- PASS` lines.

**Step 6: Commit**

```bash
git add internal/app go.mod                              # go.mod gains coder/websocket here
git commit -m "feat(app): stream new and updated records over a dashboard websocket"
```

---

### Task 24 — `internal/web/static/app.js`: the whole front-end

**Objective:** Render rows, prepend them live, expand details, and offer copy-paste commands per protocol.

**Files:**
- Create: `internal/web/static/app.js`
- Test: `internal/app/ingest_http_test.go` (served-asset assertions only; JS behaviour is verified in a browser — see Step 4)

**Step 1: Write the failing test** (append to `internal/app/ingest_http_test.go`)

```go
func TestStaticServesAppJS(t *testing.T) {
	a := newTestApp(t)
	res, body := get(t, a.BaseURL()+"/static/app.js")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	for _, want := range []string{"new WebSocket", "EventSource", "textContent"} {
		if !strings.Contains(body, want) {
			t.Fatalf("app.js is missing %q", want)
		}
	}
	if strings.Contains(body, "innerHTML") {
		t.Fatal("app.js must never assign innerHTML with request-derived data")
	}
}
```

**Step 2: Run it, expect failure**

```bash
./scripts/dev.sh go test ./internal/app -count=1 -run TestStaticServesAppJS
```

Expected: `status = 404, want 200`.

**Step 3: Write `internal/web/static/app.js`**

```javascript
(() => {
  'use strict';

  const body = document.body;
  const ID = body.dataset.id;
  const ORIGIN = body.dataset.origin;
  const GRPC = body.dataset.grpc;
  const TCP = body.dataset.tcp;
  const UDP = body.dataset.udp;

  const listEl = document.getElementById('records');
  const emptyEl = document.getElementById('empty');
  const countEl = document.getElementById('count');
  const statusEl = document.getElementById('status');
  const ttlEl = document.getElementById('ttl');

  /** seq -> { li, isOpen } */
  const rows = new Map();

  // -- DOM helpers: build nodes with textContent, never raw markup ----------
  function el(tag, props, ...kids) {
    const node = document.createElement(tag);
    if (props) {
      for (const [key, value] of Object.entries(props)) {
        if (value === null || value === undefined) continue;
        if (key === 'text') node.textContent = value;
        else if (key === 'class') node.className = value;
        else if (key.startsWith('on')) node.addEventListener(key.slice(2), value);
        else node.setAttribute(key, value);
      }
    }
    for (const kid of kids) if (kid !== null && kid !== undefined) node.append(kid);
    return node;
  }

  // -- formatting ------------------------------------------------------------
  const fmtTime = (iso) => new Date(iso).toLocaleTimeString([], { hour12: false });

  function ago(iso) {
    const seconds = Math.max(0, Math.floor((Date.now() - new Date(iso).getTime()) / 1000));
    if (seconds < 5) return 'now';
    if (seconds < 60) return seconds + 's ago';
    const minutes = Math.floor(seconds / 60);
    if (minutes < 60) return minutes + 'm ago';
    return Math.floor(minutes / 60) + 'h ago';
  }

  function bytes(n) {
    if (!n) return '0 B';
    if (n < 1024) return n + ' B';
    if (n < 1048576) return (n / 1024).toFixed(1) + ' kB';
    return (n / 1048576).toFixed(1) + ' MB';
  }

  function summary(rec) {
    const req = rec.req || {};
    switch (rec.kind) {
      case 'http':
        return (req.method || '?') + ' ' + (req.subpath || '/') + (req.query ? '?' + req.query : '');
      case 'grpc':
        return req.path || 'gRPC call';
      case 'sse':
        return 'event stream' + (req.query ? '?' + req.query : '');
      case 'ws':
        return 'websocket session';
      case 'tcp':
        return 'tcp session' + (rec.meta && rec.meta.local ? ' · ' + rec.meta.local : '');
      case 'udp':
        return 'udp datagram';
      default:
        return rec.kind || '?';
    }
  }

  function sizeOf(rec) {
    if (rec.req && rec.req.body_bytes) return rec.req.body_bytes;
    if (rec.events && rec.events.length) {
      return rec.events.reduce((sum, ev) => sum + (ev.bytes || 0), 0);
    }
    return 0;
  }

  // -- detail pane ----------------------------------------------------------
  function detailNode(rec) {
    const wrap = el('div', { class: 'detail' });

    function block(label, value) {
      if (value === null || value === undefined || value === '') return;
      if (typeof value === 'object' && !Object.keys(value).length) return;
      wrap.append(
        el('h4', { text: label }),
        el('pre', { text: typeof value === 'string' ? value : JSON.stringify(value, null, 2) })
      );
    }

    const req = rec.req || {};
    block('request', {
      protocol: req.protocol,
      method: req.method,
      path: req.path,
      subpath: req.subpath,
      query: req.query,
      host: req.host,
      http_version: req.http_version,
      content_length: req.content_length,
      body_bytes: req.body_bytes,
      truncated: req.truncated || undefined,
    });
    block('headers', req.headers);
    block('body (raw)', req.body_text);
    block('body (base64)', req.body_base64);
    if (req.decoded) {
      block('decoded (' + req.decoded.format + ')',
        req.decoded.error ? 'decode error: ' + req.decoded.error : req.decoded.value);
    }
    block('remote', rec.remote);
    block('timing (µs)', rec.timing);
    block('meta', rec.meta);
    if (rec.close_note) block('closed', rec.close_note);

    if (rec.events && rec.events.length) {
      const dropped = rec.dropped_events ? ', ' + rec.dropped_events + ' dropped' : '';
      wrap.append(el('h4', { text: 'events (' + rec.events.length + dropped + ')' }));
      const ul = el('ul', { class: 'events' });
      for (const ev of rec.events.slice(0, 200)) {
        ul.append(el('li', { class: 'ev ' + (ev.dir === 'out' ? 'out' : 'in') },
          el('span', { class: 'ev-at', text: fmtTime(ev.at) }),
          el('span', { class: 'ev-type', text: ev.type || '' }),
          el('span', {
            class: 'ev-body',
            text: ev.text !== undefined && ev.text !== ''
              ? ev.text
              : (ev.base64 ? '[binary ' + bytes(ev.bytes) + ']' : ''),
          })));
      }
      wrap.append(ul);
    }
    return wrap;
  }

  // -- rows -----------------------------------------------------------------
  function makeRow(rec) {
    const head = el('button', { class: 'row', type: 'button' },
      el('span', { class: 'badge k-' + rec.kind, text: String(rec.kind).toUpperCase() }),
      el('span', { class: 'sum', text: summary(rec) }),
      el('span', {
        class: 'when',
        text: fmtTime(rec.received_at) + ' · ' + bytes(sizeOf(rec)) + ' · ' +
          ((rec.remote && rec.remote.ip) || '?'),
      }));
    const li = el('li', {
      class: 'rec' + (rec.closed ? ' is-closed' : ''),
      'data-seq': String(rec.seq),
    }, head);

    let open = false;
    head.addEventListener('click', () => {
      open = !open;
      li.classList.toggle('open', open);
      li.replaceChildren(head);
      if (open) li.append(detailNode(rec));
    });
    return { li, isOpen: () => open };
  }

  function renderCount() {
    countEl.textContent = String(rows.size);
    emptyEl.hidden = rows.size > 0;
  }

  function loadSnapshot(records) {
    listEl.replaceChildren();
    rows.clear();
    for (const rec of records) {
      const fresh = makeRow(rec);
      rows.set(rec.seq, fresh);
      listEl.append(fresh.li);
    }
    renderCount();
  }

  function upsert(rec) {
    const existing = rows.get(rec.seq);
    if (existing) {
      const wasOpen = existing.isOpen();
      const fresh = makeRow(rec);
      existing.li.replaceWith(fresh.li);
      if (wasOpen) fresh.li.querySelector('.row').click();
      rows.set(rec.seq, fresh);
      return;
    }
    const fresh = makeRow(rec);
    rows.set(rec.seq, fresh);
    const first = listEl.firstElementChild;
    if (first && Number(first.dataset.seq) < rec.seq) listEl.prepend(fresh.li);
    else listEl.append(fresh.li);
  }

  // -- socket ---------------------------------------------------------------
  function setStatus(text) { statusEl.textContent = text; }

  function onMessage(raw) {
    let msg;
    try { msg = JSON.parse(raw); } catch { return; }
    switch (msg.type) {
      case 'snapshot':
        loadSnapshot(msg.records || []);
        break;
      case 'new':
      case 'update':
        if (msg.record) { upsert(msg.record); renderCount(); }
        break;
      case 'expired':
        setStatus('expired');
        document.querySelector('main').prepend(el('div', {
          class: 'banner',
          text: 'This dumpster has been emptied. Rent a new one to keep dumping.',
        }));
        break;
      default:
        break;
    }
  }

  let retry = 0;
  function connect() {
    const scheme = location.protocol === 'https:' ? 'wss' : 'ws';
    const socket = new WebSocket(scheme + '://' + location.host + '/d/' + ID + '/_live');
    socket.addEventListener('open', () => { retry = 0; setStatus('live'); });
    socket.addEventListener('message', (event) => onMessage(event.data));
    socket.addEventListener('close', () => {
      setStatus('reconnecting…');
      retry = Math.min(retry + 1, 6);
      setTimeout(connect, 250 * retry);
    });
    socket.addEventListener('error', () => socket.close());
  }

  // -- countdown ------------------------------------------------------------
  const expires = new Date(body.dataset.expires);
  function tick() {
    const ms = expires - Date.now();
    if (ms <= 0) { ttlEl.textContent = 'expired'; return; }
    const minutes = Math.floor(ms / 60000);
    const hours = Math.floor(minutes / 60);
    ttlEl.textContent = hours > 0 ? hours + 'h ' + (minutes % 60) + 'm' : minutes + 'm';
  }
  tick();
  setInterval(tick, 30000);

  // -- "how to send" panel --------------------------------------------------
  function hostPort(addr) {
    const i = String(addr).lastIndexOf(':');
    if (i < 0) return { host: addr, port: '' };
    return { host: addr.slice(0, i), port: addr.slice(i + 1) };
  }

  function renderCommands() {
    const originHost = hostPort(ORIGIN.replace(/^https?:\/\//, ''));
    const tcp = hostPort(TCP);
    const udp = hostPort(UDP);

    const commands = {
      http: `curl -sS -X POST '${ORIGIN}/d/${ID}/anything' \\\n  -H 'Content-Type: application/json' \\\n  -d '{"hello":"dumpster"}'`,
      sse: `curl -N '${ORIGIN}/d/${ID}/sse?count=5&interval_ms=1000'`,
      grpc: `grpcurl -plaintext -import-path examples -proto dumpster.proto \\\n  -H 'x-dumpster-id: ${ID}' \\\n  -d '{"payload":"aGVsbG8="}' ${GRPC} dumpster.Ingest/Anything`,
      ws: `websocat "ws://${originHost.host}:${originHost.port}/d/${ID}/ws"`,
      tcp: `printf '%s\\nhello\\n' '${ID}' | nc ${tcp.host} ${tcp.port}`,
      udp: `printf '%s|hello' '${ID}' | nc -u -w1 ${udp.host} ${udp.port}`,
    };

    for (const [key, value] of Object.entries(commands)) {
      const pre = document.querySelector('[data-cmd="' + key + '"]');
      if (pre) pre.textContent = value;
    }
  }

  function wireCopy() {
    for (const pre of document.querySelectorAll('pre.cmd')) {
      pre.title = 'click to copy';
      pre.addEventListener('click', async () => {
        try {
          await navigator.clipboard.writeText(pre.textContent);
          pre.classList.add('copied');
          setTimeout(() => pre.classList.remove('copied'), 600);
        } catch {
          // clipboard needs a secure context; the text is selectable regardless
        }
      });
    }
  }

  function wireSSE() {
    const button = document.getElementById('btn-sse');
    if (!button) return;
    let source = null;
    button.addEventListener('click', () => {
      if (source) {
        source.close();
        source = null;
        button.textContent = 'Trigger SSE';
        return;
      }
      source = new EventSource(ORIGIN + '/d/' + ID + '/sse?count=5&interval_ms=1000');
      button.textContent = 'Stop SSE';
      source.addEventListener('error', () => {
        // The stream ends after `count` events and EventSource would reconnect
        // forever, minting a new dump row every second. Close it instead.
        source.close();
        source = null;
        button.textContent = 'Trigger SSE';
      });
    });
  }

  renderCommands();
  wireCopy();
  wireSSE();
  connect();
})();
```

**Step 4: Run to verify pass**

```bash
./scripts/dev.sh go test ./internal/app -count=1 -run TestStatic
```

Expected: two `--- PASS` lines (`TestStaticServesStylesheet`, `TestStaticServesAppJS`).

**Step 5: Look at it with your own eyes** (this is the only step a test cannot do)

```bash
./scripts/dev.sh go run ./cmd/dumpster
```

then open `http://localhost:8080` on the Mac, click **Rent a dumpster**, and in a second terminal:

```bash
./scripts/dev.sh sh 'curl -s "http://127.0.0.1:8080/d/'"$(curl -s -o /dev/null -w '%{redirect_url}' -X POST http://127.0.0.1:8080/rent)"'/ping" >/dev/null'
```

Simpler and more reliable: on the debug page, click one of the copy-paste commands and paste it into a terminal on the Mac. A row must appear **without a refresh**. Check: badge colour per protocol, click expands details, `Traffic` counter increments, TTL countdown ticks down.

**Step 6: Commit**

```bash
git add internal/web internal/app/ingest_http_test.go   # the test lives in internal/app
git commit -m "feat(web): live record list, detail panes and copy-paste commands"
```

---

## 8. Phase 4 — Server-Sent Events

**Phase gate:** `./scripts/dev.sh go test ./internal/app -count=1 -run TestSSE` prints `ok`.

---

### Task 25 — `internal/app/ingest_sse.go`

**Objective:** A `GET` that streams N events back and records the connection plus every event.

**Files:**
- Create: `internal/app/ingest_sse.go`
- Modify: `internal/app/routes.go` (reserve `/sse`)
- Test: `internal/app/ingest_sse_test.go`

**Step 1: Write the failing test**

```go
package app

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/adam/dumpster/internal/model"
)

func TestSSEStreamsAndRecordsEvents(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	res, err := http.Get(a.BaseURL() + "/d/" + id + "/sse?count=2&interval_ms=1")
	if err != nil {
		t.Fatalf("GET sse: %v", err)
	}
	defer res.Body.Close()

	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	payload, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if got := strings.Count(string(payload), "event: dump"); got != 2 {
		t.Fatalf("events = %d, want 2\n%s", got, payload)
	}
	if got := strings.Count(string(payload), "data: {"); got != 2 {
		t.Fatalf("data lines = %d, want 2\n%s", got, payload)
	}

	recs := records(t, a, id)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Kind != model.KindSSE {
		t.Fatalf("kind = %q, want sse", rec.Kind)
	}
	if len(rec.Events) != 2 {
		t.Fatalf("recorded events = %d, want 2", len(rec.Events))
	}
	if !rec.Closed || rec.CloseNote != "stream finished" {
		t.Fatalf("closed = %v note = %q", rec.Closed, rec.CloseNote)
	}
}

func TestSSECustomEventNameAndClamping(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	res, err := http.Get(a.BaseURL() + "/d/" + id + "/sse?count=0&interval_ms=0&event=tick")
	if err != nil {
		t.Fatalf("GET sse: %v", err)
	}
	defer res.Body.Close()
	payload, _ := io.ReadAll(res.Body)
	if got := strings.Count(string(payload), "event: tick"); got != 1 {
		t.Fatalf("count=0 should clamp to 1 tick, got %d\n%s", got, payload)
	}
}
```

**Step 2: Run it, expect failure**

```bash
./scripts/dev.sh go test ./internal/app -count=1 -run TestSSE
```

Expected: the response is a 200 from the HTTP ingester with a JSON body, so `content-type = "application/json..."` fails.

**Step 3: Write `internal/app/ingest_sse.go`**

```go
package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/adam/dumpster/internal/model"
)

// handleSSEIngest streams `count` server-sent events back to the caller and
// records the connection plus each event it emitted, so the dumpster shows the
// stream the way the client saw it.
func (a *App) handleSSEIngest(w http.ResponseWriter, r *http.Request, sid string) {
	start := time.Now()
	d, ok := a.mustDumpster(w, sid)
	if !ok {
		return
	}

	count := clampInt(intQuery(r, "count", 5), 1, 100)
	interval := time.Duration(clampInt(intQuery(r, "interval_ms", 1000), 0, 60000)) * time.Millisecond
	eventName := r.URL.Query().Get("event")
	if eventName == "" {
		eventName = "dump"
	}

	rec := &model.Record{
		Kind:       model.KindSSE,
		ReceivedAt: start,
		Remote:     remoteFrom(r, a.cfg.TrustProxy),
		Req:        requestEnvelope(r, "/sse"),
		Timing:     timingFrom(r, start),
	}
	stored, err := a.store.Add(d.ID, rec)
	if err != nil {
		http.Error(w, "could not record", http.StatusInternalServerError)
		return
	}
	a.publishNew(d.ID, &stored)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, canFlush := w.(http.Flusher)
	if canFlush {
		flusher.Flush()
	}

	for i := 1; i <= count; i++ {
		payload, err := json.Marshal(map[string]any{
			"dumpster": d.ID,
			"seq":      stored.Seq,
			"event":    i,
			"of":       count,
			"at":       time.Now().UTC().Format(time.RFC3339Nano),
		})
		if err != nil {
			a.closeRecord(d.ID, stored.Seq, "marshal failed")
			return
		}
		if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", i, eventName, payload); err != nil {
			a.closeRecord(d.ID, stored.Seq, "client write failed")
			return
		}
		if canFlush {
			flusher.Flush()
		}
		if updated, err := a.store.AppendEvent(d.ID, stored.Seq, model.Event{
			At: time.Now(), Dir: "out", Type: "event", Bytes: len(payload), Text: string(payload),
		}); err == nil {
			a.publishUpdate(d.ID, &updated)
		}

		if i == count {
			break
		}
		select {
		case <-r.Context().Done():
			a.closeRecord(d.ID, stored.Seq, "client disconnected")
			return
		case <-time.After(interval):
		}
	}
	a.closeRecord(d.ID, stored.Seq, "stream finished")
}

// intQuery reads an integer query parameter, falling back to def.
func intQuery(r *http.Request, key string, def int) int {
	if v := r.URL.Query().Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// clampInt keeps a caller-supplied number inside sane bounds.
func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// closeRecord marks a live record finished and tells every open dashboard.
func (a *App) closeRecord(sid string, seq uint64, note string) {
	if updated, err := a.store.Close(sid, seq, note); err == nil {
		a.publishUpdate(sid, &updated)
	}
}
```

**Step 4: Reserve the path** — in `internal/app/routes.go`'s `handleCatchAll`, add above the `/_live` case:

```go
	if sub == "/sse" {
		a.handleSSEIngest(w, r, sid)
		return
	}
```

**Step 5: Run to verify pass**

```bash
./scripts/dev.sh go test ./internal/app -count=1 -run TestSSE -v
```

Expected: two `--- PASS` lines.

**Step 6: Commit**

```bash
git add internal/app
git commit -m "feat(app): stream and record server-sent events"
```

---

## 9. Phase 5 — WebSocket ingest

**Phase gate:** `./scripts/dev.sh go test ./internal/app -count=1 -run TestWS` prints `ok`.

---

### Task 26 — `internal/app/ingest_ws.go`

**Objective:** Any WebSocket client connects, every frame is recorded, every frame is echoed back.

**Files:**
- Create: `internal/app/ingest_ws.go`
- Modify: `internal/app/routes.go` (reserve `/ws`)
- Test: `internal/app/ingest_ws_test.go`

**Step 1: Write the failing test**

```go
package app

import (
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/adam/dumpster/internal/model"
)

func TestWSIngestEchoesAndRecordsFrames(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, wsURL(a, "/d/"+id+"/ws"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := c.Write(ctx, websocket.MessageText, []byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if typ != websocket.MessageText || string(data) != "hello" {
		t.Fatalf("echo = %v %q", typ, data)
	}
	if err := c.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatalf("close: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		recs := records(t, a, id)
		if len(recs) == 1 && recs[0].Closed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	recs := records(t, a, id)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Kind != model.KindWS {
		t.Fatalf("kind = %q, want ws", rec.Kind)
	}
	if len(rec.Events) != 1 || rec.Events[0].Text != "hello" {
		t.Fatalf("events = %+v, want one 'hello'", rec.Events)
	}
	if !rec.Closed {
		t.Fatal("record should be closed once the socket closes")
	}
}

func TestWSIngestBinaryFrame(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL(a, "/d/"+id+"/ws"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := c.Write(ctx, websocket.MessageBinary, []byte{0x00, 0xff}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := c.Read(ctx); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	c.CloseNow()

	deadline := time.Now().Add(2 * time.Second)
	var rec model.Record
	for time.Now().Before(deadline) {
		recs := records(t, a, id)
		if len(recs) == 1 && len(recs[0].Events) == 1 {
			rec = recs[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(rec.Events) != 1 || rec.Events[0].Base64 == "" {
		t.Fatalf("binary frame should be base64, got %+v", rec.Events)
	}
	if rec.Events[0].Type != "binary" {
		t.Fatalf("event type = %q, want binary", rec.Events[0].Type)
	}
}
```

**Step 2: Run it, expect failure**

```bash
./scripts/dev.sh go test ./internal/app -count=1 -run TestWS
```

Expected: `dial: ... 404` (the HTTP ingester answers the upgrade request).

**Step 3: Write `internal/app/ingest_ws.go`**

```go
package app

import (
	"context"
	"encoding/base64"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/adam/dumpster/internal/model"
)

// handleWSIngest accepts any WebSocket client, records every frame it receives,
// and echoes each frame back so the caller can see the socket is alive.
func (a *App) handleWSIngest(w http.ResponseWriter, r *http.Request, sid string) {
	d, ok := a.mustDumpster(w, sid)
	if !ok {
		return
	}

	// InsecureSkipVerify disables the origin check: this endpoint exists to be
	// hit by arbitrary clients, including browsers on other origins.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(int64(a.cfg.MaxBody))

	start := time.Now()
	rec := &model.Record{
		Kind:       model.KindWS,
		ReceivedAt: start,
		Remote:     remoteFrom(r, a.cfg.TrustProxy),
		Req:        requestEnvelope(r, "/ws"),
		Timing:     timingFrom(r, start),
		Meta:       map[string]string{"subprotocol": c.Subprotocol()},
	}
	stored, err := a.store.Add(d.ID, rec)
	if err != nil {
		_ = c.Close(websocket.StatusInternalError, "could not record")
		return
	}
	a.publishNew(d.ID, &stored)

	ctx := r.Context()
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			a.closeRecord(d.ID, stored.Seq, closeNote(err))
			return
		}
		ev := model.Event{At: time.Now(), Dir: "in", Type: frameType(typ), Bytes: len(data)}
		if utf8.Valid(data) {
			ev.Text = truncateText(string(data), maxInlineText)
		} else {
			ev.Base64 = base64.StdEncoding.EncodeToString(data)
		}
		if updated, err := a.store.AppendEvent(d.ID, stored.Seq, ev); err == nil {
			a.publishUpdate(d.ID, &updated)
		}

		if err := c.Write(ctx, typ, data); err != nil {
			a.closeRecord(d.ID, stored.Seq, "echo failed")
			return
		}
	}
}

// closeNote turns a websocket read error into something a human can read.
func closeNote(err error) string {
	status := websocket.CloseStatus(err)
	switch {
	case status == websocket.StatusNormalClosure:
		return "closed normally"
	case status == websocket.StatusGoingAway:
		return "client went away"
	case status != -1:
		return "closed: " + status.String()
	default:
		return "read error: " + err.Error()
	}
}

// frameType names a WebSocket message type the way the UI shows it.
func frameType(t websocket.MessageType) string {
	if t == websocket.MessageText {
		return "text"
	}
	return "binary"
}

// truncateText caps a string at max bytes without splitting a rune.
func truncateText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for i := max; i > 0 && i > max-4; i-- {
		if utf8.RuneStart(s[i]) {
			return s[:i]
		}
	}
	return s[:max]
}
```

Imports for this file are exactly:

```go
import (
	"encoding/base64"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/adam/dumpster/internal/model"
)
```

**Step 4: Reserve the path** — in `internal/app/routes.go`'s `handleCatchAll`, add:

```go
	if sub == "/ws" {
		a.handleWSIngest(w, r, sid)
		return
	}
```

**Step 5: Run to verify pass**

```bash
./scripts/dev.sh go test ./internal/app -count=1 -run TestWS -v
```

Expected: two `--- PASS` lines.

**Step 6: Commit**

```bash
git add internal/app
git commit -m "feat(app): record and echo websocket frames"
```

---

## 10. Phase 6 — gRPC

**Phase gate:** `./scripts/dev.sh go test ./internal/app -count=1 -run TestGRPC` prints `ok`, and `./scripts/dev.sh sh 'go run ./cmd/grpcping -addr 127.0.0.1:8081 -id <id> -msg hello'` prints `echo: hello`.

---

### Task 27 — `internal/rawproto`: a passthrough codec

**Objective:** Marshal and unmarshal opaque bytes so gRPC can accept calls for services that do not exist.

**Files:**
- Create: `internal/rawproto/rawproto.go`
- Test: `internal/rawproto/rawproto_test.go`

**Step 1: Write the failing test**

```go
package rawproto

import "testing"

func TestRoundTrip(t *testing.T) {
	in := &Message{Body: []byte{0x00, 0x01, 0xff}}
	raw, err := Codec{}.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	out := &Message{}
	if err := (Codec{}).Unmarshal(raw, out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if string(out.Body) != string(in.Body) {
		t.Fatalf("body = %v, want %v", out.Body, in.Body)
	}
	// Unmarshal must copy: the caller reuses its buffer.
	buf := []byte{1, 2, 3}
	_ = (Codec{}).Unmarshal(buf, out)
	buf[0] = 9
	if out.Body[0] != 1 {
		t.Fatal("Unmarshal aliased the source buffer")
	}
}

func TestMarshalRejectsWrongType(t *testing.T) {
	if _, err := (Codec{}).Marshal("nope"); err == nil {
		t.Fatal("expected an error for a non-Message value")
	}
	if err := (Codec{}).Unmarshal([]byte("x"), &struct{}{}); err == nil {
		t.Fatal("expected an error for a non-Message destination")
	}
}

func TestCodecName(t *testing.T) {
	if got := (Codec{}).Name(); got != "proto" {
		t.Fatalf("Name = %q, want proto", got)
	}
}
```

**Step 2: Run it, expect failure**

```bash
./scripts/dev.sh go test ./internal/rawproto -count=1
```

Expected: `no Go files`.

**Step 3: Write the implementation**

```go
// Package rawproto carries opaque gRPC message bodies.
package rawproto

import (
	"fmt"

	"google.golang.org/grpc/encoding"
)

// Message is an opaque gRPC payload.
type Message struct {
	Body []byte
}

// Codec passes bytes through untouched, which lets the server accept calls for
// services and methods it has never heard of.
type Codec struct{}

// Marshal returns the payload as-is.
func (Codec) Marshal(v any) ([]byte, error) {
	m, ok := v.(*Message)
	if !ok {
		return nil, fmt.Errorf("rawproto: cannot marshal %T", v)
	}
	return m.Body, nil
}

// Unmarshal copies the payload into v.
func (Codec) Unmarshal(data []byte, v any) error {
	m, ok := v.(*Message)
	if !ok {
		return fmt.Errorf("rawproto: cannot unmarshal into %T", v)
	}
	m.Body = append([]byte(nil), data...)
	return nil
}

// Name reports the content-subtype this codec answers for.
func (Codec) Name() string { return "proto" }

var _ encoding.Codec = Codec{}
```

**Step 4: Run to verify pass**

```bash
./scripts/dev.sh go test ./internal/rawproto -count=1
```

Expected: `ok` (3 tests).

**Step 5: Commit**

```bash
git add internal/rawproto
git commit -m "feat(rawproto): passthrough codec for unknown grpc services"
```

---

### Task 28 — `internal/app/ingest_grpc.go` + the `:8081` listener

**Objective:** `grpcurl` (or any client) hits `/d<hex>/Any`, the server records the message and echoes it.

**Files:**
- Create: `internal/app/ingest_grpc.go`
- Modify: `internal/app/app.go` (add the gRPC listener)
- Test: `internal/app/ingest_grpc_test.go`

**Step 1: Write the failing test**

```go
package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/adam/dumpster/internal/model"
	"github.com/adam/dumpster/internal/rawproto"
)

func grpcClient(t *testing.T, a *App) *grpc.ClientConn {
	t.Helper()
	cc, err := grpc.NewClient(a.Addrs().GRPC, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return cc
}

func TestGRPCDumpsRawMessageByPath(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)
	cc := grpcClient(t, a)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	method := "/d" + strings.ReplaceAll(id, "-", "") + "/Anything"
	out := &rawproto.Message{}
	err := cc.Invoke(ctx, method,
		&rawproto.Message{Body: []byte("ping")},
		out,
		grpc.ForceCodec(rawproto.Codec{}),
	)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if string(out.Body) != "ping" {
		t.Fatalf("echo = %q, want ping", out.Body)
	}

	recs := records(t, a, id)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Kind != model.KindGRPC {
		t.Fatalf("kind = %q, want grpc", rec.Kind)
	}
	if len(rec.Events) != 1 || rec.Events[0].Text != "ping" {
		t.Fatalf("events = %+v", rec.Events)
	}
	if !rec.Closed {
		t.Fatal("record should be closed when the RPC returns")
	}
}

func TestGRPCDumpsByMetadataHeader(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)
	cc := grpcClient(t, a)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "x-dumpster-id", id)

	out := &rawproto.Message{}
	err := cc.Invoke(ctx, "/dumpster.Ingest/Anything",
		&rawproto.Message{Body: []byte("hi")}, out, grpc.ForceCodec(rawproto.Codec{}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := len(records(t, a, id)); got != 1 {
		t.Fatalf("records = %d, want 1", got)
	}
}

func TestGRPCUnknownDumpster(t *testing.T) {
	a := newTestApp(t)
	cc := grpcClient(t, a)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := cc.Invoke(ctx, "/d00000000000000700080000000000000/Any",
		&rawproto.Message{Body: []byte("x")}, &rawproto.Message{}, grpc.ForceCodec(rawproto.Codec{}))
	if err == nil {
		t.Fatal("expected NotFound")
	}
	if !strings.Contains(err.Error(), "unknown or expired") {
		t.Fatalf("err = %v", err)
	}
}
```

Add `"google.golang.org/grpc/metadata"` to the imports.

**Step 2: Run it, expect failure**

```bash
./scripts/dev.sh go test ./internal/app -count=1 -run TestGRPC
```

Expected: `a.Addrs().GRPC undefined` — the listener does not exist yet.

**Step 3: Write `internal/app/ingest_grpc.go`**

```go
package app

import (
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/adam/dumpster/internal/id"
	"github.com/adam/dumpster/internal/model"
	"github.com/adam/dumpster/internal/rawproto"
)

// newGRPCServer builds a server that accepts any service and method. The forced
// codec means the payload is never parsed as protobuf, so no .proto file is
// needed to dump a call.
func (a *App) newGRPCServer() *grpc.Server {
	return grpc.NewServer(
		grpc.ForceServerCodec(rawproto.Codec{}),
		grpc.MaxRecvMsgSize(a.cfg.MaxBody),
		grpc.MaxSendMsgSize(a.cfg.MaxBody),
		grpc.UnknownServiceHandler(a.handleGRPCStream),
	)
}

// handleGRPCStream records one RPC and echoes its first message back.
func (a *App) handleGRPCStream(_ any, stream grpc.ServerStream) error {
	full, ok := grpc.MethodFromServerStream(stream)
	if !ok {
		return status.Error(codes.Internal, "no method on stream")
	}
	ctx := stream.Context()

	sid, method := parseGRPCMethod(full)
	if sid == "" {
		if md, mdOK := metadata.FromIncomingContext(ctx); mdOK {
			if v := md.Get("x-dumpster-id"); len(v) > 0 {
				sid = v[0]
			}
		}
	}
	if sid == "" {
		return status.Error(codes.InvalidArgument,
			"no dumpster: call /d<uuid-hex>/Method or send x-dumpster-id metadata")
	}
	canonical, err := id.Canonical(sid)
	if err != nil {
		return status.Error(codes.InvalidArgument, "bad dumpster id")
	}
	d, ok := a.store.Get(canonical)
	if !ok {
		return status.Error(codes.NotFound, "unknown or expired dumpster")
	}

	start := time.Now()
	rec := &model.Record{
		Kind:       model.KindGRPC,
		ReceivedAt: start,
		Remote:     remoteFromPeer(ctx),
		Req:        &model.Request{Protocol: "grpc", Path: full, Headers: headerMap(ctx)},
		Meta:       map[string]string{"method": method},
	}
	stored, err := a.store.Add(d.ID, rec)
	if err != nil {
		return status.Error(codes.Internal, "could not record")
	}
	a.publishNew(d.ID, &stored)

	var first []byte
	for {
		var msg rawproto.Message
		err := stream.RecvMsg(&msg)
		if err == io.EOF {
			break
		}
		if err != nil {
			a.closeRecord(d.ID, stored.Seq, "recv: "+err.Error())
			return err
		}
		if first == nil {
			first = msg.Body
		}
		ev := model.Event{At: time.Now(), Dir: "in", Type: "message", Bytes: len(msg.Body)}
		if utf8.Valid(msg.Body) {
			ev.Text = truncateText(string(msg.Body), maxInlineText)
		} else {
			ev.Base64 = base64.StdEncoding.EncodeToString(msg.Body)
		}
		if updated, err := a.store.AppendEvent(d.ID, stored.Seq, ev); err == nil {
			a.publishUpdate(d.ID, &updated)
		}
	}
	a.closeRecord(d.ID, stored.Seq, "rpc finished")

	if first == nil {
		first = []byte{}
	}
	return stream.SendMsg(&rawproto.Message{Body: first})
}

// parseGRPCMethod splits "/d<hex>/Method" into ("<hex>", "Method"). Any other
// service name yields an empty dumpster id, which forces the metadata route.
func parseGRPCMethod(full string) (sid, method string) {
	parts := strings.Split(strings.TrimPrefix(full, "/"), "/")
	if len(parts) != 2 {
		return "", ""
	}
	service := parts[0]
	if len(service) == 33 && (service[0] == 'd' || service[0] == 'D') {
		return service[1:], parts[1]
	}
	return "", parts[1]
}

// headerMap copies incoming gRPC metadata into the record's header map.
func headerMap(ctx context.Context) map[string][]string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil
	}
	out := make(map[string][]string, len(md))
	for k, v := range md {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// remoteFromPeer describes the gRPC caller.
func remoteFromPeer(ctx context.Context) model.Remote {
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return model.Remote{}
	}
	return remoteFromAddr(p.Addr)
}
```

Imports for this file are exactly:

```go
import (
	"context"
	"encoding/base64"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/adam/dumpster/internal/id"
	"github.com/adam/dumpster/internal/model"
	"github.com/adam/dumpster/internal/rawproto"
)
```

**Step 4: Patch `internal/app/app.go`**

Add these imports: `"google.golang.org/grpc"`.

Extend `Addrs`:

```go
// Addrs are the addresses the listeners actually bound to.
type Addrs struct {
	HTTP string
	GRPC string
}
```

Add two fields to the struct, after `httpLis`:

```go
	grpcSrv *grpc.Server
	grpcLis net.Listener
```

In `Start`, after the HTTP listener is bound, bind and serve gRPC — insert this block immediately **before** the `a.httpSrv = &http.Server{` assignment:

```go
	gl, err := net.Listen("tcp", a.cfg.GRPCAddr)
	if err != nil {
		_ = hl.Close()
		return fmt.Errorf("listen grpc: %w", err)
	}
	a.grpcLis = gl
	a.grpcSrv = a.newGRPCServer()
```

and change the address recording to include it:

```go
	a.addrs = Addrs{HTTP: hl.Addr().String(), GRPC: gl.Addr().String()}
```

Then start it next to the HTTP goroutine:

```go
	go func() { _ = a.grpcSrv.Serve(gl) }()
```

Finally, in `Shutdown`, before the HTTP shutdown:

```go
	if a.grpcSrv != nil {
		a.grpcSrv.GracefulStop()
	}
```

**Step 5: Run to verify pass**

```bash
./scripts/dev.sh go test ./internal/app -count=1 -run TestGRPC -v
```

Expected: three `--- PASS` lines.

**Step 6: Commit**

```bash
git add internal/app
git commit -m "feat(app): dump grpc calls through a passthrough codec"
```

---

### Task 29 — `examples/dumpster.proto` and `cmd/grpcping`

**Objective:** A copy-pasteable gRPC call that works with the stock tools, and a verifiable one-shot client.

**Files:**
- Create: `examples/dumpster.proto`
- Create: `cmd/grpcping/main.go`

**Step 1: Write `examples/dumpster.proto`**

```proto
syntax = "proto3";

package dumpster;

// Ingest is a service the server does not implement: it is caught by the
// unknown-service handler and recorded. The shape only exists so grpcurl can
// build a request; any method name works.
service Ingest {
  rpc Anything (Raw) returns (Raw);
}

message Raw {
  bytes payload = 1;
}
```

**Step 2: Write `cmd/grpcping/main.go`**

```go
// Command grpcping sends one message to a dumpster over gRPC using the path
// form /d<uuid-hex>/Anything, and prints the echo. It exists so the smoke test
// and the plan's verification steps need neither grpcurl nor a .proto file.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/adam/dumpster/internal/rawproto"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8081", "gRPC address")
	id := flag.String("id", "", "dumpster uuid")
	msg := flag.String("msg", "hello", "payload to send")
	flag.Parse()

	if *id == "" {
		fmt.Fprintln(os.Stderr, "grpcping: -id is required")
		os.Exit(2)
	}

	cc, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "grpcping: dial: %v\n", err)
		os.Exit(1)
	}
	defer cc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	method := "/d" + strings.ReplaceAll(*id, "-", "") + "/Anything"
	out := &rawproto.Message{}
	err = cc.Invoke(ctx, method,
		&rawproto.Message{Body: []byte(*msg)}, out,
		grpc.ForceCodec(rawproto.Codec{}))
	if err != nil {
		fmt.Fprintf(os.Stderr, "grpcping: invoke: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("echo: %s\n", out.Body)
}
```

**Step 3: Verify it compiles**

```bash
./scripts/dev.sh go build ./...
```

Expected: no output.

**Step 4: Verify the proto is parseable** (only if `grpcurl` exists on the machine you run it on; the sandbox has none)

```bash
./scripts/dev.sh sh 'command -v grpcurl >/dev/null && grpcurl -proto examples/dumpster.proto describe dumpster.Ingest || echo "grpcurl not installed here — skipping"'
```

Expected: either the service description, or the skip line. **Do not** claim the grpcurl command was verified if you only got the skip line.

**Step 5: Commit**

```bash
git add examples cmd/grpcping
git commit -m "feat(grpcping): one-shot grpc client and the proto for grpcurl"
```

---

## 11. Phase 7 — Raw TCP

**Phase gate:** `./scripts/dev.sh go test ./internal/app -count=1 -run TestTCP` prints `ok`.

---

### Task 30 — `internal/app/ingest_tcp.go` + the `:8082` listener

**Objective:** `printf '<id>\nhello\n' | nc host 8082` produces one row with the id line and every chunk received.

**Files:**
- Create: `internal/app/ingest_tcp.go`
- Modify: `internal/app/app.go` (add the TCP listener)
- Test: `internal/app/ingest_tcp_test.go`

**Step 1: Write the failing test**

```go
package app

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/adam/dumpster/internal/model"
)

func TestTCPRecordsIDLineAndChunks(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	conn, err := net.Dial("tcp", a.Addrs().TCP)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if _, err := fmt.Fprintf(conn, "%s\nhello there\n", id); err != nil {
		t.Fatalf("write: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	var rec model.Record
	for time.Now().Before(deadline) {
		recs := records(t, a, id)
		if len(recs) == 1 && len(recs[0].Events) > 0 {
			rec = recs[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rec.Kind != model.KindTCP {
		t.Fatalf("kind = %q, want tcp (records=%d)", rec.Kind, len(records(t, a, id)))
	}
	if rec.Meta["id_line"] != id {
		t.Fatalf("id_line = %q, want %q", rec.Meta["id_line"], id)
	}
	joined := ""
	for _, ev := range rec.Events {
		joined += ev.Text
	}
	if !strings.Contains(joined, "hello there") {
		t.Fatalf("events did not capture the payload: %+v", rec.Events)
	}
}

func TestTCPRejectsUnknownDumpster(t *testing.T) {
	a := newTestApp(t)
	conn, err := net.Dial("tcp", a.Addrs().TCP)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "%s\nhi\n", "00000000-0000-7000-8000-000000000000"); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(line, "unknown") {
		t.Fatalf("reply = %q, want a message about an unknown dumpster", line)
	}
}
```

**Step 2: Run it, expect failure**

```bash
./scripts/dev.sh go test ./internal/app -count=1 -run TestTCP
```

Expected: `a.Addrs().TCP undefined`.

**Step 3: Write `internal/app/ingest_tcp.go`**

```go
package app

import (
	"bufio"
	"encoding/base64"
	"errors"
	"net"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/adam/dumpster/internal/id"
	"github.com/adam/dumpster/internal/model"
)

// acceptTCP runs the accept loop for raw TCP until the listener closes.
func (a *App) acceptTCP(lis net.Listener) {
	for {
		conn, err := lis.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		go a.handleTCPConn(conn)
	}
}

// handleTCPConn reads the dumpster id from the first line, then records every
// chunk that follows. TCP is diagnostics only: nothing is echoed back except
// the error lines below.
func (a *App) handleTCPConn(conn net.Conn) {
	defer conn.Close()

	remote := remoteFromAddr(conn.RemoteAddr())
	start := time.Now()
	_ = conn.SetReadDeadline(time.Now().Add(a.cfg.ConnIdle))

	reader := bufio.NewReaderSize(conn, 4096)
	line, err := readIDLine(reader, 128)
	if err != nil {
		_, _ = conn.Write([]byte("expected the dumpster id on the first line\n"))
		return
	}
	canonical, err := id.Canonical(strings.TrimSpace(line))
	if err != nil {
		_, _ = conn.Write([]byte("invalid dumpster id\n"))
		return
	}
	d, ok := a.store.Get(canonical)
	if !ok {
		_, _ = conn.Write([]byte("unknown or expired dumpster\n"))
		return
	}

	rec := &model.Record{
		Kind:       model.KindTCP,
		ReceivedAt: start,
		Remote:     remote,
		Meta: map[string]string{
			"id_line": strings.TrimSpace(line),
			"local":   conn.LocalAddr().String(),
		},
	}
	stored, err := a.store.Add(d.ID, rec)
	if err != nil {
		return
	}
	a.publishNew(d.ID, &stored)

	buf := make([]byte, 32*1024)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(a.cfg.ConnIdle))
		n, err := reader.Read(buf)
		if n > 0 {
			data := buf[:n]
			ev := model.Event{At: time.Now(), Dir: "in", Type: "chunk", Bytes: n}
			if utf8.Valid(data) {
				ev.Text = truncateText(string(data), maxInlineText)
			} else {
				ev.Base64 = base64.StdEncoding.EncodeToString(data)
			}
			if updated, appendErr := a.store.AppendEvent(d.ID, stored.Seq, ev); appendErr == nil {
				a.publishUpdate(d.ID, &updated)
			}
		}
		if err != nil {
			note := "closed by peer"
			if errors.Is(err, os.ErrDeadlineExceeded) {
				note = "idle timeout"
			}
			a.closeRecord(d.ID, stored.Seq, note)
			return
		}
	}
}

// readIDLine reads until a newline, refusing to buffer an unbounded line from a
// client that never sends one.
func readIDLine(reader *bufio.Reader, max int) (string, error) {
	var sb strings.Builder
	for sb.Len() < max {
		b, err := reader.ReadByte()
		if err != nil {
			return "", err
		}
		if b == '\n' {
			return sb.String(), nil
		}
		sb.WriteByte(b)
	}
	return "", errors.New("id line too long")
}
```

**Step 4: Patch `internal/app/app.go`**

Extend `Addrs`:

```go
type Addrs struct {
	HTTP string
	GRPC string
	TCP  string
}
```

Add one field after `grpcLis`:

```go
	tcpLis net.Listener
```

In `Start`, after the gRPC listener block:

```go
	tl, err := net.Listen("tcp", a.cfg.TCPAddr)
	if err != nil {
		_ = hl.Close()
		_ = gl.Close()
		return fmt.Errorf("listen tcp: %w", err)
	}
	a.tcpLis = tl
```

Include it in the recorded addrs:

```go
	a.addrs = Addrs{HTTP: hl.Addr().String(), GRPC: gl.Addr().String(), TCP: tl.Addr().String()}
```

Start the loop:

```go
	go a.acceptTCP(tl)
```

In `Shutdown`, after the gRPC stop:

```go
	if a.tcpLis != nil {
		_ = a.tcpLis.Close()
	}
```

**Step 5: Run to verify pass**

```bash
./scripts/dev.sh go test ./internal/app -count=1 -run TestTCP -v
```

Expected: two `--- PASS` lines.

**Step 6: Commit**

```bash
git add internal/app
git commit -m "feat(app): dump raw tcp sessions keyed by the first line"
```

---

## 12. Phase 8 — Raw UDP

**Phase gate:** `./scripts/dev.sh go test ./internal/app -count=1 -run TestUDP` prints `ok`.

---

### Task 31 — `internal/app/ingest_udp.go` + the `:8083` socket

**Objective:** `printf '<id>|hello' | nc -u host 8083` produces a row.

**Files:**
- Create: `internal/app/ingest_udp.go`
- Modify: `internal/app/app.go` (add the UDP socket)
- Test: `internal/app/ingest_udp_test.go`

**Step 1: Write the failing test**

```go
package app

import (
	"net"
	"testing"
	"time"

	"github.com/adam/dumpster/internal/model"
)

func TestUDPDatagramBecomesOneRecord(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	conn, err := net.Dial("udp", a.Addrs().UDP)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte(id + "|hello datagram")); err != nil {
		t.Fatalf("write: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	var rec model.Record
	for time.Now().Before(deadline) {
		recs := records(t, a, id)
		if len(recs) == 1 {
			rec = recs[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rec.Kind != model.KindUDP {
		t.Fatalf("kind = %q, want udp", rec.Kind)
	}
	if len(rec.Events) != 1 || rec.Events[0].Text != "hello datagram" {
		t.Fatalf("events = %+v", rec.Events)
	}
}

func TestUDPTwoDatagramsTwoRecords(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	conn, err := net.Dial("udp", a.Addrs().UDP)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	for _, payload := range []string{"one", "two"} {
		if _, err := conn.Write([]byte(id + "|" + payload)); err != nil {
			t.Fatalf("write %s: %v", payload, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(records(t, a, id)) == 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("records = %d, want 2 (one per datagram)", len(records(t, a, id)))
}

func TestUDPIgnoresGarbage(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	conn, err := net.Dial("udp", a.Addrs().UDP)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("too short")); err != nil {
		t.Fatalf("write: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if got := len(records(t, a, id)); got != 0 {
		t.Fatalf("garbage produced %d records", got)
	}
}
```

**Step 2: Run it, expect failure**

```bash
./scripts/dev.sh go test ./internal/app -count=1 -run TestUDP
```

Expected: `a.Addrs().UDP undefined`.

**Step 3: Write `internal/app/ingest_udp.go`**

```go
package app

import (
	"encoding/base64"
	"errors"
	"net"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/adam/dumpster/internal/id"
	"github.com/adam/dumpster/internal/model"
)

// readUDP reads datagrams until the socket closes. Datagrams whose first 36
// bytes are not a dumpster id are silently dropped: UDP has no way to complain.
func (a *App) readUDP(conn net.PacketConn) {
	buf := make([]byte, 65535)
	for {
		n, addr, err := conn.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		a.handleUDPDatagram(buf[:n], addr)
	}
}

// handleUDPDatagram records exactly one datagram as one record.
func (a *App) handleUDPDatagram(packet []byte, addr net.Addr) {
	// A datagram shorter than an id cannot be addressed to anyone.
	if len(packet) < 36 {
		return
	}
	canonical, err := id.Canonical(string(packet[:36]))
	if err != nil {
		return
	}
	d, ok := a.store.Get(canonical)
	if !ok {
		return
	}

	payload := packet[36:]
	if len(payload) > 0 {
		switch payload[0] {
		case '|', '\n', '\r', ' ':
			payload = payload[1:]
		}
	}

	ev := model.Event{At: time.Now(), Dir: "in", Type: "datagram", Bytes: len(payload)}
	if utf8.Valid(payload) {
		ev.Text = truncateText(string(payload), maxInlineText)
	} else {
		ev.Base64 = base64.StdEncoding.EncodeToString(payload)
	}

	rec := &model.Record{
		Kind:       model.KindUDP,
		ReceivedAt: time.Now(),
		Remote:     remoteFromAddr(addr),
		Meta:       map[string]string{"datagram_bytes": strconv.Itoa(len(packet))},
		Events:     []model.Event{ev},
	}
	stored, err := a.store.Add(d.ID, rec)
	if err != nil {
		return
	}
	a.publishNew(d.ID, &stored)
}
```

**Step 4: Patch `internal/app/app.go`**

Extend `Addrs`:

```go
type Addrs struct {
	HTTP string
	GRPC string
	TCP  string
	UDP  string
}
```

Add one field after `tcpLis`:

```go
	udpConn net.PacketConn
```

In `Start`, after the TCP listener block:

```go
	ul, err := net.ListenPacket("udp", a.cfg.UDPAddr)
	if err != nil {
		_ = hl.Close()
		_ = gl.Close()
		_ = tl.Close()
		return fmt.Errorf("listen udp: %w", err)
	}
	a.udpConn = ul
```

Include it in the recorded addrs:

```go
	a.addrs = Addrs{
		HTTP: hl.Addr().String(),
		GRPC: gl.Addr().String(),
		TCP:  tl.Addr().String(),
		UDP:  ul.LocalAddr().String(),
	}
```

Start the loop:

```go
	go a.readUDP(ul)
```

In `Shutdown`, after the TCP close:

```go
	if a.udpConn != nil {
		_ = a.udpConn.Close()
	}
```

**Step 5: Now that the addresses exist, point the debug page at them** — in `internal/app/pages.go`, replace the three public-address arguments so they use the bound ports instead of the configured ones:

```go
		"PublicGRPC": a.publicAddr(a.cfg.PublicGRPC, a.Addrs().GRPC),
		"PublicTCP":  a.publicAddr(a.cfg.PublicTCP, a.Addrs().TCP),
		"PublicUDP":  a.publicAddr(a.cfg.PublicUDP, a.Addrs().UDP),
```

(Task 22 deliberately used the *configured* ports because these fields did not exist yet; swap them now that `Addrs` carries real bound ports, so the page stays correct even when a listener binds `:0` in tests.)

**Step 6: Run to verify pass**

```bash
./scripts/dev.sh go test ./internal/app -count=1 -run TestUDP -v
```

Expected: three `--- PASS` lines.

**Step 7: Full suite**

```bash
./scripts/dev.sh go test ./... -count=1
```

Expected: `ok` for all six packages, no `FAIL`.

**Step 8: Commit**

```bash
git add internal/app
git commit -m "feat(app): dump udp datagrams keyed by the leading uuid"
```

---
## 13. Phase 9 — Limits, shutdown, main

**Phase gate:** `./scripts/dev.sh go test ./... -count=1` is green, and a built binary serves `/healthz` when run directly.

---

### Task 32 — Prove the caps

**Objective:** The abuse controls from decision D8 are enforced, not decorative.

**Files:**
- Test: `internal/app/caps_test.go` (new file, so no other file's test count moves)

**Step 1: Write the failing test**

```go
package app

import (
	"net/http"
	"strings"
	"testing"
)

func TestYardIsFull(t *testing.T) {
	a := newTestApp(t, func(c *Config) { c.MaxDumpsters = 1 })
	rent(t, a)

	res, err := noRedirect.Post(a.BaseURL()+"/rent", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatalf("second rent: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when the yard is full", res.StatusCode)
	}
}

func TestRecordCapTrimsOldest(t *testing.T) {
	a := newTestApp(t, func(c *Config) { c.MaxRecords = 2 })
	id := rent(t, a)

	for i := 0; i < 3; i++ {
		res, err := http.Post(a.BaseURL()+"/d/"+id+"/x", "text/plain", strings.NewReader("hi"))
		if err != nil {
			t.Fatalf("POST %d: %v", i, err)
		}
		res.Body.Close()
	}
	recs := records(t, a, id)
	if len(recs) != 2 {
		t.Fatalf("records = %d, want 2 (MaxRecords)", len(recs))
	}
	if recs[0].Seq != 3 {
		t.Fatalf("newest seq = %d, want 3", recs[0].Seq)
	}
}

func TestBodyCapRejectsNothingButTruncates(t *testing.T) {
	a := newTestApp(t, func(c *Config) { c.MaxBody = 8 })
	id := rent(t, a)

	res, err := http.Post(a.BaseURL()+"/d/"+id+"/x", "text/plain",
		strings.NewReader(strings.Repeat("z", 4096)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("an oversized body must still be accepted, got %d", res.StatusCode)
	}
	rec := records(t, a, id)[0]
	if !rec.Req.Truncated || rec.Req.BodyBytes != 8 {
		t.Fatalf("record = %+v", rec.Req)
	}
}
```

**Step 2: Run it**

```bash
./scripts/dev.sh go test ./internal/app -count=1 -run 'TestYardIsFull|TestRecordCap|TestBodyCap' -v
```

Expected: **all pass immediately** — the caps were built into the store and the ingester in Phases 1–2. This task is the proof, not the feature. If any fails, the store's trim or the `ErrFull` path is wrong; fix `internal/store/store.go`, not the test.

**Step 3: Commit**

```bash
git add internal/app
git commit -m "test(app): prove the yard, record and body caps"
```

---

### Task 33 — `cmd/dumpster/main.go`

**Objective:** Flags over env over defaults, graceful shutdown on SIGINT/SIGTERM.

**Files:**
- Create: `cmd/dumpster/main.go`

**Step 1: Write it**

```go
// Command dumpster runs the multi-protocol request dumpster.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/adam/dumpster/internal/app"
)

func main() {
	cfg := app.DefaultConfig().FromEnv()

	flag.StringVar(&cfg.HTTPAddr, "http", cfg.HTTPAddr, "http listen address (websocket and sse share it)")
	flag.StringVar(&cfg.GRPCAddr, "grpc", cfg.GRPCAddr, "grpc listen address")
	flag.StringVar(&cfg.TCPAddr, "tcp", cfg.TCPAddr, "raw tcp listen address")
	flag.StringVar(&cfg.UDPAddr, "udp", cfg.UDPAddr, "raw udp listen address")
	flag.StringVar(&cfg.BaseURL, "base-url", cfg.BaseURL, "external origin for links, e.g. https://dumpster.example")
	flag.StringVar(&cfg.PublicGRPC, "public-grpc", cfg.PublicGRPC, "address shown to users for grpc")
	flag.StringVar(&cfg.PublicTCP, "public-tcp", cfg.PublicTCP, "address shown to users for tcp")
	flag.StringVar(&cfg.PublicUDP, "public-udp", cfg.PublicUDP, "address shown to users for udp")
	flag.DurationVar(&cfg.TTL, "ttl", cfg.TTL, "how long a dumpster lives")
	flag.IntVar(&cfg.MaxBody, "max-body", cfg.MaxBody, "bytes of body kept per request")
	flag.BoolVar(&cfg.TrustProxy, "trust-proxy", cfg.TrustProxy, "trust X-Forwarded-For for the reported client ip")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	a := app.New(cfg)
	if err := a.Start(); err != nil {
		slog.Error("start failed", "err", err)
		os.Exit(1)
	}

	addrs := a.Addrs()
	slog.Info("dumpster up",
		"http", addrs.HTTP,
		"grpc", addrs.GRPC,
		"tcp", addrs.TCP,
		"udp", addrs.UDP,
		"ttl", cfg.TTL.String(),
		"base", a.BaseURL(),
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	slog.Info("draining")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown", "err", err)
		os.Exit(1)
	}
}
```

**Step 2: Build and run it for real**

```bash
./scripts/dev.sh sh 'set -e
  go build -o /tmp/dumpster ./cmd/dumpster
  /tmp/dumpster -http 127.0.0.1:18080 -grpc 127.0.0.1:18081 -tcp 127.0.0.1:18082 -udp 127.0.0.1:18083 &
  sleep 2
  curl -sS http://127.0.0.1:18080/healthz
  kill -TERM %1
  wait || true'
```

Expected, in order:
- a `dumpster up` log line with all four addresses,
- `{"ok":true}`,
- `draining`, then exit.

**Step 3: Check the flags print**

```bash
./scripts/dev.sh sh 'go build -o /tmp/dumpster ./cmd/dumpster && /tmp/dumpster -h 2>&1 | head -20'
```

Expected: a usage block listing `-http`, `-grpc`, `-tcp`, `-udp`, `-base-url`, `-ttl`, `-max-body`, `-trust-proxy` with their defaults.

**Step 4: Commit**

```bash
git add cmd/dumpster
git commit -m "feat(cmd): run the dumpster with flags, env and graceful shutdown"
```

---

### Task 34 — `Dockerfile`, built and run on a real daemon

**Objective:** A small static image (~30 MB measured: alpine base + a -s -w stripped Go binary), proven by actually building and running it.

**Files:**
- Create: `Dockerfile`
- Create: `.dockerignore`

**Step 1: Write `Dockerfile`**

```dockerfile
FROM golang:1.27.3-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/dumpster ./cmd/dumpster

FROM alpine:3
RUN adduser -D -u 10001 dumpster
USER dumpster
COPY --from=build /out/dumpster /usr/local/bin/dumpster
EXPOSE 8080 8081 8082
EXPOSE 8083/udp
ENTRYPOINT ["/usr/local/bin/dumpster"]
```

**Step 2: Write `.dockerignore`**

```
.git
.hermes
bin
*.test
*.out
```

**Step 3: Build it on Galadriel**

Build it locally — the daemon shares this filesystem, so the build context is the
repo itself:

```bash
docker build -t dumpster:dev .
```

Expected: a long layer log ending with

```
 => exporting to image
 => => naming to docker.io/library/dumpster:dev
```

If the first line fails with `golang:1.27.3-alpine: not found`, the tag is not published yet — change both the `FROM` line and `GO_IMAGE` default to `golang:1.27-alpine` and retry.

**Step 4: Run the image and hit it**

```bash
docker run --rm -d --name dumpster-smoke \
  -p 18080:8080 -p 18081:8081 -p 18082:8082 -p 18083:8083 dumpster:dev
sleep 2
docker exec dumpster-smoke wget -qO- http://127.0.0.1:8080/healthz
```

Expected: `{"ok":true}`.

`docker exec` is deliberate: published ports land on the daemon's host, not
necessarily on this sandbox, so asking the container about itself is the check that
cannot lie about routing.

**Step 5: Rent one from the real image, then clean up**

```bash
docker exec dumpster-smoke wget -qS -O /dev/null --post-data='' http://127.0.0.1:8080/rent 2>&1 | head -5
docker rm -f dumpster-smoke
```

Expected: `HTTP/1.1 303 See Other` and a `Location: /d/<36-char-uuid>` header, then
the container name echoed back by `rm`. The `--post-data=''` matters: only `POST /rent`
is registered, so a bare `GET` correctly answers **404** (the catch-all has no route
for it) — that 404 is not a bug in the image.

**Step 6: Commit**

```bash
git add Dockerfile .dockerignore
git commit -m "build: ship a static image on alpine"
```

---

### Task 35 — `compose.yaml`

**Objective:** A deployable two-line story for Coolify or any Compose host.

**Files:**
- Create: `compose.yaml`

**Step 1: Write it**

```yaml
services:
  dumpster:
    build: .
    image: dumpster:local
    restart: unless-stopped
    ports:
      - "8080:8080"       # http + websocket + sse
      - "8081:8081"       # grpc (h2c)
      - "8082:8082"       # raw tcp
      - "8083:8083/udp"   # raw udp — the /udp suffix is required; a bare port publishes tcp
    environment:
      DUMPSTER_TTL: 12h
      DUMPSTER_MAX_BODY: "1048576"
      DUMPSTER_MAX_DUMPSTERS: "10000"
      # set these when behind a proxy:
      # DUMPSTER_BASE_URL: https://dumpster.example
      # DUMPSTER_TRUST_PROXY: "1"
      # DUMPSTER_PUBLIC_GRPC: dumpster.example:8081
      # DUMPSTER_PUBLIC_TCP: dumpster.example:8082
      # DUMPSTER_PUBLIC_UDP: dumpster.example:8083
    read_only: true
    security_opt:
      - no-new-privileges:true
```

**Step 2: Validate it without deploying**

```bash
docker compose -f compose.yaml config -q && echo COMPOSE_OK
```

Expected: `COMPOSE_OK`.

**Step 3: Commit**

```bash
git add compose.yaml
git commit -m "build: add compose file with proxy-ready env"
```

---

### Task 35b — `compose.dev.yaml`: the local development stack

**Objective:** One command brings up Redis plus the Go toolchain container with the
source bind-mounted, so the whole inner loop is `make dev` then `make run` — and
Redis becomes exercisable locally instead of only in tests.

> **Why lettered:** inserted after Phases 0–9 and Task 35 were written, for the same
> reason as §13A — renumbering would break every `Task N` cross-reference.

**Files:**
- Create: `compose.dev.yaml`
- Modify: `scripts/dev.sh` (make `up` compose-aware, add `down`)
- Modify: `Makefile` (add `dev` and `down`)

**Step 1: Write `compose.dev.yaml`**

```yaml
# The development stack. `docker compose up` here is NOT how you run the app for
# real — that is ./compose.yaml. This stack exists to give you:
#   * Redis on localhost:6380 (for the redis backend, without touching your own)
#   * a `dumpster-dev` container with the source mounted and the Go toolchain warm
#     and the HTTP/gRPC/TCP/UDP ports published, so `go run` is reachable from the Mac
name: dumpster-dev

services:
  redis:
    image: redis:7-alpine
    # No persistence on purpose: local dev wants a clean yard on every restart.
    command: ["redis-server", "--save", "", "--appendonly", "no"]
    ports:
      - "6380:6379"          # 6380 so it cannot fight a redis you already run
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 2s
      timeout: 2s
      retries: 15

  dev:
    image: golang:1.27.3
    container_name: dumpster-dev
    working_dir: /src
    command: ["sleep", "infinity"]
    volumes:
      # SRC_DIR must be the path as the DOCKER DAEMON sees it, not as this shell
      # sees it. scripts/dev.sh sets it per mode; by hand from the host it is:
      #   SRC_DIR=$HOME/Developer/dumpster docker compose -f compose.dev.yaml up -d
      - ${SRC_DIR:-.}:/src
      - go-mod:/go/pkg/mod
      - go-cache:/root/.cache/go-build
    environment:
      CGO_ENABLED: "0"
      GOFLAGS: "-mod=mod"
      # `make run` in this container therefore uses Redis; unset it to use memory.
      DUMPSTER_REDIS_URL: redis://redis:6379/0
      DUMPSTER_BASE_URL: http://localhost:8080
      DUMPSTER_TTL: 1h          # short locally, so expiry is easy to watch
    ports:
      - "8080:8080"       # http + websocket + sse
      - "8081:8081"       # grpc
      - "8082:8082"       # raw tcp
      - "8083:8083/udp"   # raw udp — the /udp suffix is required; a bare port publishes tcp
    depends_on:
      redis:
        condition: service_healthy

volumes:
  go-mod:
  go-cache:
```

**Step 2: Nothing to edit in `scripts/dev.sh`** — it was written compose-aware in
Task 1: `up` prefers `compose.dev.yaml` whenever that file exists, and `down` tears
the stack down (with volumes) before removing the bare container. The service keeps
`container_name: dumpster-dev`, so `dev.sh go …` still works unchanged.

Check it took effect after Step 5; `[dev] dev stack up (compose)` is the tell.

**Step 3: Add the targets** — in `Makefile`:

```make
down:  ; $(DEV) down
```

and change nothing else: `make up` now brings up Redis too, and `make run` picks up
`DUMPSTER_REDIS_URL` from the compose environment automatically.

**Step 4: Validate the file**

```bash
SRC_DIR="$PWD" docker compose -f compose.dev.yaml config -q && echo COMPOSE_OK
```

Expected: `COMPOSE_OK`.

**Step 5: Bring it up and prove both halves**

```bash
./scripts/dev.sh down
./scripts/dev.sh up
./scripts/dev.sh sh 'go version && echo "redis url: $DUMPSTER_REDIS_URL"'
```

Expected, in order:

```
[dev] removed dumpster-dev
[dev] dev stack up (compose)
go version go1.27.3 linux/arm64
redis url: redis://redis:6379/0
```

**Step 6: Prove Redis is actually reachable from the toolchain container**

```bash
./scripts/dev.sh sh 'exec 3<>/dev/tcp/redis/6379 && printf "PING\r\n" >&3 && head -c 7 <&3'
```

Expected: `+PONG` followed by a newline. If you get
`connect: Connection refused`, the `redis` service is not healthy yet — run
`docker compose -f compose.dev.yaml ps` in the same context and read the health column.

**Step 7: Prove the app runs against it, end to end**

```bash
./scripts/dev.sh sh 'set -e
  go build -o /tmp/dumpster ./cmd/dumpster
  /tmp/dumpster > /tmp/app.log 2>&1 &
  sleep 2
  curl -sS http://127.0.0.1:8080/healthz
  kill %1 || true'
```

Expected: `{"ok":true}` — and because `DUMPSTER_REDIS_URL` is in the container
environment, that `ok` now comes from a Redis `PING`, not from the memory backend.
To confirm which backend answered, check the startup log line in `/tmp/app.log`
(`dumpster up ... base=...`); Task 41's `openStore` is the only thing that branches.

**Step 8: Commit**

```bash
git add compose.dev.yaml scripts/dev.sh Makefile
git commit -m "build: add a compose stack for local development"
```

---

### Task 36 — `README.md`

**Objective:** A stranger can run it, and knows the four URL shapes.

**Files:**
- Create: `README.md`

**Step 1: Write it**

````markdown
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
| UDP | `:8083/udp`, first 36 bytes are the uuid, optionally followed by `\|` |

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

# udp  (-u is mandatory; the first 36 bytes are the uuid, then a separator)
printf '%s|hello' "$ID" | nc -u -w1 localhost 8083
# nothing prints — UDP is fire-and-forget. The row appears in the page.

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
````

**Step 2: Verify the file is valid markdown with balanced fences**

```bash
grep -c '^```' README.md
```

Expected: an **even** number — `6`, because the file above has three fenced blocks (the outer ````markdown` fence and two inner ones).

**Step 3: Commit**

```bash
git add README.md
git commit -m "docs: explain the four listeners, limits and configuration"
```

---

### Task 37 — `scripts/smoke.sh`, all six protocols end to end

**Objective:** One command proves the whole thing works against a running server.

**Files:**
- Create: `scripts/smoke.sh`

**Step 1: Write it**

```bash
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
```

**Step 2: Make it executable**

```bash
chmod +x scripts/smoke.sh
```

**Step 3: Run it against a live server, inside the container**

```bash
./scripts/dev.sh sh 'set -e
  go build -o /tmp/dumpster ./cmd/dumpster
  /tmp/dumpster > /tmp/dumpster.log 2>&1 &
  sleep 2
  bash scripts/smoke.sh
  kill %1 || true'
```

Expected: the `== ...` banners, then `recorded: http sse tcp udp grpc`, then `OK`. If `/dev/tcp` reports `No such file or directory`, the shell is not bash — the `dev.sh sh` wrapper uses bash, so run the script through it and not through `sh` directly.

**Step 4: Commit**

```bash
git add scripts/smoke.sh
git commit -m "test(smoke): exercise all six protocols end to end"
```

---

### Task 38 — `cmd/loadgen`: prove "highly concurrent"

**Objective:** Numbers, not adjectives.

**Files:**
- Create: `cmd/loadgen/main.go`

**Step 1: Write it**

```go
// Command loadgen posts to one dumpster from many goroutines and reports
// throughput and latency, so "highly concurrent" is a measurement.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	base := flag.String("url", "http://127.0.0.1:8080", "base url")
	id := flag.String("id", "", "dumpster uuid")
	total := flag.Int("n", 2000, "requests to send")
	conc := flag.Int("c", 100, "concurrent workers")
	size := flag.Int("size", 512, "body size in bytes")
	flag.Parse()

	if *id == "" {
		fmt.Fprintln(os.Stderr, "loadgen: -id is required")
		os.Exit(2)
	}

	target := *base + "/d/" + *id + "/load"
	payload := bytes.Repeat([]byte("x"), *size)
	client := &http.Client{
		Transport: &http.Transport{MaxIdleConnsPerHost: *conc, MaxConnsPerHost: *conc},
		Timeout:   10 * time.Second,
	}

	var ok, failed int64
	var mu sync.Mutex
	latencies := make([]time.Duration, 0, *total)

	jobs := make(chan struct{})
	var wg sync.WaitGroup
	for worker := 0; worker < *conc; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range jobs {
				started := time.Now()
				res, err := client.Post(target, "application/octet-stream", bytes.NewReader(payload))
				if err != nil {
					atomic.AddInt64(&failed, 1)
					continue
				}
				res.Body.Close()
				if res.StatusCode != http.StatusOK {
					atomic.AddInt64(&failed, 1)
					continue
				}
				atomic.AddInt64(&ok, 1)
				mu.Lock()
				latencies = append(latencies, time.Since(started))
				mu.Unlock()
			}
		}()
	}

	started := time.Now()
	for i := 0; i < *total; i++ {
		jobs <- struct{}{}
	}
	close(jobs)
	wg.Wait()
	elapsed := time.Since(started)

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	pct := func(q float64) time.Duration {
		if len(latencies) == 0 {
			return 0
		}
		return latencies[int(float64(len(latencies)-1)*q)]
	}

	fmt.Printf("%d ok, %d failed in %s\n", ok, failed, elapsed.Round(time.Millisecond))
	fmt.Printf("%.0f req/s  p50=%s  p95=%s  p99=%s\n",
		float64(ok)/elapsed.Seconds(),
		pct(0.50).Round(time.Microsecond),
		pct(0.95).Round(time.Microsecond),
		pct(0.99).Round(time.Microsecond),
	)
	if failed > 0 {
		os.Exit(1)
	}
}
```

**Step 2: Run it and record the real numbers**

```bash
./scripts/dev.sh sh 'set -e
  go build -o /tmp/dumpster ./cmd/dumpster
  /tmp/dumpster > /tmp/dumpster.log 2>&1 &
  sleep 2
  id=$(curl -sS -o /dev/null -w "%{redirect_url}" -X POST http://127.0.0.1:8080/rent | sed "s#.*/d/##")
  go run ./cmd/loadgen -id "$id" -n 2000 -c 100
  kill %1 || true'
```

Expected shape:

```
2000 ok, 0 failed in < 2 s
<1000+" req/s  p50=<1ms  p95=<5ms  p99=<20ms
```

**Quote whatever you actually measured** in your final report. Do not repeat a
number from this plan — the plan contains none, deliberately. If `failed` is
non-zero, the server is dropping connections: check `go build` warnings, the
container's memory limit (`TERMINAL_CONTAINER_MEMORY` is 5 GiB), and whether
`MaxBody` is smaller than `-size`.

**Step 3: Note the cap interaction**

The 2 001st record is not stored: `MaxRecords` is 500. Re-running `_records.json`
afterwards returning exactly 500 rows is **correct**, not a bug.

**Step 4: Commit**

```bash
git add cmd/loadgen
git commit -m "test(loadgen): measure ingest throughput and latency"
```

---

## 13A. Phase 10 — Redis backend (decision D2, revised)

> **Why this section is lettered:** it was added after Phases 0–9 were written and
> accepted. Numbering it `13A` keeps every existing `§` cross-reference and every
> "Task N" pointer in the earlier phases valid, which renumbering would silently
> break.

**What changes:** `App.store` becomes an interface with two implementations — the
in-memory one you already built, and a Redis one. **Nothing in Phases 0–9 changes
behaviour**, because the memory implementation keeps exactly the semantics it had.

**What Redis actually buys — decide with your eyes open:**

| Motivation | Is Redis the right tool? |
| --- | --- |
| "Don't lose the yard when the container redeploys" | Only with `appendonly yes`. A default Redis is a *memory* store that writes an RDB snapshot on its own terms; treat it as durable only after you have set AOF and mounted `/data`. |
| "Run two or more app instances behind a load balancer" | **Yes, and this is the real win.** `LPUSH`/`LTRIM` → newest-first capped list. `INCR` → the per-dumpster `Seq`. `EXPIREAT` → the 12 h deletion with no reaper. `PUBLISH`/`PSUBSCRIBE` → dashboard fan-out across instances. |
| "Make the store faster" | **No.** Every ingest becomes 2–4 network round trips instead of a mutex. Expect p50 to go from ~50 µs to a few hundred. |

**The honesty clause:** a networked backend can fail, but `Get`/`List` in the
`Backend` interface return `(value, bool)` with no error channel. That is
deliberate — threading `context.Context` and `error` through fifteen already-written
handlers would rewrite the whole plan for a debug tool. The cost is that a Redis
outage makes dumpsters read as "gone". The mitigation is `Backend.Health()`, which
`/healthz` now reports: **check `/healthz` first when a dumpster looks empty.**

**Phase gate:** `./scripts/dev.sh go test ./internal/redisstore ./internal/hub ./internal/app -count=1` is green with no Redis server anywhere on the machine.

---

### Task 39 — `store.Backend`: the interface the app talks to

**Objective:** Name the eight operations + two lifecycle hooks, with the memory store as the first implementation.

**Files:**
- Create: `internal/store/backend.go`
- Modify: `internal/store/store.go` (add `Health` and `Shutdown`)
- Modify: `internal/app/app.go` (field type only)
- Test: `internal/store/backend_test.go`

**Step 1: Write the failing test**

```go
package store

import (
	"testing"
	"time"

	"github.com/adam/dumpster/internal/model"
)

func TestMemoryImplementsBackend(t *testing.T) {
	var b Backend = New(Limits{MaxRecords: 2}, time.Minute)

	d, err := b.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got, ok := b.Get(d.ID); !ok || got.ID != d.ID {
		t.Fatalf("Get = %+v, %v", got, ok)
	}
	rec, err := b.Add(d.ID, &model.Record{Kind: model.KindHTTP})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := b.AppendEvent(d.ID, rec.Seq, model.Event{Type: "frame"}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if _, err := b.Close(d.ID, rec.Seq, "done"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	recs, ok := b.List(d.ID)
	if !ok || len(recs) != 1 {
		t.Fatalf("List = %d records, %v", len(recs), ok)
	}
	if b.Len() != 1 {
		t.Fatalf("Len = %d, want 1", b.Len())
	}
	if err := b.Health(); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if err := b.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}
```

**Step 2: Run it, expect failure**

```bash
./scripts/dev.sh go test ./internal/store -count=1
```

Expected: `undefined: Backend`.

**Step 3: Write `internal/store/backend.go`**

```go
package store

import (
	"time"

	"github.com/adam/dumpster/internal/model"
)

// Backend is every operation the app needs from a dumpster store. The memory
// implementation lives in this package; the Redis one in internal/redisstore.
//
// Get and List report "not found" as false rather than a separate error, because
// every caller treats a backend failure and an expired dumpster the same way: the
// dumpster is not there. Backend.Health is what distinguishes the two, and
// /healthz reports it.
type Backend interface {
	Create() (model.Dumpster, error)
	Get(sid string) (model.Dumpster, bool)
	Add(sid string, rec *model.Record) (model.Record, error)
	AppendEvent(sid string, seq uint64, ev model.Event) (model.Record, error)
	Close(sid string, seq uint64, note string) (model.Record, error)
	List(sid string) ([]model.Record, bool)
	DeleteExpired(now time.Time) []string
	Len() int

	// Health reports whether the backend can serve requests right now.
	Health() error
	// Shutdown releases whatever the backend holds.
	Shutdown() error
}

var _ Backend = (*Store)(nil)
```

**Step 4: Give the memory store its two hooks** — append to `internal/store/store.go`:

```go
// Health always succeeds: the memory store cannot be unreachable.
func (s *Store) Health() error { return nil }

// Shutdown drops every dumpster. Nothing else holds a reference to them.
func (s *Store) Shutdown() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dumps = make(map[string]*entry)
	return nil
}
```

**Step 5: Change the field type in `internal/app/app.go`**

```go
	store store.Backend
```

That is the only line that changes in this task. The field name and the package
name may coincide; Go resolves `store.New(...)` in the value expression, and
`a.store.Add(...)` in method bodies, without ambiguity.

**Step 6: Run to verify pass**

```bash
./scripts/dev.sh go test ./internal/store ./internal/app -count=1
```

Expected: both `ok` — the memory store's seven tests, and every `internal/app` test still green.

**Step 7: Commit**

```bash
git add internal/store internal/app
git commit -m "refactor(store): extract the backend interface"
```

---

### Task 40 — `internal/redisstore`: the same semantics, in Redis

**Objective:** `LPUSH`/`LTRIM` for records, `INCR` for sequence numbers, `EXPIREAT` for the 12 h death.

**Files:**
- Create: `internal/redisstore/redisstore.go`
- Test: `internal/redisstore/redisstore_test.go`
- Modify: `go.mod` (adds `github.com/redis/go-redis/v9` v9.23.0, test-only `github.com/alicebob/miniredis/v2` v2.40.0)

**Step 1: Add the dependencies**

```bash
./scripts/dev.sh go get github.com/redis/go-redis/v9@v9.23.0 github.com/alicebob/miniredis/v2@v2.40.0
```

Expected: no output; `go.mod` now requires both.

**Step 2: Write the failing test**

```go
package redisstore

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/adam/dumpster/internal/model"
	"github.com/adam/dumpster/internal/store"
)

func newBackend(t *testing.T, tweak ...func(*Options)) (*Backend, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	opts := Options{
		URL:          "redis://" + mr.Addr(),
		TTL:          time.Hour,
		MaxRecords:   3,
		MaxEvents:    2,
		MaxDumpsters: 2,
	}
	for _, f := range tweak {
		f(&opts)
	}
	b, err := New(context.Background(), opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = b.Shutdown() })
	return b, mr
}

func TestCreateAndGet(t *testing.T) {
	b, _ := newBackend(t)
	d, err := b.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(d.ID) != 36 {
		t.Fatalf("id = %q", d.ID)
	}
	got, ok := b.Get(d.ID)
	if !ok || got.ID != d.ID {
		t.Fatalf("Get = %+v %v", got, ok)
	}
	if ttl := got.ExpiresAt.Sub(got.CreatedAt); ttl != time.Hour {
		t.Fatalf("ttl = %v, want 1h", ttl)
	}
	if _, ok := b.Get("00000000-0000-7000-8000-000000000000"); ok {
		t.Fatal("unknown id should not resolve")
	}
}

func TestMaxDumpsters(t *testing.T) {
	b, _ := newBackend(t)
	if _, err := b.Create(); err != nil {
		t.Fatalf("Create 1: %v", err)
	}
	if _, err := b.Create(); err != nil {
		t.Fatalf("Create 2: %v", err)
	}
	if _, err := b.Create(); err != store.ErrFull {
		t.Fatalf("Create 3 = %v, want ErrFull", err)
	}
}

func TestAddAssignsSeqTrimsAndListsNewestFirst(t *testing.T) {
	b, _ := newBackend(t)
	d, _ := b.Create()
	for i := 0; i < 5; i++ {
		if _, err := b.Add(d.ID, &model.Record{Kind: model.KindHTTP}); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	recs, ok := b.List(d.ID)
	if !ok {
		t.Fatal("List: not found")
	}
	if len(recs) != 3 {
		t.Fatalf("len = %d, want 3 (MaxRecords)", len(recs))
	}
	if recs[0].Seq != 5 || recs[2].Seq != 3 {
		t.Fatalf("seqs = %d..%d, want 5..3", recs[0].Seq, recs[2].Seq)
	}
}

func TestAppendEventCapsAndCountsDrops(t *testing.T) {
	b, _ := newBackend(t)
	d, _ := b.Create()
	rec, err := b.Add(d.ID, &model.Record{Kind: model.KindTCP})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	for i := 0; i < 4; i++ {
		if _, err := b.AppendEvent(d.ID, rec.Seq, model.Event{Type: "chunk"}); err != nil {
			t.Fatalf("AppendEvent %d: %v", i, err)
		}
	}
	recs, _ := b.List(d.ID)
	if len(recs[0].Events) != 2 {
		t.Fatalf("events = %d, want 2", len(recs[0].Events))
	}
	if recs[0].DroppedEvents != 2 {
		t.Fatalf("dropped = %d, want 2", recs[0].DroppedEvents)
	}
}

func TestCloseMarksRecord(t *testing.T) {
	b, _ := newBackend(t)
	d, _ := b.Create()
	rec, _ := b.Add(d.ID, &model.Record{Kind: model.KindWS})
	closed, err := b.Close(d.ID, rec.Seq, "closed normally")
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !closed.Closed || closed.CloseNote != "closed normally" {
		t.Fatalf("closed = %+v", closed)
	}
}

func TestExpiredKeysHidesAndSweeps(t *testing.T) {
	b, mr := newBackend(t, func(o *Options) { o.TTL = time.Minute })
	d, _ := b.Create()

	// miniredis only advances TTLs on request: fast-forward past the expiry.
	mr.FastForward(2 * time.Minute)

	if _, ok := b.Get(d.ID); ok {
		t.Fatal("expired dumpster is still visible")
	}
	gone := b.DeleteExpired(time.Now())
	if len(gone) != 1 || gone[0] != d.ID {
		t.Fatalf("DeleteExpired = %v, want [%s]", gone, d.ID)
	}
	if b.Len() != 0 {
		t.Fatalf("Len = %d, want 0 after the sweep", b.Len())
	}
}

func TestHealth(t *testing.T) {
	b, mr := newBackend(t)
	if err := b.Health(); err != nil {
		t.Fatalf("Health: %v", err)
	}
	mr.Close()
	if err := b.Health(); err == nil {
		t.Fatal("Health should fail once the server is gone")
	}
}
```

**Step 3: Run it, expect failure**

```bash
./scripts/dev.sh go test ./internal/redisstore -count=1
```

Expected: `no Go files` — the package does not exist yet.

**Step 4: Write `internal/redisstore/redisstore.go`**

```go
// Package redisstore is the durable, multi-instance dumpster backend.
//
// Key layout, all under one prefix:
//
//	dumpster:d:<id>        hash   created_at / expires_at (unix nanos)
//	dumpster:ids           set    every live dumpster id
//	dumpster:seq:<id>      string per-dumpster sequence counter
//	dumpster:r:<id>        list   record sequence numbers, newest first
//	dumpster:rec:<id>:<n>  string the record as JSON
//
// Expiry is Redis's own: every key written for a dumpster gets EXPIREAT at the
// dumpster's absolute expiry, so nothing sweeps records in the hot path.
package redisstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/adam/dumpster/internal/id"
	"github.com/adam/dumpster/internal/model"
	"github.com/adam/dumpster/internal/store"
)

// Options configures a Backend.
type Options struct {
	URL          string // redis://host:port/db
	Prefix       string // defaults to "dumpster:"
	TTL          time.Duration
	MaxRecords   int
	MaxEvents    int
	MaxDumpsters int
}

// Backend speaks the same Backend contract as the memory store.
type Backend struct {
	rdb    *redis.Client
	prefix string
	ttl    time.Duration
	limits store.Limits
	now    func() time.Time
}

var _ store.Backend = (*Backend)(nil)

// New dials Redis and verifies it answers.
func New(ctx context.Context, opts Options) (*Backend, error) {
	if opts.Prefix == "" {
		opts.Prefix = "dumpster:"
	}
	if opts.MaxRecords <= 0 {
		opts.MaxRecords = 500
	}
	if opts.MaxEvents <= 0 {
		opts.MaxEvents = 500
	}
	clientOpts, err := redis.ParseURL(opts.URL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	client := redis.NewClient(clientOpts)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return &Backend{
		rdb:    client,
		prefix: opts.Prefix,
		ttl:    opts.TTL,
		limits: store.Limits{
			MaxRecords:   opts.MaxRecords,
			MaxEvents:    opts.MaxEvents,
			MaxDumpsters: opts.MaxDumpsters,
		},
		now: time.Now,
	}, nil
}

// Client exposes the underlying connection, for the pub/sub relay.
func (b *Backend) Client() *redis.Client { return b.rdb }

// Prefix reports the key prefix in use.
func (b *Backend) Prefix() string { return b.prefix }

func (b *Backend) key(parts ...string) string {
	return b.prefix + strings.Join(parts, ":")
}

// Create mints a dumpster and arms its expiry.
func (b *Backend) Create() (model.Dumpster, error) {
	ctx := context.Background()
	if b.limits.MaxDumpsters > 0 {
		n, err := b.rdb.SCard(ctx, b.key("ids")).Result()
		if err != nil {
			return model.Dumpster{}, err
		}
		if int(n) >= b.limits.MaxDumpsters {
			return model.Dumpster{}, store.ErrFull
		}
	}
	now := b.now()
	for attempt := 0; attempt < 5; attempt++ {
		sid, err := id.New()
		if err != nil {
			return model.Dumpster{}, err
		}
		expires := now.Add(b.ttl)
		pipe := b.rdb.TxPipeline()
		pipe.HSet(ctx, b.key("d", sid),
			"created_at", now.UnixNano(),
			"expires_at", expires.UnixNano())
		pipe.PExpireAt(ctx, b.key("d", sid), expires)
		pipe.SAdd(ctx, b.key("ids"), sid)
		if _, err := pipe.Exec(ctx); err != nil {
			return model.Dumpster{}, err
		}
		return model.Dumpster{ID: sid, CreatedAt: now, ExpiresAt: expires}, nil
	}
	return model.Dumpster{}, errors.New("could not mint a unique dumpster id")
}

// Get returns the dumpster if it exists and has not expired.
func (b *Backend) Get(sid string) (model.Dumpster, bool) {
	vals, err := b.rdb.HGetAll(context.Background(), b.key("d", sid)).Result()
	if err != nil || len(vals) == 0 {
		return model.Dumpster{}, false
	}
	created, errC := strconv.ParseInt(vals["created_at"], 10, 64)
	expires, errE := strconv.ParseInt(vals["expires_at"], 10, 64)
	if errC != nil || errE != nil {
		return model.Dumpster{}, false
	}
	d := model.Dumpster{
		ID:        sid,
		CreatedAt: time.Unix(0, created),
		ExpiresAt: time.Unix(0, expires),
	}
	if !d.ExpiresAt.After(b.now()) {
		return model.Dumpster{}, false
	}
	return d, true
}

// Add assigns the next sequence number, stores the record, and trims the list.
func (b *Backend) Add(sid string, rec *model.Record) (model.Record, error) {
	ctx := context.Background()
	d, ok := b.Get(sid)
	if !ok {
		return model.Record{}, store.ErrNotFound
	}

	seq, err := b.rdb.Incr(ctx, b.key("seq", sid)).Result()
	if err != nil {
		return model.Record{}, err
	}

	stored := *rec
	stored.Seq = uint64(seq)
	if stored.ReceivedAt.IsZero() {
		stored.ReceivedAt = b.now()
	}
	if stored.Events == nil {
		stored.Events = []model.Event{}
	}
	blob, err := json.Marshal(stored)
	if err != nil {
		return model.Record{}, err
	}

	listKey := b.key("r", sid)
	recKey := b.key("rec", sid, strconv.FormatInt(seq, 10))
	pipe := b.rdb.TxPipeline()
	pipe.LPush(ctx, listKey, seq)
	pipe.LTrim(ctx, listKey, 0, int64(b.limits.MaxRecords-1))
	pipe.Set(ctx, recKey, blob, 0)
	pipe.PExpireAt(ctx, listKey, d.ExpiresAt)
	pipe.PExpireAt(ctx, recKey, d.ExpiresAt)
	pipe.PExpireAt(ctx, b.key("seq", sid), d.ExpiresAt)
	if _, err := pipe.Exec(ctx); err != nil {
		return model.Record{}, err
	}
	return stored, nil
}

// AppendEvent rewrites one record with an extra event.
//
// This is a read-modify-write of the record JSON rather than a separate list:
// it keeps the wire protocol identical to the memory backend (one whole record
// per update, which the browser swaps in wholesale) at the cost of re-encoding a
// bounded blob per frame. Records are capped at 500 events / 1 MiB.
func (b *Backend) AppendEvent(sid string, seq uint64, ev model.Event) (model.Record, error) {
	return b.mutate(sid, seq, func(rec *model.Record) {
		if len(rec.Events) >= b.limits.MaxEvents {
			rec.DroppedEvents++
			return
		}
		rec.Events = append(rec.Events, ev)
	})
}

// Close marks a record finished.
func (b *Backend) Close(sid string, seq uint64, note string) (model.Record, error) {
	return b.mutate(sid, seq, func(rec *model.Record) {
		rec.Closed = true
		rec.CloseNote = note
	})
}

func (b *Backend) mutate(sid string, seq uint64, fn func(*model.Record)) (model.Record, error) {
	ctx := context.Background()
	key := b.key("rec", sid, strconv.FormatUint(seq, 10))
	raw, err := b.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return model.Record{}, store.ErrNotFound
	}
	if err != nil {
		return model.Record{}, err
	}
	var rec model.Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return model.Record{}, err
	}
	fn(&rec)
	blob, err := json.Marshal(rec)
	if err != nil {
		return model.Record{}, err
	}
	if err := b.rdb.Set(ctx, key, blob, redis.KeepTTL).Err(); err != nil {
		return model.Record{}, err
	}
	return rec, nil
}

// List returns every record, newest first.
func (b *Backend) List(sid string) ([]model.Record, bool) {
	ctx := context.Background()
	if _, ok := b.Get(sid); !ok {
		return nil, false
	}
	seqs, err := b.rdb.LRange(ctx, b.key("r", sid), 0, -1).Result()
	if err != nil {
		return nil, false
	}
	if len(seqs) == 0 {
		return []model.Record{}, true
	}
	keys := make([]string, len(seqs))
	for i, seq := range seqs {
		keys[i] = b.key("rec", sid, seq)
	}
	blobs, err := b.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, false
	}
	out := make([]model.Record, 0, len(blobs))
	for _, value := range blobs {
		var raw []byte
		switch v := value.(type) {
		case string:
			raw = []byte(v)
		case []byte:
			raw = v
		default:
			continue
		}
		var rec model.Record
		if err := json.Unmarshal(raw, &rec); err == nil {
			out = append(out, rec)
		}
	}
	return out, true
}

// DeleteExpired drops ids whose dumpster hash has already expired, and returns
// them so their subscribers can be closed. Redis does the actual deletion.
func (b *Backend) DeleteExpired(now time.Time) []string {
	ctx := context.Background()
	ids, err := b.rdb.SMembers(ctx, b.key("ids")).Result()
	if err != nil {
		return nil
	}
	var gone []string
	for _, sid := range ids {
		exists, err := b.rdb.Exists(ctx, b.key("d", sid)).Result()
		if err != nil {
			continue
		}
		if exists == 0 {
			_ = b.rdb.SRem(ctx, b.key("ids"), sid).Err()
			gone = append(gone, sid)
		}
	}
	sort.Strings(gone)
	return gone
}

// Len counts dumpsters including any Redis has expired but not yet swept.
func (b *Backend) Len() int {
	n, err := b.rdb.SCard(context.Background(), b.key("ids")).Result()
	if err != nil {
		return 0
	}
	return int(n)
}

// Health reports whether Redis answers.
func (b *Backend) Health() error { return b.rdb.Ping(context.Background()).Err() }

// Shutdown closes the client.
func (b *Backend) Shutdown() error { return b.rdb.Close() }
```

**Step 5: Run to verify pass**

```bash
./scripts/dev.sh go test ./internal/redisstore -count=1 -v
```

Expected: seven `--- PASS` lines (`TestCreateAndGet`, `TestMaxDumpsters`, `TestAddAssignsSeqTrimsAndListsNewestFirst`, `TestAppendEventCapsAndCountsDrops`, `TestCloseMarksRecord`, `TestExpiredKeysHidesAndSweeps`, `TestHealth`), **with no Redis server running** — `miniredis` is an in-process Redis over a real TCP socket.

**Step 6: Commit**

```bash
git add go.mod go.sum internal/redisstore
git commit -m "feat(redisstore): add the redis-backed dumpster store"
```

---

### Task 41 — Wire the backend selection into the app

**Objective:** `-redis`/`DUMPSTER_REDIS_URL` selects Redis; nothing else changes.

**Files:**
- Modify: `internal/app/config.go` (add `RedisURL`, `InstanceID`)
- Modify: `internal/app/app.go` (open the backend in `Start`, report health)
- Modify: `internal/app/routes.go` (`/healthz` gains a backend check)
- Modify: `cmd/dumpster/main.go` (two flags)
- Test: `internal/app/redis_test.go`

**Step 1: Write the failing test**

```go
package app

import (
	"io"
	"net/http"
	"testing"

	"github.com/alicebob/miniredis/v2"
)

func TestRedisBackendServesDumpsters(t *testing.T) {
	mr := miniredis.RunT(t)
	a := newTestApp(t, func(c *Config) { c.RedisURL = "redis://" + mr.Addr() })

	id := rent(t, a)
	res, err := http.Post(a.BaseURL()+"/d/"+id+"/x", "text/plain", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	res.Body.Close()

	if got := len(records(t, a, id)); got != 1 {
		t.Fatalf("records = %d, want 1", got)
	}
	_, body := get(t, a.BaseURL()+"/healthz")
	if body != "{\"ok\":true}\n" {
		t.Fatalf("healthz = %q", body)
	}
}

func TestHealthzReportsADeadBackend(t *testing.T) {
	mr := miniredis.RunT(t)
	a := newTestApp(t, func(c *Config) { c.RedisURL = "redis://" + mr.Addr() })

	res, _ := get(t, a.BaseURL()+"/healthz")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("healthy = %d, want 200", res.StatusCode)
	}

	mr.Close()
	res, body := get(t, a.BaseURL()+"/healthz")
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 once redis is gone", res.StatusCode)
	}
	if body == "" {
		t.Fatal("unhealthy body should explain why")
	}
}

func TestBadRedisURLFailsStart(t *testing.T) {
	cfg := DefaultConfig()
	cfg.HTTPAddr = "127.0.0.1:0"
	cfg.RedisURL = "://not-a-url"
	if err := New(cfg).Start(); err == nil {
		t.Fatal("Start should fail on an unparseable redis url")
	}
}
```

Add `"github.com/alicebob/miniredis/v2"` to the test imports.

**Step 2: Run it, expect failure**

```bash
./scripts/dev.sh go test ./internal/app -count=1 -run 'TestRedis|TestHealthz|TestBadRedis'
```

Expected: `unknown field RedisURL in struct literal`.

**Step 3: Extend `internal/app/config.go`**

Add to the struct, next to `BaseURL`:

```go
	// RedisURL, when set, makes the app use Redis instead of memory. Both
	// backends implement store.Backend, so nothing else changes.
	RedisURL string

	// InstanceID identifies this process in the pub/sub relay, so it can ignore
	// its own messages. Generated per process when empty.
	InstanceID string
```

Add to `FromEnv`:

```go
	if v := os.Getenv("DUMPSTER_REDIS_URL"); v != "" {
		c.RedisURL = v
	}
	if v := os.Getenv("DUMPSTER_INSTANCE_ID"); v != "" {
		c.InstanceID = v
	}
```

**Step 4: Move backend construction from `New` into `Start`** — in `internal/app/app.go`, delete the `store:` line from the `New` struct literal, and add to `Start`, before any listener is bound:

```go
	db, err := a.openStore()
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	a.store = db
```

and append at the end of the file:

```go
// openStore picks the backend. Redis is opt-in, so the default build needs no
// external service at all.
func (a *App) openStore() (store.Backend, error) {
	if a.cfg.RedisURL == "" {
		return store.New(store.Limits{
			MaxRecords:   a.cfg.MaxRecords,
			MaxEvents:    a.cfg.MaxEvents,
			MaxDumpsters: a.cfg.MaxDumpsters,
		}, a.cfg.TTL), nil
	}
	return redisstore.New(a.ctx, redisstore.Options{
		URL:          a.cfg.RedisURL,
		TTL:          a.cfg.TTL,
		MaxRecords:   a.cfg.MaxRecords,
		MaxEvents:    a.cfg.MaxEvents,
		MaxDumpsters: a.cfg.MaxDumpsters,
	})
}
```

Add the import `"github.com/adam/dumpster/internal/redisstore"`, and in `Shutdown`, before the HTTP drain:

```go
	if a.store != nil {
		_ = a.store.Shutdown()
	}
```

**Step 5: Teach `/healthz` to check the backend** — in `internal/app/routes.go`:

```go
func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	if a.store != nil {
		if err := a.store.Health(); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"ok":    false,
				"error": err.Error(),
			})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
```

**Step 6: Add the flags** — in `cmd/dumpster/main.go`, before `flag.Parse()`:

```go
	flag.StringVar(&cfg.RedisURL, "redis", cfg.RedisURL, "redis url; empty uses in-memory storage")
```

**Step 7: Run to verify pass**

```bash
./scripts/dev.sh go test ./internal/app -count=1
```

Expected: `ok`.

**Step 8: Commit**

```bash
git add internal/app cmd/dumpster
git commit -m "feat(app): select the store backend from a redis url"
```

---

### Task 42 — Cross-instance fan-out over Redis Pub/Sub

**Objective:** A browser on instance B sees a request that landed on instance A.

**Files:**
- Modify: `internal/hub/hub.go` (add the `Broker` seam and split `Publish` into marshal + `deliver`)
- Create: `internal/hub/redis.go` (the pub/sub implementation)
- Modify: `internal/app/app.go` (attach the broker in `Start`)
- Test: `internal/hub/redis_test.go`

**Step 1: Write the failing test**

```go
package hub

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func TestRedisBrokerRelaysBetweenHubsButNotToSelf(t *testing.T) {
	mr := miniredis.RunT(t)
	url := "redis://" + mr.Addr()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hubA := New(8)
	hubB := New(8)

	brokerA, err := NewRedisBroker(ctx, url, "test:", "a")
	if err != nil {
		t.Fatalf("broker A: %v", err)
	}
	brokerB, err := NewRedisBroker(ctx, url, "test:", "b")
	if err != nil {
		t.Fatalf("broker B: %v", err)
	}
	hubA.UseBroker(brokerA)
	hubB.UseBroker(brokerB)

	go func() { _ = hubA.RunBroker(ctx) }()
	go func() { _ = hubB.RunBroker(ctx) }()

	chA, cancelA := hubA.Subscribe("d1")
	defer cancelA()
	chB, cancelB := hubB.Subscribe("d1")
	defer cancelB()

	// Give both subscriptions a moment to attach.
	time.Sleep(100 * time.Millisecond)

	hubA.Publish("d1", map[string]string{"type": "new"})

	// B must receive the relayed message.
	select {
	case got := <-chB:
		if len(got) == 0 {
			t.Fatal("B received an empty payload")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("B never received the relayed message")
	}

	// A must receive exactly one copy: its local fan-out, not the echo.
	select {
	case <-chA:
	case <-time.After(2 * time.Second):
		t.Fatal("A never received its own local message")
	}
	select {
	case <-chA:
		t.Fatal("A received its own message twice (echo not filtered)")
	case <-time.After(300 * time.Millisecond):
	}
}
```

**Step 2: Run it, expect failure**

```bash
./scripts/dev.sh go test ./internal/hub -count=1
```

Expected: `undefined: NewRedisBroker`.

**Step 3: Add the seam to `internal/hub/hub.go`**

Replace the current `Publish` with these three pieces (keep `Subscribe`, `Close`, `New` untouched):

```go
// Broker carries published messages to other instances.
type Broker interface {
	// Publish sends an already-marshalled payload for one dumpster.
	Publish(ctx context.Context, dumpsterID string, payload []byte) error
	// Run delivers remote payloads to handle until ctx is cancelled.
	Run(ctx context.Context, handle func(dumpsterID string, payload []byte)) error
}

// UseBroker attaches a cross-instance broker. Publishing then goes to the local
// subscribers and, through the broker, to every other instance.
func (h *Hub) UseBroker(b Broker) {
	h.mu.Lock()
	h.broker = b
	h.mu.Unlock()
}

// RunBroker pumps remote messages into the local subscribers.
func (h *Hub) RunBroker(ctx context.Context) error {
	h.mu.Lock()
	b := h.broker
	h.mu.Unlock()
	if b == nil {
		return nil
	}
	return b.Run(ctx, func(dumpsterID string, payload []byte) {
		h.deliver(dumpsterID, payload)
	})
}

// Publish marshals msg once and delivers it locally, then to other instances.
func (h *Hub) Publish(dumpsterID string, msg any) {
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.deliver(dumpsterID, b)

	h.mu.Lock()
	broker := h.broker
	h.mu.Unlock()
	if broker != nil {
		_ = broker.Publish(context.Background(), dumpsterID, b)
	}
}

// deliver fans out to local subscribers only. Never blocks.
func (h *Hub) deliver(dumpsterID string, payload []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[dumpsterID] {
		select {
		case ch <- payload:
		default:
		}
	}
}
```

Add `broker Broker` to the `Hub` struct and `"context"` to its imports.

**Step 4: Write `internal/hub/redis.go`**

```go
package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"
)

// RedisBroker relays hub messages between processes over Redis Pub/Sub.
//
// Each message is wrapped in an envelope carrying the publishing instance id, so
// an instance ignores its own traffic: every subscriber receives every message,
// including the sender, and without the filter a single ingest would render twice.
//
// Pub/Sub is fire-and-forget. That is acceptable here because a browser that
// misses a message is corrected by the next one, and every reconnect starts from
// a full snapshot.
type RedisBroker struct {
	rdb    *redis.Client
	prefix string
	self   string
}

type envelope struct {
	Origin  string          `json:"origin"`
	Payload json.RawMessage `json:"payload"`
}

// NewRedisBroker dials Redis and verifies it answers.
func NewRedisBroker(ctx context.Context, url, prefix, self string) (*RedisBroker, error) {
	if prefix == "" {
		prefix = "dumpster:"
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	client := redis.NewClient(opts)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return &RedisBroker{rdb: client, prefix: prefix, self: self}, nil
}

// Publish sends one payload to the dumpster's channel.
func (b *RedisBroker) Publish(ctx context.Context, dumpsterID string, payload []byte) error {
	body, err := json.Marshal(envelope{Origin: b.self, Payload: payload})
	if err != nil {
		return err
	}
	return b.rdb.Publish(ctx, b.channel(dumpsterID), body).Err()
}

// Run delivers remote payloads until ctx is cancelled.
func (b *RedisBroker) Run(ctx context.Context, handle func(string, []byte)) error {
	sub := b.rdb.PSubscribe(ctx, b.prefix+"msg:*")
	defer func() { _ = sub.Close() }()

	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, open := <-sub.Channel():
			if !open {
				return nil
			}
			var env envelope
			if err := json.Unmarshal([]byte(msg.Payload), &env); err != nil {
				continue
			}
			if env.Origin == b.self {
				continue
			}
			handle(strings.TrimPrefix(msg.Channel, b.prefix+"msg:"), env.Payload)
		}
	}
}

// Close releases the connection.
func (b *RedisBroker) Close() error { return b.rdb.Close() }

func (b *RedisBroker) channel(dumpsterID string) string {
	return b.prefix + "msg:" + dumpsterID
}
```

**Step 5: Attach the broker in `internal/app/app.go`** — in `Start`, right after the listeners are up:

```go
	if a.cfg.RedisURL != "" {
		self := a.cfg.InstanceID
		if self == "" {
			self, _ = id.New()
		}
		broker, err := hub.NewRedisBroker(a.ctx, a.cfg.RedisURL, "dumpster:", self)
		if err != nil {
			return fmt.Errorf("start pubsub relay: %w", err)
		}
		a.hub.UseBroker(broker)
		go func() { _ = a.hub.RunBroker(a.ctx) }()
	}
```

Add the import `"github.com/adam/dumpster/internal/id"` if it is not already there.

**Step 6: Run to verify pass**

```bash
./scripts/dev.sh go test ./internal/hub -count=1 -v
```

Expected: five `--- PASS` lines (the four original hub tests plus the relay).

**Step 7: Commit**

```bash
git add internal/hub internal/app
git commit -m "feat(hub): relay dashboard messages across instances over redis pubsub"
```

---

### Task 43 — Prove two instances, and deploy them

**Objective:** One dumpster, two processes, one live page — plus a compose file that runs it.

**Files:**
- Test: `internal/app/redis_test.go` (append)
- Modify: `compose.yaml`
- Modify: `README.md`

**Step 1: Append the failing test**

```go
func TestSecondInstanceSeesTrafficFromTheFirst(t *testing.T) {
	mr := miniredis.RunT(t)
	url := "redis://" + mr.Addr()

	a := newTestApp(t, func(c *Config) { c.RedisURL = url; c.InstanceID = "a" })
	b := newTestApp(t, func(c *Config) { c.RedisURL = url; c.InstanceID = "b" })

	id := rent(t, a)

	// The dashboard is open on instance B, which never received the ingest.
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL(b, "/d/"+id+"/_live"), nil)
	if err != nil {
		t.Fatalf("dial B: %v", err)
	}
	defer c.CloseNow()

	var snap wsMsg
	if err := wsjson.Read(ctx, c, &snap); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Type != "snapshot" {
		t.Fatalf("first message = %q", snap.Type)
	}

	// Redis Pub/Sub drops messages published before the subscription is
	// established, and B's relay goroutine started inside Start. Let it attach.
	time.Sleep(200 * time.Millisecond)

	// Traffic lands on A.
	res, err := http.Post(a.BaseURL()+"/d/"+id+"/from-a", "text/plain", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("POST A: %v", err)
	}
	res.Body.Close()

	var next wsMsg
	if err := wsjson.Read(ctx, c, &next); err != nil {
		t.Fatalf("B's socket never saw the record: %v", err)
	}
	if next.Type != "new" || next.Record == nil {
		t.Fatalf("got %+v", next)
	}
	if next.Record.Req.BodyText != "hello" {
		t.Fatalf("body = %q", next.Record.Req.BodyText)
	}

	// And B can read the record back from the shared store.
	recs := records(t, b, id)
	if len(recs) != 1 {
		t.Fatalf("B sees %d records, want 1", len(recs))
	}
}
```

Imports needed in `redis_test.go`: `"context"`, `"strings"`, `"time"`, `"github.com/coder/websocket"`, `"github.com/coder/websocket/wsjson"`.

**Step 2: Run it**

```bash
./scripts/dev.sh go test ./internal/app -count=1 -run TestSecondInstance -v
```

Expected: **may pass immediately** — Tasks 40–42 already built everything. If it fails with "never saw the record", the relay is attached *after* the listeners start but the test dialled too early; make sure `UseBroker` and `RunBroker` run inside `Start` before it returns. If it fails with "B sees 0 records", the shared store prefix differs between instances — hardcode `"dumpster:"` in both, as the plan does.

**Step 3: Patch `compose.yaml`** — add the service and the volume:

```yaml
  redis:
    image: redis:7-alpine
    restart: unless-stopped
    # appendonly yes is what makes dumpsters survive a restart. Without it,
    # Redis is a memory store and the yard is empty after every crash.
    command: ["redis-server", "--appendonly", "yes", "--maxmemory", "256mb", "--maxmemory-policy", "allkeys-lru"]
    volumes:
      - redis-data:/data

volumes:
  redis-data:
```

and on the `dumpster` service:

```yaml
    environment:
      DUMPSTER_REDIS_URL: redis://redis:6379/0
    depends_on:
      - redis
```

**Step 4: Validate**

```bash
docker compose -f compose.yaml config -q && echo COMPOSE_OK
```

Expected: `COMPOSE_OK`.

**Step 5: Add a paragraph to `README.md`** under Configuration:

```markdown
### Storage

By default dumpsters live in the process's memory: fast, zero dependencies, and
gone when the process stops. Set `DUMPSTER_REDIS_URL` (or `-redis`) to use Redis
instead, which gives you two things:

- **Survival across restarts** — only with `appendonly yes`; the compose file sets it.
- **Multiple app instances** — the store is shared and dashboard updates are relayed
  over Redis Pub/Sub, so a page on instance B sees traffic that landed on instance A.

`/healthz` returns `503` with the reason when the backend is unreachable. A dead
Redis makes every dumpster read as "gone" — check `/healthz` before you believe it.
```

**Step 6: Commit**

```bash
git add internal/app compose.yaml README.md
git commit -m "feat: share dumpsters across instances and document the redis mode"
```

---

### Task 44 — Final gate

**Objective:** Leave the repository green and honest.

**Step 1: Full suite**

```bash
./scripts/dev.sh go vet ./...
./scripts/dev.sh go build ./...
./scripts/dev.sh go test ./... -count=1
```

Expected: `vet` and `build` print nothing; every package prints `ok` — **ten** lines, for `internal/app`, `internal/decode`, `internal/hub`, `internal/id`, `internal/model`, `internal/ratelimit`, `internal/rawproto`, `internal/redisstore`, `internal/store` and `internal/web`. The three `cmd/` packages print nothing because they have no tests.

**Step 2: Both backends, by hand, against real services**

```bash
./scripts/dev.sh sh 'set -e
  go build -o /tmp/dumpster ./cmd/dumpster

  # memory backend: strip the url the compose stack injected
  env -u DUMPSTER_REDIS_URL /tmp/dumpster \
    -http 127.0.0.1:18090 -grpc 127.0.0.1:18091 -tcp 127.0.0.1:18092 -udp 127.0.0.1:18093 \
    > /tmp/mem.log 2>&1 &
  sleep 2
  printf "memory: "; curl -sS http://127.0.0.1:18090/healthz
  kill %1 || true

  # redis backend: address the compose service by its network name
  DUMPSTER_REDIS_URL=redis://redis:6379/0 /tmp/dumpster \
    -http 127.0.0.1:18094 -grpc 127.0.0.1:18095 -tcp 127.0.0.1:18096 -udp 127.0.0.1:18097 \
    > /tmp/redis.log 2>&1 &
  sleep 2
  printf "redis:  "; curl -sS http://127.0.0.1:18094/healthz
  kill %1 || true'
```

Expected:

```
memory: {"ok":true}
redis:  {"ok":true}
```

Two backends, two port sets, both answering from a real binary — which is only
possible because Task 35b put Redis in the dev stack. If the `redis:` line answers
`503` with a connection error, the service is not up: `./scripts/dev.sh up`, then
re-check with the `/dev/tcp` PING from Task 35b Step 6.

**Step 3: Confirm the tree is clean and every task committed**

```bash
git status --short          # expected: nothing
git log --oneline | wc -l   # expected: one commit per task, 44 or more
```

**Step 4: Final commit if anything was tidied**

```bash
git add -A
git commit -m "chore: final cleanup after the full suite" || echo "nothing to commit"
```

---

## 13B. Post-gate hardening (applied after Task 44)

Two defects surfaced only under repetition, and neither is visible in a single green run.
Both are fixed in the committed tree; this section records them so a rebuild from this
plan produces the same final state instead of rediscovering them.

### 13B.1 The suite was 12% flaky, and the cause was the test harness

`TestHTTPIngestAcceptsEveryMethod`, `TestRecordsJSONNewestFirst` and
`TestRecordCapTrimsOldest` failed intermittently with:

```
app_test.go:32: Shutdown: context deadline exceeded
--- FAIL: TestHTTPIngestAcceptsEveryMethod (3.00s)
```

Measured at 5 failures in 40 full-package runs (12.5%), never reproducible with
`-run`, and present on the tree *before* the Redis work — so it was not introduced by
Tasks 39–43.

Diagnosis, in the order it was actually established (each step killed a theory):

1. A goroutine dump at the moment of the timeout showed **9 goroutines, no handler
   frames** — nothing was actively serving.
2. The dump did show an **idle keep-alive connection** still attached to the server.
3. `newTestApp` shares `http.DefaultClient` across the whole package, and its idle
   connections to each app's ephemeral port outlive that app. `http.Server.Shutdown`
   waits for connections to become idle *and close*, so those connections kept it
   waiting until the 3 s budget expired.

The fix is in the harness, not the product: drop the shared client's idle connections
before draining.

```go
t.Cleanup(func() {
	// http.DefaultClient is shared by every test in this package and keeps
	// idle keep-alive connections to each app's port. Those outlive the
	// server and keep http.Server.Shutdown waiting for them, so drop them
	// before draining — otherwise roughly one run in eight times out.
	http.DefaultClient.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := a.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
})
```

Verified: **0 failures in 60 consecutive full-package runs** (was 5/40).

### 13B.2 `Shutdown` had an unbounded call in its critical path

`grpcSrv.GracefulStop()` has no deadline of its own, and the original `Shutdown` called
it *before* `httpSrv.Shutdown(ctx)` — so a stuck RPC would consume the entire budget and
the HTTP drain would then fail instantly with `context deadline exceeded`, leaving the
TCP and UDP listeners open. That is a production hang, not just a test annoyance.

The committed `Shutdown` drains HTTP first (the only listener whose handlers can be
mid-request and the only one that honours the deadline), disables keep-alives so idle
connections close rather than linger, bounds `GracefulStop` with a hard `Stop` fallback,
and only then closes the raw listeners and cancels the app context.

```go
// Shutdown drains in the order that keeps the ctx budget useful.
//
// HTTP goes first because it is the only listener whose handlers can be
// mid-request, and it is the only one that honours the deadline. Keep-alives are
// disabled first so idle connections are closed rather than held open.
//
// gRPC goes second, and is bounded explicitly: GracefulStop has no deadline of
// its own, so a stuck stream would otherwise hang the shutdown forever. If the
// budget runs out we fall back to a hard Stop.
//
// The remaining listeners are closed last, after which the app context is
// cancelled to retire the reaper and any pub/sub relay.
func (a *App) Shutdown(ctx context.Context) error {
	if a.httpSrv != nil {
		a.httpSrv.SetKeepAlivesEnabled(false)
		if err := a.httpSrv.Shutdown(ctx); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	}
	if a.grpcSrv != nil {
		stopped := make(chan struct{})
		go func() {
			a.grpcSrv.GracefulStop()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-ctx.Done():
			a.grpcSrv.Stop()
			<-stopped
		}
	}
	if a.tcpLis != nil {
		_ = a.tcpLis.Close()
	}
	if a.udpConn != nil {
		_ = a.udpConn.Close()
	}
	if a.store != nil {
		_ = a.store.Shutdown()
	}
	a.cancel()
	return nil
}
```

Both fixes are in `internal/app/` and were committed after Task 44 as
`fix(app): drain http first and bound the grpc graceful stop` and
`fix(app): drop idle client connections before draining in tests`.

**Lesson for the implementer:** a single green run proves nothing about a suite that
is 12% flaky. Run the package at least twenty times before declaring a gate green, and
when a failure names a teardown step, dump the goroutines instead of re-reading the
assertion that "failed" — the failing test was never the broken one.

---

### 13B.3 UDP was not reachable from outside the container

`docker run -p 8083:8083` and a bare compose port line publish **TCP only**. The app
bound `:8083/udp` correctly and every test passed, because tests bind inside the
container and never cross a published port — so a datagram sent from the host was
dropped before it reached the process, and the page simply stayed empty while HTTP,
SSE, WebSocket, gRPC and TCP all worked.

Every publish point now carries the suffix: `dev.sh`'s `PORT_MAP`,
`compose.dev.yaml`, `compose.yaml`, and the `Dockerfile`'s `EXPOSE 8083/udp`.
Verified from outside the container after the fix — a datagram sent to `:8083`
produced `"kind":"udp"`, `"datagram_bytes":"51"`, `"text":"hello over udp"`.

**Lesson:** a listener is not a network path. If a protocol can only be exercised
inside the container, the port mapping is untested — send at least one datagram
from the host side before claiming the protocol works.

Two related fixes landed with it: `down` no longer passes `-v` (that was deleting the
`go-mod`/`go-cache` volumes, so every `up` re-downloaded ~100 MB), and the README now
states that `8083` must be published as `8083:8083/udp` and that UDP prints nothing
because it is fire-and-forget.

---

## 14. Validation summary

Run these in order; each is a real gate, not a formality.

| # | Command | Expected |
| --- | --- | --- |
| 1 | `./scripts/dev.sh mode` | one word: `ssh` (or `socket`) |
| 2 | `./scripts/dev.sh go version` | `go version go1.27.3 linux/arm64` |
| 3 | `./scripts/dev.sh go build ./...` | no output |
| 4 | `./scripts/dev.sh go vet ./...` | no output |
| 5 | `./scripts/dev.sh go test ./... -count=1` | ten `ok` lines, no `fail` |
| 6 | `./scripts/dev.sh sh 'bash scripts/smoke.sh'` (server running) | `recorded: http sse tcp udp grpc`, then `OK` |
| 6b | `./scripts/dev.sh up` then `./scripts/dev.sh sh 'exec 3<>/dev/tcp/redis/6379 && printf "PING\r\n" >&3 && head -c 7 <&3'` | dev stack up; `+PONG` |
| 6c | `./scripts/dev.sh sh 'go test ./internal/redisstore ./internal/hub -count=1'` | two `ok` lines, no Redis server needed (miniredis) |
| 7 | `./scripts/dev.sh sh 'go run ./cmd/loadgen -id <id> -n 2000 -c 100'` | `2000 ok, 0 failed` and a measured req/s |
| 8 | `docker build -t dumpster:dev .` | `naming to docker.io/library/dumpster:dev` |
| 9 | `docker run --rm -d --name dumpster-smoke -p 18080:8080 dumpster:dev && docker exec dumpster-smoke wget -qO- http://127.0.0.1:8080/healthz` | `{"ok":true}` |
| 10 | Both backends: memory and `redis://redis:6379/0` binaries on separate ports (Task 44 Step 2) | `memory: {"ok":true}` and `redis:  {"ok":true}` |
| 11 | Open `http://localhost:8080` on the Mac | one page, one button reading **Rent a dumpster**; after renting, rows prepend live |

**On quoting counts:** every "N tests" figure in this plan is derived from the test
functions the plan itself writes — the file contents are fixed, so the counts
hold. When you add a test of your own, the count in that file changes: update it
in the same commit rather than leaving a stale number behind. Only the `ok`/`FAIL`
status and the exit code actually gate anything; the `0.00Xs` duration varies run
to run and means nothing.

---

## 15. Risks, tradeoffs, and open questions

### 15.1 Risks, with the mitigation already in the plan

| Risk | Likelihood | Mitigation |
| --- | --- | --- |
| `grpc.ForceServerCodec` + `UnknownServiceHandler` are marked `Experimental` in grpc-go | Low (stable in practice since v1.38) | They are the only two experimental APIs used. If a future grpc-go breaks them, the fallback is a registered `ServiceDesc` with a hand-written handler — same record shape, ~40 lines. |
| gRPC clients that require reflection fail | Certain (by design) | Nothing advertises reflection. `examples/dumpster.proto` plus `x-dumpster-id` is the documented path; `cmd/grpcping` needs neither. Say so in the README instead of pretending reflection works. |
| The dev container dies when the daemon restarts | Medium | `./scripts/dev.sh up` recreates it in seconds (image and volumes are cached). Costs nothing but a re-run; `down` is idempotent too. |
| Build cache lives on a named volume that a `docker system prune` can still eat | Low | Worst case is one slow rebuild. `go test` stays correct either way. |
| Unauthenticated public endpoint gets abused | Medium | Per-IP rent limit, global dumpster cap, body/record/event caps, no outbound requests, no code execution. Not a defence against a determined attacker — it is a speed bump, and D8 says so. |
| Dumps contain secrets | Certain | The UI states the link is public, the README repeats it. No redaction: silently masking headers would make the tool lie, which is worse for a debugger. |
| `text/event-stream` behind a buffering proxy | Medium | `X-Accel-Buffering: no` + immediate `Flush()`. If fields still arrive in one burst, the proxy is buffering and the fix is on the proxy. |
| `TestHTTPIngestAcceptsEveryMethod` failed once at exactly 3.00 s in a full-package run | Observed once; not reproducible in 22 subsequent runs (12 isolated, 10 full-package) | That 3 s figure is the test's own teardown deadline, and the failure coincided with a concurrent image build. Re-run the package before investigating; if it ever recurs in isolation, suspect a handler outliving the drain |
| Deeply nested JSON bodies blow up `encoding/json` | Low | Body cap is 1 MiB, and `Decoded.Value` is `any` — memory is bounded by the body, not by nesting in a dangerous way; Go's decoder handles depth. |
| Memory grows with many open WS/SSE connections | Medium | Each connection is a goroutine plus a 128-message channel; 10 000 idle sockets is tens of MB, acceptable. TCP idle timeout is 90 s. No global connection cap — **known gap**, see OQ-4. |
| UUIDv7 collisions | Negligible | 74 random bits per millisecond; `Create` retries 5 times on the impossible case. |

### 15.2 Tradeoffs taken deliberately

- **Two backends behind one interface.** In-memory is the default: nothing to run, and `go test ./...` needs no service. Redis is opt-in and costs 2–4 round trips per ingest — it buys durability (only with AOF) and multi-instance sharing, never speed. `AppendEvent` does a read-modify-write of the record JSON rather than a separate event list, which keeps one wire format for both backends at the price of re-encoding a bounded blob per frame.
- **Four ports instead of one.** More surface to firewall, but each protocol gets a boring, standard listener instead of a multiplexer (D3, §2.4).
- **No raw socket capture.** The "raw body" is exact; the "raw head" is reconstructed from the parsed request (D4). Byte-exact framing on keep-alive connections would need per-request buffers on a shared socket.
- **Whole-record broadcasts.** Every event update re-sends the entire record rather than a delta. Records are capped at 500 events and 1 MiB, so a burst of frames re-sends up to that much — wasteful, simple, and impossible to get out of sync.
- **Vanilla JS.** No framework, no build, no npm. The cost is a hand-rolled `el()` helper and a `Map` keyed by `seq`.

### 15.3 Open questions — answer these in the README when you know

- **OQ-1 — Hostname and TLS.** Development is `http://localhost:8080`. If it goes behind Traefik, `-trust-proxy` must be on **and** `-base-url` set, otherwise the copy-paste commands advertise `localhost`. Decide before the first deploy.
- **OQ-2 — Persistence.** *Answered:* Redis is the optional backend (§13A), selected
  by `DUMPSTER_REDIS_URL`. It survives a redeploy only with `appendonly yes`; the
  shipped compose file sets it. What is still open: whether the 12 h TTL should
  become per-dumpster (`?ttl=1h`, capped) rather than global — that is a product
  decision, and it changes the rent response, not the store.
- **OQ-3 — Reachability of ports 8081–8083.** Many corporate networks block everything but 80/443. Options: a single-port design via `cmux` (rejected in §2.4), or accept that gRPC/TCP/UDP are dev-machine tools. Undecided.
- **OQ-4 — Global connection cap.** Nothing limits concurrent long-lived sockets. If this ever faces the internet, add a semaphore in `handleWSIngest`/`handleSSEIngest` returning 503 when saturated.
- **OQ-5 — A public "yard" listing.** A page listing active dumpsters would make sharing easier and would also publish every dumpster's URL to the world. Deliberately not built.
- **OQ-6 — Export.** `/_records.json` is the export today. A `Content-Disposition: attachment` variant is one line if someone wants to download a dump.

### 15.4 What is deliberately **not** implemented

- Authentication, accounts, ownership, or "my dumpsters" — no auth was specified.
- WebSocket subprotocol negotiation beyond recording `Sec-WebSocket-Protocol`.
- gRPC reflection, server streaming configuration, deadlines, or compression.
- TLS termination in-process (put it behind a proxy).
- Any outbound request: no webhooks, no forwarding, no SSRF surface.
- Raw byte-exact wire capture of request heads (D4).

---

## 16. Appendix

### A. Protocol cheat sheet (what the debug page shows)

| Protocol | Client sends | Dumpster id lives in | Record has |
| --- | --- | --- | --- |
| HTTP | any request | URL path `/d/{id}` | method, path, subpath, query, host, version, headers, remote, timings, raw body, decoded body |
| SSE | `GET` with `Accept: text/event-stream` | URL path `/d/{id}/sse` | request envelope + one event per emitted event |
| WebSocket | upgrade on `/d/{id}/ws` | URL path | request envelope + one event per **received** frame, echoes frames |
| gRPC | any RPC | method path `/d{hex}/{Method}` or `x-dumpster-id` metadata | full method, metadata headers, remote, one event per message, echoes the first |
| TCP | id on the first `\n`-terminated line | first line | remote, local addr, id line, one event per read chunk |
| UDP | `<36-byte uuid>[sep]<payload>` | first 36 bytes | remote, one event (the payload), datagram size |

### B. Wire messages on the dashboard socket

```json
{"type":"snapshot","dumpster":{"id":"…","created_at":"…","expires_at":"…"},"records":[…]}
{"type":"new","record":{…}}
{"type":"update","record":{…}}
{"type":"expired"}
```

`records` is newest-first. The browser keys rows by `record.seq` and replaces the
whole row on `update`, so no delta logic exists on either side.

### C. Environment variables (complete list)

```
DUMPSTER_HTTP_ADDR  DUMPSTER_GRPC_ADDR  DUMPSTER_TCP_ADDR  DUMPSTER_UDP_ADDR
DUMPSTER_BASE_URL   DUMPSTER_PUBLIC_GRPC DUMPSTER_PUBLIC_TCP DUMPSTER_PUBLIC_UDP
DUMPSTER_TTL        DUMPSTER_REAP_EVERY DUMPSTER_CONN_IDLE
DUMPSTER_MAX_BODY   DUMPSTER_MAX_RECORDS DUMPSTER_MAX_EVENTS
DUMPSTER_MAX_DUMPSTERS DUMPSTER_RENT_PER_HOUR DUMPSTER_TRUST_PROXY
```

### D. Where each requirement is satisfied

| Requirement | Implemented in | Proven by |
| --- | --- | --- |
| "Rent a dumpster" CTA | `internal/web/templates/index.html`, `handleRent` | `TestIndexHasExactlyOneCTA`, `TestRentMintsDumpster` |
| UUIDv7 primary key | `internal/id.New` | `TestNewIsOrderedV7` |
| HTTP: headers, remote IP, timings, raw + decoded body | `ingest_http.go`, `decode` | `TestHTTPIngestDumpsRequest`, `TestHTTPIngestFormBody`, `TestHTTPIngestBinaryBodyIsBase64`, `TestHTTPIngestTruncatesHugeBody` |
| gRPC | `ingest_grpc.go`, `rawproto` | `TestGRPCDumpsRawMessageByPath`, `TestGRPCDumpsByMetadataHeader` |
| WebSocket | `ingest_ws.go` | `TestWSIngestEchoesAndRecordsFrames`, `TestWSIngestBinaryFrame` |
| SSE, triggered from the app | `ingest_sse.go`, `#btn-sse` in `app.js` | `TestSSEStreamsAndRecordsEvents`, `TestSSECustomEventNameAndClamping` |
| TCP | `ingest_tcp.go` | `TestTCPRecordsIDLineAndChunks`, `TestTCPRejectsUnknownDumpster` |
| UDP | `ingest_udp.go` | `TestUDPDatagramBecomesOneRecord`, `TestUDPTwoDatagramsTwoRecords` |
| One dumpster, many protocols at once | one `store` entry, six ingest paths | `scripts/smoke.sh` asserts five kinds in one snapshot |
| Single list, prepended in realtime | `hub`, `live.go`, `app.js` | `TestLiveSendsSnapshotThenNewRecords` |
| Deleted after 12 h | `reaper.go`, `store.DeleteExpired` | `TestReaperRemovesExpired`, `TestLiveSendsExpiredWhenReaped`, `TestExpiryHidesAndDeletes` |
| Highly concurrent | goroutine per connection, per-dumpster locks | `cmd/loadgen` measurement, quoted in your report |
| Websocket **and** SSE both used | dashboard socket + SSE ingest | both test files above |
