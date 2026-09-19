export PATH := $(PATH):$(shell go env GOPATH)/bin

RPCS := copywriting
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

# 建新表（只新增 afv_copywriting*，不碰现有表）
db:
	docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc < deploy/sql/V1.0.106__ai_copywriting.sql

build:
	@mkdir -p bin
	go build -o bin/copywriting-rpc ./app/copywriting/rpc
	go build -o bin/gateway-api     ./app/gateway/api
	go build -o bin/admin-api       ./app/admin/api

test:
	go test ./...

# 本地一键启动（日志写到 logs/）
run-all: build stop-all
	@mkdir -p logs
	nohup bin/copywriting-rpc -f deploy/etc/copywriting-rpc.yaml > logs/copywriting-rpc.log 2>&1 &
	nohup bin/gateway-api     -f deploy/etc/gateway-api.yaml     > logs/gateway-api.log 2>&1 &
	nohup bin/admin-api       -f deploy/etc/admin-api.yaml       > logs/admin-api.log 2>&1 &
	@echo "started, logs in ./logs"

PROCS := bin/(copywriting-rpc|gateway-api|admin-api)

# 等待优雅退出完成，避免新进程启动时端口仍被占用
stop-all:
	-@pkill -f '$(PROCS)' || true
	@for i in $$(seq 1 30); do pgrep -f '$(PROCS)' >/dev/null || exit 0; sleep 0.5; done; \
		echo "force kill"; pkill -9 -f '$(PROCS)' || true
