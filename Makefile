export PATH := $(PATH):$(shell go env GOPATH)/bin

RPCS := copywriting digitalhuman note imagegen
# 使用 goctl 内置默认模板，避免受本机 ~/.goctl 自定义模板影响
GOCTL_HOME := $(CURDIR)/deploy/goctl

.PHONY: gen gen-rpc gen-api gen-model db build test run-all stop-all release package

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
	goctl model mysql ddl -src deploy/sql/V1.0.106__ai_copywriting.sql -dir app/copywriting/model --style=go_zero --home $(GOCTL_HOME)
	goctl model mysql ddl -src deploy/sql/V1.0.114__shop_profile.sql -dir app/copywriting/model --style=go_zero --home $(GOCTL_HOME)
	goctl model mysql ddl -src deploy/sql/V1.0.108__ai_digital_human.sql -dir app/digitalhuman/model --style=go_zero --home $(GOCTL_HOME)
	goctl model mysql ddl -src deploy/sql/V1.0.109__ai_image_text_note.sql -dir app/note/model --style=go_zero --home $(GOCTL_HOME)
	@echo '注意：V1.0.111 含 INSERT..SELECT，goctl 解析不了，改表结构时手工截出 CREATE TABLE 再生成 app/imagegen/model'

# 建新表（只新增 afv_copywriting* / afv_digital_human* 等新表，不碰现有表结构）
db:
	docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc < deploy/sql/V1.0.106__ai_copywriting.sql
	docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc < deploy/sql/V1.0.108__ai_digital_human.sql
	docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc < deploy/sql/V1.0.109__ai_image_text_note.sql
	docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc < deploy/sql/V1.0.110__asset_category_localLife.sql
	docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc < deploy/sql/V1.0.111__ai_poster_long_image.sql
	docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc < deploy/sql/V1.0.114__shop_profile.sql
	docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc < deploy/sql/V1.0.116__ai_video_creation.sql

build:
	@mkdir -p bin
	go build -o bin/copywriting-rpc  ./app/copywriting/rpc
	go build -o bin/digitalhuman-rpc ./app/digitalhuman/rpc
	go build -o bin/note-rpc         ./app/note/rpc
	go build -o bin/imagegen-rpc     ./app/imagegen/rpc
	go build -o bin/gateway-api      ./app/gateway/api
	go build -o bin/admin-api        ./app/admin/api
	go build -o bin/mock-aigc        ./tools/mock-aigc
	go build -o bin/mock-union       ./tools/mock-union

test:
	go test ./...

# 交叉编译 Linux 二进制（部署用），产物在 bin/linux/
release:
	@mkdir -p bin/linux
	@for s in copywriting/rpc digitalhuman/rpc note/rpc imagegen/rpc gateway/api admin/api; do \
		name=$$(echo $$s | sed 's#/rpc#-rpc#; s#/api#-api#'); \
		echo "build $$name"; \
		GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -tags timetzdata -trimpath -ldflags '-s -w' -o bin/linux/$$name ./app/$$s || exit 1; \
	done
	@ls -lh bin/linux

# 服务器部署包：dist/aigc-life-<版本>.tar.gz，上传到服务器解压后 bash install.sh（见 deploy/server/README.md）
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
package:
	@rm -rf dist/aigc-life && mkdir -p dist/aigc-life/bin dist/aigc-life/etc-dev dist/aigc-life/sql dist/aigc-life/scripts dist/aigc-life/docs
	@for s in copywriting/rpc digitalhuman/rpc note/rpc imagegen/rpc gateway/api admin/api; do \
		name=$$(echo $$s | sed 's#/rpc#-rpc#; s#/api#-api#'); \
		echo "build $$name"; \
		GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -tags timetzdata -trimpath -ldflags '-s -w' -o dist/aigc-life/bin/$$name ./app/$$s || exit 1; \
	done
	@for t in mock-aigc mkconf seed; do \
		echo "build $$t"; \
		GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -tags timetzdata -trimpath -ldflags '-s -w' -o dist/aigc-life/bin/$$t ./tools/$$t || exit 1; \
	done
	cp deploy/etc/*.yaml dist/aigc-life/etc-dev/
	cp deploy/sql/V*.sql dist/aigc-life/sql/
	cp deploy/scripts/gen-voice-previews.sh dist/aigc-life/scripts/
	cp docs/*.md dist/aigc-life/docs/
	cp deploy/server/install.sh deploy/server/deploy.env.example deploy/server/README.md dist/aigc-life/
	@echo $(VERSION) > dist/aigc-life/VERSION
	tar -C dist -czf dist/aigc-life-$(VERSION).tar.gz aigc-life
	@ls -lh dist/aigc-life-$(VERSION).tar.gz

# 本地一键启动（日志写到 logs/）
run-all: build stop-all
	@mkdir -p logs
	nohup bin/mock-aigc        -addr :5055                           > logs/mock-aigc.log 2>&1 &
	nohup bin/mock-union       -addr :5056                           > logs/mock-union.log 2>&1 &
	nohup bin/copywriting-rpc  -f deploy/etc/copywriting-rpc.yaml  > logs/copywriting-rpc.log 2>&1 &
	nohup bin/digitalhuman-rpc -f deploy/etc/digitalhuman-rpc.yaml > logs/digitalhuman-rpc.log 2>&1 &
	nohup bin/note-rpc         -f deploy/etc/note-rpc.yaml         > logs/note-rpc.log 2>&1 &
	nohup bin/imagegen-rpc     -f deploy/etc/imagegen-rpc.yaml     > logs/imagegen-rpc.log 2>&1 &
	nohup bin/gateway-api      -f deploy/etc/gateway-api.yaml      > logs/gateway-api.log 2>&1 &
	nohup bin/admin-api        -f deploy/etc/admin-api.yaml        > logs/admin-api.log 2>&1 &
	@echo "started, logs in ./logs"

PROCS := bin/(copywriting-rpc|digitalhuman-rpc|note-rpc|imagegen-rpc|gateway-api|admin-api|mock-aigc|mock-union)

# 等待优雅退出完成，避免新进程启动时端口仍被占用
stop-all:
	-@pkill -f '$(PROCS)' || true
	@for i in $$(seq 1 30); do pgrep -f '$(PROCS)' >/dev/null || exit 0; sleep 0.5; done; \
		echo "force kill"; pkill -9 -f '$(PROCS)' || true
