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
EXPOSE 8080 8081 8082 8083
ENTRYPOINT ["/usr/local/bin/dumpster"]
