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
