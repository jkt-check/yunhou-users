# syntax=docker/dockerfile:1.7

# ---- Stage 1a: build server ----
FROM golang:1.25-alpine AS build-server
WORKDIR /src

ENV GOPROXY=https://goproxy.cn,https://goproxy.io,direct
ENV GOSUMDB=sum.golang.org

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -extldflags=-static" -o /out/server ./cmd/server

# ---- Stage 1b: build migrate ----
FROM golang:1.25-alpine AS build-migrate
WORKDIR /src

ENV GOPROXY=https://goproxy.cn,https://goproxy.io,direct
ENV GOSUMDB=sum.golang.org

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -extldflags=-static" -o /out/migrate ./cmd/migrate

# ---- Stage 2: runtime (server) ----
FROM alpine:3.20
RUN adduser -D -u 65532 appuser
COPY --from=build-server /out/server /server
COPY --from=build-migrate /out/migrate /migrate
# Migrate reads SQL files at runtime — Go embed rules forbid `..` paths,
# so the runtime COPY is the supported way to bundle migrations/.
COPY migrations/ /migrations
USER appuser
EXPOSE 8080
HEALTHCHECK CMD ["/server", "-healthcheck"]
ENTRYPOINT ["/server"]