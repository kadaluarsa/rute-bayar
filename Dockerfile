# syntax=docker/dockerfile:1.7
# Rute Bayar for Wishpull: static binary on distroless (modernc SQLite is
# pure Go, so CGO stays off).
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags "-s -w -X github.com/pendig/rute-bayar/internal/build.Version=${VERSION}" \
      -o /out/rutebayar ./cmd/rute-bayar

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/rutebayar /rutebayar
COPY deploy/config/routing.example.json /etc/rute-bayar/routing.json
ENV RUTE_BAYAR_DB_PATH=/data/rute-bayar.sqlite3 \
    RUTE_BAYAR_WEBHOOK_ADDR=:8080 \
    RUTE_BAYAR_ROUTING_CONFIG=/etc/rute-bayar/routing.json \
    RUTE_BAYAR_REQUIRE_MANDATE=true
VOLUME ["/data"]
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/rutebayar", "webhook", "serve", "--mode", "all"]
