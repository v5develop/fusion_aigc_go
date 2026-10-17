# aigc-local-life 镜像：一个镜像包含全部服务与部署工具，各服务用不同的 command 启动。
#
#   正式（不含 mock）：docker build -f deploy/docker/Dockerfile --build-arg VERSION=v1.0.0 -t <仓库>/aigc-local-life:v1.0.0 .
#   本地联调（含 mock）：由 docker-compose.dev.yml 构建，INCLUDE_MOCKS=1
#
# 构建上下文是仓库根目录。配置不打进镜像：由 prepare 服务在启动时生成到挂载的 etc/。
ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG INCLUDE_MOCKS=0
RUN set -eux; mkdir -p /out; \
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
# mariadb-client：prepare 用它检查数据库、执行迁移、可选备份（mysql / mysqldump）
# curl：健康检查；jq：音色试听脚本
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tzdata curl jq mariadb-client \
 && rm -rf /var/lib/apt/lists/* \
 && mysql --version && mysqldump --version && jq --version
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
