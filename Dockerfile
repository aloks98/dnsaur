# syntax=docker/dockerfile:1

# The dashboard is built first and handed to the Go stage, because
# //go:embed all:dist reads it at compile time (web/embed.go). Pinned to the
# same Node the CI web job uses.
FROM --platform=$BUILDPLATFORM node:24-alpine AS web
WORKDIR /web
RUN corepack enable
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm build

# Cross-compiled rather than emulated: the binary is pure Go with
# CGO_ENABLED=0 (modernc.org/sqlite, no cgo), so an arm64 image builds on an
# amd64 runner at native speed. --platform=$BUILDPLATFORM keeps the toolchain
# native and GOARCH does the work.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /web/dist ./web/dist
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.Version=${VERSION}" -o /out/dnsaur ./cmd/dnsaur
# Created here so the next stage can copy it with an owner: distroless has no
# shell to mkdir with.
RUN mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
LABEL org.opencontainers.image.title="dnsaur" \
      org.opencontainers.image.description="Self-hosted DNS server with per-client blocklist filtering, authoritative zones, a query log and a web dashboard" \
      org.opencontainers.image.source="https://github.com/aloks98/dnsaur" \
      org.opencontainers.image.documentation="https://github.com/aloks98/dnsaur/blob/main/README.md" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/dnsaur /dnsaur
COPY --from=build --chown=65532:65532 /out/data /data
# WORKDIR, not a -config flag. config.DefaultPath is the relative
# "dnsaur.yaml", and it is the one path config.Load tolerates being absent —
# so a container configured entirely through DNSAUR_* env vars starts, and
# one with a file bind-mounted at /data/dnsaur.yaml picks it up. Naming the
# path explicitly would make the file mandatory and break the first case.
WORKDIR /data
VOLUME /data
ENV DNSAUR_DATA_DIR=/data
EXPOSE 53/udp 53/tcp 8080 853 443
ENTRYPOINT ["/dnsaur"]
