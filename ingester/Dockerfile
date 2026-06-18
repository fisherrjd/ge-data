# Multi-stage build for the ge-data ingester: one static Go binary on a
# distroless base. No CGO, no shell, runs as nonroot. The build context is this
# ingester/ directory (the Go module root) — see ../.github/workflows/docker-publish.yml.

# ---- build ----
FROM golang:1.26.3-bookworm AS build
WORKDIR /src

# Module graph first so `go mod download` stays cached until go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

# Then the source. CGO off => a fully static binary that runs on distroless/scratch.
# -trimpath strips local build paths; -ldflags "-s -w" drops the symbol/debug tables.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" -o /out/ingester .

# ---- runtime ----
# distroless/static ships ca-certificates (for the Wiki HTTPS calls) and tzdata,
# and nothing else: no shell, no package manager. :nonroot runs as uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ingester /ingester
USER nonroot:nonroot
ENTRYPOINT ["/ingester"]
