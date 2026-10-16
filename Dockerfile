# fusion-aigc-go 的服务镜像：一个 Dockerfile 构建任意一个进程（rpc / api / 本地 mock）。
#
#   docker build --build-arg PKG=./app/copywriting/rpc --build-arg BIN=copywriting-rpc -t aigc/copywriting-rpc .
#
# 配置不打进镜像：运行时挂载 deploy/docker/etc（由 deploy/docker/render-etc.sh 渲染）到 /app/etc。
ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG PKG
ARG BIN
RUN CGO_ENABLED=0 GOOS=linux go build -tags timetzdata -trimpath -ldflags '-s -w' -o /out/${BIN} ${PKG}

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tzdata && rm -rf /var/lib/apt/lists/*
ENV TZ=Asia/Shanghai
WORKDIR /app
ARG BIN
COPY --from=build /out/${BIN} /app/service
RUN mkdir -p /app/data && useradd --system --uid 10001 app && chown -R app /app
USER app
ENTRYPOINT ["/app/service"]
