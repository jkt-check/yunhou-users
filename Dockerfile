# syntax=docker/dockerfile:1.7

# ---- Stage 1: build ----
FROM golang:1.25-alpine AS builder
WORKDIR /src

# Cache module download
COPY go.mod go.sum ./
RUN go mod download

# Copy source and run unit tests (e2e excluded — needs a live DB)
COPY . .
RUN go test -race ./internal/...

# Build a static, stripped binary
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# ---- Stage 2: runtime ----
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/server /server
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/server"]
