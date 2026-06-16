# syntax=docker/dockerfile:1

# --- build ---
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/ingester .

# --- runtime ---
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ingester /ingester
ENTRYPOINT ["/ingester"]
# default subcommand; the backfill job overrides this with ["backfill", ...]
CMD ["serve"]
