export PATH := $(PATH):$(shell go env GOPATH)/bin

RPCS := copywriting digitalhuman note imagegen studio assetlib canvas
# 使用 goctl 内置默认模板，避免受本机 ~/.goctl 自定义模板影响
GOCTL_HOME := $(CURDIR)/deploy/goctl

.PHONY: gen gen-rpc gen-api gen-model gen-audit db build test test-migrate run-all stop-all release package image

gen: gen-rpc gen-api gen-model

gen-rpc:
	@for s in $(RPCS); do \
		echo "gen rpc $$s"; \
		(cd app/$$s/rpc && goctl rpc protoc $$s.proto --home $(GOCTL_HOME) --go_out=. --go-grpc_out=. --zrpc_out=. --style=go_zero -m=false) || exit 1; \
	done

gen-api:
	cd app/gateway/api && goctl api go -api gateway.api -dir . --style=go_zero --home $(GOCTL_HOME)
	cd app/admin/api && goctl api go -api admin.api -dir . --style=go_zero --home $(GOCTL_HOME)
	goctl api swagger --api app/gateway/api/gateway.api --dir app/gateway/api/doc --filename gateway
	goctl api swagger --api app/admin/api/admin.api --dir app/admin/api/doc --filename admin

gen-model:
	goctl model mysql ddl -src deploy/sql/Go_V1.0.0__ai_copywriting.sql -dir app/copywriting/model --style=go_zero --home $(GOCTL_HOME)
	goctl model mysql ddl -src deploy/sql/Go_V1.0.7__shop_profile.sql -dir app/copywriting/model --style=go_zero --home $(GOCTL_HOME)
	goctl model mysql ddl -src deploy/sql/Go_V1.0.1__ai_digital_human.sql -dir app/digitalhuman/model --style=go_zero --home $(GOCTL_HOME)
	goctl model mysql ddl -src deploy/sql/Go_V1.0.2__ai_image_text_note.sql -dir app/note/model --style=go_zero --home $(GOCTL_HOME)
	@echo '注意：V1.0.111 含 INSERT..SELECT，goctl 解析不了，改表结构时手工截出 CREATE TABLE 再生成 app/imagegen/model'

# 计费 / 对账审计的只读查询（sqlc，见 sqlc.yaml）：改 db/audit/{schema,query}/*.sql 后执行，产物 pkg/auditdb。
# 用 go run 临时拉 sqlc，不进本仓库 go.mod；本机装了 sqlc 也可直接 `sqlc generate`。
gen-audit:
	go run github.com/sqlc-dev/sqlc/cmd/sqlc@latest generate

# 本地开发用：直接执行、不写 go_schema_history 记录。正式环境只走 install.sh（会登记、会备份）
# 建新表（只新增 afv_copywriting* / afv_digital_human* 等新表，不碰现有表结构）
db:
	docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc < deploy/sql/Go_V1.0.0__ai_copywriting.sql
	docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc < deploy/sql/Go_V1.0.1__ai_digital_human.sql
	docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc < deploy/sql/Go_V1.0.2__ai_image_text_note.sql
	docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc < deploy/sql/Go_V1.0.3__asset_category_local_life.sql
	docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc < deploy/sql/Go_V1.0.4__ai_poster_long_image.sql
	docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc < deploy/sql/Go_V1.0.7__shop_profile.sql
	docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc < deploy/sql/Go_V1.0.9__ai_video_creation.sql
	docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc < deploy/sql/Go_V1.0.21__studio.sql
	docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc < deploy/sql/Go_V1.0.22__admin_studio_permission.sql
	docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc < deploy/sql/Go_V1.0.23__asset_ext.sql
	docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc < deploy/sql/Go_V1.0.24__admin_assetlib_permission.sql
	docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc < deploy/sql/Go_V1.0.25__canvas.sql
	docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc < deploy/sql/Go_V1.0.26__admin_canvas_permission.sql
	docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc < deploy/sql/Go_V1.0.27__studio_task_color_fix.sql
	docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc < deploy/sql/Go_V1.0.28__studio_mask.sql
	docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc < deploy/sql/seed/canvas-official-prompts.sql
	docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc < deploy/sql/seed/canvas-official-templates.sql

build:
	@mkdir -p bin
	go build -o bin/copywriting-rpc  ./app/copywriting/rpc
	go build -o bin/digitalhuman-rpc ./app/digitalhuman/rpc
	go build -o bin/note-rpc         ./app/note/rpc
	go build -o bin/imagegen-rpc     ./app/imagegen/rpc
	go build -o bin/studio-rpc       ./app/studio/rpc
	go build -o bin/assetlib-rpc     ./app/assetlib/rpc
	go build -o bin/canvas-rpc       ./app/canvas/rpc
	go build -o bin/gateway-api      ./app/gateway/api
	go build -o bin/admin-api        ./app/admin/api
	go build -o bin/mock-aigc        ./tools/mocks/mock-aigc
	go build -o bin/mock-union       ./tools/mocks/mock-union
	go build -o bin/mock-linkparse   ./tools/mocks/mock-linkparse
	go build -o bin/mock-reverse     ./tools/mocks/mock-reverse
	go build -o bin/mock-aigateway   ./tools/mocks/mock-aigateway
	go build -o bin/mock-media       ./tools/mocks/mock-media

test:
	go test ./...

# 迁移记录逻辑（离线，用假库）
test-migrate:
	bash tests/migrate/migrate_test.sh

# 交叉编译 Linux 二进制（部署用），产物在 bin/linux/
release:
	@mkdir -p bin/linux
	@for s in copywriting/rpc digitalhuman/rpc note/rpc imagegen/rpc studio/rpc assetlib/rpc canvas/rpc gateway/api admin/api; do \
		name=$$(echo $$s | sed 's#/rpc#-rpc#; s#/api#-api#'); \
		echo "build $$name"; \
		GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -tags timetzdata -trimpath -ldflags '-s -w' -o bin/linux/$$name ./app/$$s || exit 1; \
	done
	@ls -lh bin/linux

# 正式镜像（容器部署用）：linux/amd64（线上是 x86_64），不含 mock。
#   make image IMAGE=<仓库>/aigc-local-life VERSION=v1.0.0           # 构建到本机
#   make image IMAGE=<仓库>/aigc-local-life VERSION=v1.0.0 PUSH=1    # 直接推送；部署时 .env 的 AIGC_IMAGE 填推送后的 digest
IMAGE ?= aigc-local-life
image:
	docker buildx build --platform linux/amd64 -f deploy/docker/Dockerfile \
		--build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) $(if $(filter 1,$(PUSH)),--push,--load) .

# 服务器部署包：dist/aigc-local-life-<版本>.tar.gz，上传到服务器解压后 bash install.sh（见 deploy/server/README.md）
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
package:
	@rm -rf dist/aigc-local-life dist/*.tar.gz   # 只留这一次的包，免得部署时拿错版本
	@mkdir -p dist/aigc-local-life/bin dist/aigc-local-life/etc-dev dist/aigc-local-life/sql dist/aigc-local-life/scripts dist/aigc-local-life/docs
	@for s in copywriting/rpc digitalhuman/rpc note/rpc imagegen/rpc studio/rpc assetlib/rpc canvas/rpc gateway/api admin/api; do \
		name=$$(echo $$s | sed 's#/rpc#-rpc#; s#/api#-api#'); \
		echo "build $$name"; \
		GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -tags timetzdata -trimpath -ldflags '-s -w' -o dist/aigc-local-life/bin/$$name ./app/$$s || exit 1; \
	done
	@for t in mkconf seed billing-check; do \
		echo "build $$t"; \
		GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -tags timetzdata -trimpath -ldflags '-s -w' -o dist/aigc-local-life/bin/$$t ./tools/ops/$$t || exit 1; \
	done
	cp deploy/etc/*.yaml dist/aigc-local-life/etc-dev/
	cp deploy/sql/Go_V*.sql dist/aigc-local-life/sql/
	cp deploy/server/migrate.sh dist/aigc-local-life/
	cp deploy/scripts/gen-voice-previews.sh deploy/scripts/seed-templates.sh dist/aigc-local-life/scripts/   # e2e-real 会真实调用模型、产生费用，不进正式包
	cp deploy/sql/ops/fix-billing-offers-2026-10-16.sql deploy/sql/ops/check-billing-online.sql dist/aigc-local-life/scripts/   # 手工执行的，不会被 install.sh 自动跑
	cp -r docs/api dist/aigc-local-life/docs/   # 前端接口文档；计费与问答文档是内部资料，不进正式包
	cp deploy/server/install.sh deploy/server/deploy.env.example deploy/server/README.md dist/aigc-local-life/
	cp -r deploy/server/nginx dist/aigc-local-life/
	@echo $(VERSION) > dist/aigc-local-life/VERSION
	@if ls dist/aigc-local-life/bin | grep -q mock; then echo "正式包里不能有 mock"; exit 1; fi
	@if ls dist/aigc-local-life/docs | grep -qv "^api$$"; then echo "正式包只带 docs/api"; exit 1; fi
	tar -C dist -czf dist/aigc-local-life-$(VERSION).tar.gz aigc-local-life
	@ls -lh dist/aigc-local-life-$(VERSION).tar.gz

# 本地一键启动（日志写到 logs/）
run-all: build stop-all
	@mkdir -p logs
	nohup bin/mock-aigc        -addr :5055                           > logs/mock-aigc.log 2>&1 &
	nohup bin/mock-union       -addr :5056                           > logs/mock-union.log 2>&1 &
	nohup bin/mock-linkparse   -addr :5058                           > logs/mock-linkparse.log 2>&1 &
	nohup bin/mock-reverse     -addr :5057                           > logs/mock-reverse.log 2>&1 &
	nohup bin/mock-aigateway   -addr :5059                           > logs/mock-aigateway.log 2>&1 &
	nohup bin/copywriting-rpc  -f deploy/etc/copywriting-rpc.yaml  > logs/copywriting-rpc.log 2>&1 &
	nohup bin/digitalhuman-rpc -f deploy/etc/digitalhuman-rpc.yaml > logs/digitalhuman-rpc.log 2>&1 &
	nohup bin/note-rpc         -f deploy/etc/note-rpc.yaml         > logs/note-rpc.log 2>&1 &
	nohup bin/imagegen-rpc     -f deploy/etc/imagegen-rpc.yaml     > logs/imagegen-rpc.log 2>&1 &
	nohup bin/studio-rpc       -f deploy/etc/studio-rpc.yaml       > logs/studio-rpc.log 2>&1 &
	nohup bin/assetlib-rpc     -f deploy/etc/assetlib-rpc.yaml     > logs/assetlib-rpc.log 2>&1 &
	nohup bin/canvas-rpc       -f deploy/etc/canvas-rpc.yaml       > logs/canvas-rpc.log 2>&1 &
	nohup bin/gateway-api      -f deploy/etc/gateway-api.yaml      > logs/gateway-api.log 2>&1 &
	nohup bin/admin-api        -f deploy/etc/admin-api.yaml        > logs/admin-api.log 2>&1 &
	@echo "started, logs in ./logs"

PROCS := bin/(copywriting-rpc|digitalhuman-rpc|note-rpc|imagegen-rpc|studio-rpc|assetlib-rpc|canvas-rpc|gateway-api|admin-api|mock-aigc|mock-union|mock-linkparse|mock-reverse|mock-aigateway|mock-media)

# 等待优雅退出完成，避免新进程启动时端口仍被占用
stop-all:
	-@pkill -f '$(PROCS)' || true
	@for i in $$(seq 1 30); do pgrep -f '$(PROCS)' >/dev/null || exit 0; sleep 0.5; done; \
		echo "force kill"; pkill -9 -f '$(PROCS)' || true
