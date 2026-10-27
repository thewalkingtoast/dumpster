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
PORT_MAP="${PORT_MAP:--p 8080:8080 -p 8081:8081 -p 8082:8082 -p 8083:8083}"

log() { printf '[dev] %s
' "$*" >&2; }

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
          SRC_DIR="$SRC_DIR" docker compose -f compose.dev.yaml down -v
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
    case "$MODE" in
      native) exec bash -lc "$cmd" ;;
      socket) exec docker exec -it -w /src "$NAME" bash -lc "$cmd" ;;
    esac ;;

  *)
    sed -n '2,14p' "$0"; exit 2 ;;
esac
