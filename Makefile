export PATH := $(PATH):$(shell go env GOPATH)/bin

RPCS := copywriting digitalhuman note
# 使用 goctl 内置默认模板，避免受本机 ~/.goctl 自定义模板影响
GOCTL_HOME := $(CURDIR)/deploy/goctl

.PHONY: gen gen-rpc gen-api gen-model db build test run-all stop-all

gen: gen-rpc gen-api gen-model

gen-rpc:
	@for s in $(RPCS); do \
		echo "gen rpc $$s"; \
		(cd app/$$s/rpc && goctl rpc protoc $$s.proto --home $(GOCTL_HOME) --go_out=. --go-grpc_out=. --zrpc_out=. --style=go_zero -m=false) || exit 1; \
	done

gen-api:
	cd app/gateway/api && goctl api go -api gateway.api -dir . --style=go_zero --home $(GOCTL_HOME)
	cd app/admin/api && goctl api go -api admin.api -dir . --style=go_zero --home $(GOCTL_HOME)

gen-model:
	goctl model mysql ddl -src deploy/sql/V1.0.106__ai_copywriting.sql -dir app/copywriting/model --style=go_zero --home $(GOCTL_HOME)
	goctl model mysql ddl -src deploy/sql/V1.0.108__ai_digital_human.sql -dir app/digitalhuman/model --style=go_zero --home $(GOCTL_HOME)
	goctl model mysql ddl -src deploy/sql/V1.0.109__ai_image_text_note.sql -dir app/note/model --style=go_zero --home $(GOCTL_HOME)

# 建新表（只新增 afv_copywriting* / afv_digital_human* 等新表，不碰现有表结构）
db:
	docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc < deploy/sql/V1.0.106__ai_copywriting.sql
	docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc < deploy/sql/V1.0.108__ai_digital_human.sql
	docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc < deploy/sql/V1.0.109__ai_image_text_note.sql

build:
	@mkdir -p bin
	go build -o bin/copywriting-rpc  ./app/copywriting/rpc
	go build -o bin/digitalhuman-rpc ./app/digitalhuman/rpc
	go build -o bin/note-rpc         ./app/note/rpc
	go build -o bin/gateway-api      ./app/gateway/api
	go build -o bin/admin-api        ./app/admin/api
	go build -o bin/mock-aigc        ./tools/mock-aigc

test:
	go test ./...

# 本地一键启动（日志写到 logs/）
run-all: build stop-all
	@mkdir -p logs
	nohup bin/mock-aigc        -addr :5055                           > logs/mock-aigc.log 2>&1 &
	nohup bin/copywriting-rpc  -f deploy/etc/copywriting-rpc.yaml  > logs/copywriting-rpc.log 2>&1 &
	nohup bin/digitalhuman-rpc -f deploy/etc/digitalhuman-rpc.yaml > logs/digitalhuman-rpc.log 2>&1 &
	nohup bin/note-rpc         -f deploy/etc/note-rpc.yaml         > logs/note-rpc.log 2>&1 &
	nohup bin/gateway-api      -f deploy/etc/gateway-api.yaml      > logs/gateway-api.log 2>&1 &
	nohup bin/admin-api        -f deploy/etc/admin-api.yaml        > logs/admin-api.log 2>&1 &
	@echo "started, logs in ./logs"

PROCS := bin/(copywriting-rpc|digitalhuman-rpc|note-rpc|gateway-api|admin-api|mock-aigc)

# 等待优雅退出完成，避免新进程启动时端口仍被占用
stop-all:
	-@pkill -f '$(PROCS)' || true
	@for i in $$(seq 1 30); do pgrep -f '$(PROCS)' >/dev/null || exit 0; sleep 0.5; done; \
		echo "force kill"; pkill -9 -f '$(PROCS)' || true
