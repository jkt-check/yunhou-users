# syntax=docker/dockerfile:1.7

# ---- Stage 1: build ----
FROM golang:1.25-alpine AS builder
WORKDIR /src

# 设置 Go Proxy 为中国镜像（bytedance/gopkg 在七牛云也有缓存）
ENV GOPROXY=https://goproxy.cn,https://goproxy.io,direct
ENV GOSUMDB=sum.golang.org

# Cache module download
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build a static, stripped binary
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -extldflags=-static" -o /out/server ./cmd/server

# ---- Stage 2: runtime ----
FROM alpine:3.20
RUN adduser -D -u 65532 appuser
COPY --from=builder /out/server /server
USER appuser
EXPOSE 8080
HEALTHCHECK CMD ["/server", "-healthcheck"]
ENTRYPOINT ["/server"]
