// Package aigateway ai-gateway（供应商中立的 AI 调用网关）的 Go 客户端。
//
// 契约以 ai-gateway 仓库为准：contract/hmac.md（签名）、src/ai_gateway/contract/openapi_paths.yaml（路由与 scope）、
// src/ai_gateway/contract/*.py（字段）。本包只建模 Go 侧用到的字段，未知字段一律忽略。
//
// # 身份与 scope
//
// 调用方 caller_id=fusion-go，密钥只有 calls、catalog 两个 scope：
//
//	POST /v1/calls                 calls    Submit（Idempotency-Key 头；query: wait、retry_failed、reconciled_not_charged）
//	GET  /v1/calls/{id}            calls    Get（query: wait ≤ 60 秒长轮询）
//	POST /v1/calls/{id}/cancel     calls    Cancel
//	POST /v1/calls:batchGet        calls    BatchGet（≤ 200）
//	GET  /v1/catalog/model-groups  catalog  ModelGroups
//	POST /v1/catalog/readiness     catalog  Readiness
//	GET  /v1/ledger/costs          ledger   Costs（fusion-go 无此 scope，会 403，接口先保留）
//
// 每个请求带七个签名头 X-Aig-Version / Caller / Key-Id / Purpose / Timestamp / Nonce / Signature，
// Purpose 必须等于路由的 x-aig-scope。签名算法见 Signer。
//
// # 配置
//
// etc yaml：
//
//	AiGateway:
//	  BaseURL: http://ai-gateway:8080
//	  KeyID: fusion-go-v1      # 默认 fusion-go-v1
//	  Secret: <HMAC 密钥>
//	  # CallerID: fusion-go    # 默认
//	  # Timeout: 10s           # 单次请求超时，长轮询另加 wait
//	  # LongPollWait: 25s      # 传 UseDefaultWait 时的等待时长
//
// BaseURL 为空时客户端处于未接入状态，所有调用返回 ErrNotConfigured。
//
// # 使用约定
//
//   - 提交按 (caller_id, Idempotency-Key) 幂等：超时或网络错误后用同一个键原样重提即可拿回同一条记录；
//     409 IDEMPOTENCY_CONFLICT 表示同键不同内容，是调用方 bug，绝不能重试。
//   - 网关从不回调：用 Get 长轮询（每次状态变化立即唤醒）或 BatchGet 比对 updated_seq 观察完成。
//   - 本包不自动重试，*Error 的 Temporary / RetrySafe 供上层编排判断。
//   - 产物是媒体服务资源，持久引用为 mr://{rid}（MediaOut.Ref）；url 只是 24 小时签名下载地址。
//     需要落到业务桶时，由调用方经媒体服务 resources/copy 复制（本包不做）。
package aigateway
