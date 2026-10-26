# aigc-local-life 镜像：一个镜像包含全部服务与部署工具，各服务用不同的 command 启动。
#
#   正式（不含 mock）：docker build -f deploy/docker/Dockerfile --build-arg VERSION=v1.0.0 -t <仓库>/aigc-local-life:v1.0.0 .
#   本地联调（含 mock）：由 docker-compose.dev.yml 构建，INCLUDE_MOCKS=1
#
# 构建上下文是仓库根目录。配置不打进镜像：由 prepare 服务在启动时生成到挂载的 etc/。
ARG GO_VERSION=1.26

# 编译阶段跑在构建机本机架构（Apple Silicon 上不走 QEMU 模拟），用 GOOS/GOARCH 交叉编译到目标平台；
# 全部 CGO_ENABLED=0，交叉编译出的二进制与在目标架构上编译的一致。运行时阶段仍是目标平台。
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
# 模块与编译缓存用 BuildKit 缓存挂载，跨次构建保留（缓存不进镜像层）
RUN --mount=type=cache,target=/go/pkg/mod go mod download
# 只拷编译需要的源码：deploy/ 等改动不会让编译层失效
COPY app/ app/
COPY pkg/ pkg/
COPY tools/ tools/
ARG INCLUDE_MOCKS=0
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    set -eux; mkdir -p /out; \
    export GOOS="$TARGETOS" GOARCH="$TARGETARCH"; \
    for s in copywriting/rpc digitalhuman/rpc note/rpc imagegen/rpc gateway/api admin/api; do \
      n=$(echo "$s" | sed 's#/rpc#-rpc#; s#/api#-api#'); \
      CGO_ENABLED=0 go build -tags timetzdata -trimpath -ldflags '-s -w' -o "/out/$n" "./app/$s"; \
    done; \
    for t in mkconf billing-check seed; do \
      CGO_ENABLED=0 go build -tags timetzdata -trimpath -ldflags '-s -w' -o "/out/$t" "./tools/ops/$t"; \
    done; \
    if [ "$INCLUDE_MOCKS" = 1 ]; then \
      for t in mock-aigc mock-union mock-linkparse; do \
        CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o "/out/$t" "./tools/mocks/$t"; \
      done; \
    fi

FROM debian:bookworm-slim
# MySQL 官方客户端（mysql / mysqldump）：prepare 用它检查数据库、执行迁移、可选备份。
# 不能用 MariaDB 客户端：RDS / MySQL 8 默认的 caching_sha2_password 认证它没有插件，连接直接失败。
# curl：健康检查；jq：音色试听脚本
# MySQL 官方 APT 仓库的 8.4 客户端只发布 amd64（线上服务器是 x86_64）：其它架构直接报错，别拿到一个装不上的镜像。
RUN test "$(dpkg --print-architecture)" = amd64 || { echo '运行时镜像只支持 linux/amd64（MySQL 官方客户端只有 amd64）：用 --platform linux/amd64 构建'; exit 1; }
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tzdata curl wget jq gnupg lsb-release \
 && curl -fsSL -o /tmp/mysql-apt-config.deb https://dev.mysql.com/get/mysql-apt-config_0.8.36-1_all.deb \
 && DEBIAN_FRONTEND=noninteractive dpkg -i /tmp/mysql-apt-config.deb \
 && rm -f /tmp/mysql-apt-config.deb \
 && apt-get update \
 && apt-get install -y --no-install-recommends mysql-community-client \
 && rm -rf /var/lib/apt/lists/* \
 && mysql --version && mysqldump --version && jq --version \
 && mysql --version | grep -q "Ver 8\." || (echo "mysql 客户端不是 8.x" && exit 1)
ENV TZ=Asia/Shanghai
RUN groupadd --system --gid 10001 app \
 && useradd --system --uid 10001 --gid 10001 --home-dir /app --shell /usr/sbin/nologin app
WORKDIR /app
COPY --from=build /out/ /app/pkg/bin/
COPY deploy/etc/ /app/pkg/etc-dev/
COPY deploy/sql/Go_V*.sql /app/pkg/sql/
COPY deploy/server/install.sh deploy/server/migrate.sh /app/pkg/
COPY deploy/server/nginx/ /app/pkg/nginx/
COPY deploy/scripts/gen-voice-previews.sh /app/pkg/scripts/
ARG VERSION=dev
RUN echo "$VERSION" > /app/pkg/VERSION \
 && mkdir -p /app/etc /app/nginx /app/backup /app/logs \
 && chown -R app:app /app/etc /app/logs
USER app
