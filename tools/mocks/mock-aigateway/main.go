// mock-aigateway 模拟 ai-gateway（调用方 fusion-go），仅开发/测试用。端口默认 5059（5058 已被 mock-linkparse 占用）。
//
//	POST /v1/calls                  （Idempotency-Key；同键同内容返回同一条，同键不同内容 409；
//	                                  「内容」不含 labels、constraints.deadline_at / max_attempts，同网关 request_hash）
//	GET  /v1/calls/{id}[?wait=N]    每次读取推进一步 PENDING→RUNNING→SUCCEEDED；带 wait 直接到终态
//	POST /v1/calls/{id}/cancel      PENDING 变 CANCELLED；其它状态原样返回
//	POST /v1/calls:batchGet         {"ids":[...]} → {"items":[...],"missing":[...]}（每条同样推进一步）
//	GET  /v1/catalog/model-groups   img.gpt-image-2 / img.nano-banana-2.1 / img.seedream-5-pro
//	POST /v1/catalog/readiness      请求里的模型组全部 ready
//	GET  /img/{n}.png               产物图（64×64 PNG，不验签）
//
// operation=chat.completion 成功时返回 message.content = {"prompt":"（mock 润色）<最后一条 user 文本>"}。
//
// 产物落媒体服务（可选）：设 MOCK_AIG_MEDIA_URL（如 http://127.0.0.1:18090）与 MOCK_AIG_MEDIA_TOKEN 后，
// 成功时把 /img/{n}.png 经媒体服务 /v1/mediasvc/ingest（purpose=intermediate）落桶，outputs[].media.rid 用真实 rid，
// 调用方随后可以 resources/copy 转存。不设则 rid 为 mock-{n}（媒体服务不认，转存会失败）。
//
// 七个 X-Aig-* 头按 contract/hmac.md 真实验签（复用 pkg/aigateway 的 CanonicalString / Signature）：
// caller 必须是 fusion-go，Purpose 必须等于路由 scope（calls / catalog），密钥取环境变量 MOCK_AIG_SECRET（默认 test-secret）。
//
// 可控开关：
//
//	请求 labels.fail=1      → 该调用到终态时是 FAILED（failure.class=VENDOR_FAILED, code=mock_failed）
//	GET /debug/calls        → 当前所有调用（便于 e2e 断言）；?reset=1 清空
//	POST /debug/inject      → 按请求体内容注入结果（调用方的 labels 由服务端自己拼，e2e 改不了，所以按请求体子串匹配，
//	                          如提示词里放一个标记）。body：{"match":"子串","outcome":"FAILED|UNKNOWN|HOLD|SUBMIT_503","class":"…","times":N}
//	                            FAILED      到终态时 FAILED；class 缺省 VENDOR_FAILED（retry_safe=true，调用方会自动重试），
//	                                        给 INPUT_REJECTED 之类不可重试的类别则 retry_safe=false，一次就失败
//	                            UNKNOWN     到终态时 UNKNOWN（money_at_risk=true），调用方应按「待对账」处理、不退款
//	                            HOLD        一直停在 RUNNING（带 wait 也不终态），cancel 可把它变 CANCELLED
//	                            SUBMIT_503  提交直接回 503（不建调用），调用方应当作暂时失败留在排队
//	                          times>0 命中这么多个**新**调用后失效（同幂等键重提拿回的是原调用，不重复计数），≤0 不限次。
//	GET /debug/inject       → 当前规则；?reset=1 清空
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"aigc-local-life/pkg/aigateway"
)

const (
	callerID = "fusion-go"
	maxSkew  = 300
)

type rec struct {
	call     map[string]any
	bodyHash string
	n        int
	fail     bool
	class    string // FAILED 的 failure.class（空 = VENDOR_FAILED）
	outcome  string // 注入的结果：UNKNOWN / HOLD（FAILED 用 fail 表示）
	status   string
	rid      string // 落媒体服务后的 rid
	text     string // chat.completion 最后一条 user 文本
}

type server struct {
	mu     sync.Mutex
	secret string
	byKey  map[string]*rec
	byID   map[string]*rec
	seq    int
	rules  []*rule
}

// rule 一条注入规则（POST /debug/inject）。
type rule struct {
	Match   string `json:"match"`
	Outcome string `json:"outcome"`
	Class   string `json:"class,omitempty"`
	Times   int    `json:"times"`
	Hits    int    `json:"hits"`
}

// pick 新调用命中的第一条规则（调用方须持锁）；命中计数，次数用完的规则移除。
func (s *server) pick(body []byte) *rule {
	for i, r := range s.rules {
		if r.Match == "" || !strings.Contains(string(body), r.Match) {
			continue
		}
		r.Hits++
		if r.Times > 0 && r.Hits >= r.Times {
			s.rules = append(s.rules[:i:i], s.rules[i+1:]...)
		}
		return r
	}
	return nil
}

func newServer() *server {
	secret := os.Getenv("MOCK_AIG_SECRET")
	if secret == "" {
		secret = "test-secret"
	}
	return &server{secret: secret, byKey: map[string]*rec{}, byID: map[string]*rec{}}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, status int, code, check, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "check": check, "message": msg}})
}

// verify 按 hmac.md 的验证顺序检查；通过返回 body 原始字节。
func (s *server) verify(w http.ResponseWriter, r *http.Request, scope string) ([]byte, bool) {
	h := r.Header
	for _, k := range []string{aigateway.HeaderVersion, aigateway.HeaderCaller, aigateway.HeaderKeyID, aigateway.HeaderPurpose,
		aigateway.HeaderTimestamp, aigateway.HeaderNonce, aigateway.HeaderSignature} {
		if h.Get(k) == "" {
			errJSON(w, 401, aigateway.CodeAuthFailed, "hmac.missing_field", "missing "+k)
			return nil, false
		}
	}
	if h.Get(aigateway.HeaderVersion) != aigateway.SignVersion {
		errJSON(w, 401, aigateway.CodeAuthFailed, "hmac.unsupported_version", "")
		return nil, false
	}
	if h.Get(aigateway.HeaderCaller) != callerID {
		errJSON(w, 401, aigateway.CodeAuthFailed, "hmac.caller_mismatch", "caller must be "+callerID)
		return nil, false
	}
	purpose := h.Get(aigateway.HeaderPurpose)
	if purpose != scope {
		errJSON(w, 401, aigateway.CodeAuthFailed, "hmac.purpose_mismatch", "route scope is "+scope)
		return nil, false
	}
	ts, err := strconv.ParseInt(h.Get(aigateway.HeaderTimestamp), 10, 64)
	if err != nil {
		errJSON(w, 401, aigateway.CodeAuthFailed, "hmac.invalid_timestamp", "")
		return nil, false
	}
	if d := time.Now().Unix() - ts; d > maxSkew || d < -maxSkew {
		errJSON(w, 401, aigateway.CodeAuthFailed, "hmac.timestamp_out_of_window", "")
		return nil, false
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	path, err := aigateway.NormalizePath(r.URL.Path)
	if err != nil {
		errJSON(w, 401, aigateway.CodeAuthFailed, "hmac.invalid_format", err.Error())
		return nil, false
	}
	canonical := aigateway.CanonicalString(callerID, purpose, r.Method, path, aigateway.CanonicalQuery(r.URL.RawQuery), ts, h.Get(aigateway.HeaderNonce), body)
	want := aigateway.Signature(s.secret, canonical)
	if !hmac.Equal([]byte(want), []byte(strings.ToLower(h.Get(aigateway.HeaderSignature)))) {
		errJSON(w, 401, aigateway.CodeAuthFailed, "hmac.invalid_signature", "")
		return nil, false
	}
	return body, true
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/calls", s.submit)
	mux.HandleFunc("/v1/calls:batchGet", s.batchGet)
	mux.HandleFunc("/v1/calls/", s.callByID)
	mux.HandleFunc("/v1/catalog/model-groups", s.modelGroups)
	mux.HandleFunc("/v1/catalog/readiness", s.readiness)
	mux.HandleFunc("/img/", s.img)
	mux.HandleFunc("/debug/calls", s.debug)
	mux.HandleFunc("/debug/inject", s.inject)
	return mux
}

func (s *server) submit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errJSON(w, 405, aigateway.CodeValidationFailed, "http.method", "")
		return
	}
	body, ok := s.verify(w, r, aigateway.PurposeCalls)
	if !ok {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		errJSON(w, 400, aigateway.CodeValidationFailed, "aig.idempotency_key", "Idempotency-Key required")
		return
	}
	var req struct {
		Operation  string            `json:"operation"`
		ModelGroup string            `json:"model_group"`
		Labels     map[string]string `json:"labels"`
		Input      struct {
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
		} `json:"input"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Operation == "" || req.ModelGroup == "" {
		errJSON(w, 422, aigateway.CodeValidationFailed, "body.invalid", "operation / model_group required")
		return
	}
	hash := requestHash(body)

	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.byKey[key]; ok {
		if old.bodyHash != hash {
			errJSON(w, 409, aigateway.CodeIdempotencyConflict, "aig.idempotency_conflict", "same key, different request")
			return
		}
		writeJSON(w, 200, s.render(r, old, false))
		return
	}
	ru := s.pick(body)
	if ru != nil && ru.Outcome == "SUBMIT_503" {
		errJSON(w, 503, "UPSTREAM_UNAVAILABLE", "mock.inject", "mock: injected 503")
		return
	}
	s.seq++
	id := fmt.Sprintf("call_mock%06d", s.seq)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	rc := &rec{n: s.seq, bodyHash: hash, fail: req.Labels["fail"] == "1", status: aigateway.StatusPending}
	if ru != nil {
		switch ru.Outcome {
		case aigateway.StatusFailed:
			rc.fail, rc.class = true, ru.Class
		default:
			rc.outcome = ru.Outcome
		}
	}
	for _, m := range req.Input.Messages {
		if m.Role == "user" {
			for _, c := range m.Content {
				if c.Type == "text" {
					rc.text = c.Text
				}
			}
		}
	}
	rc.call = map[string]any{
		"id": id, "idempotency_key": key, "caller_id": callerID, "request_hash": hash,
		"operation": req.Operation, "model_group": req.ModelGroup, "run_no": 1, "updated_seq": 1,
		"attempts": []any{}, "retry_safe": false, "money_at_risk": false, "labels": req.Labels,
		"created_at": now, "updated_at": now,
	}
	s.byKey[key], s.byID[id] = rc, rc
	_, hasWait := r.URL.Query()["wait"]
	writeJSON(w, 200, s.render(r, rc, hasWait))
}

// requestHash 与网关 request_hash 同口径：labels、constraints.deadline_at / max_attempts、input.stream 不参与，
// 所以同幂等键重提时换了截止时间不会 409。
func requestHash(body []byte) string {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		sum := sha256.Sum256(body)
		return hex.EncodeToString(sum[:])
	}
	delete(m, "labels")
	if c, ok := m["constraints"].(map[string]any); ok {
		delete(c, "deadline_at")
		delete(c, "max_attempts")
		if len(c) == 0 {
			delete(m, "constraints")
		}
	}
	if in, ok := m["input"].(map[string]any); ok {
		delete(in, "stream")
	}
	b, _ := json.Marshal(m) // map 键有序，结果稳定
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// render 推进一步（advance=true 直接到终态）并生成响应。调用方须持锁。
func (s *server) render(r *http.Request, rc *rec, toTerminal bool) map[string]any {
	switch {
	case isTerminal(rc.status):
	case rc.outcome == "HOLD": // 一直生成中，只能被 cancel
		rc.status = aigateway.StatusRunning
	case toTerminal || rc.status == aigateway.StatusRunning:
		rc.status = aigateway.StatusSucceeded
		if rc.fail {
			rc.status = aigateway.StatusFailed
		}
		if rc.outcome == aigateway.StatusUnknown {
			rc.status = aigateway.StatusUnknown
		}
	case rc.status == aigateway.StatusPending && rc.call["_polled"] != nil:
		rc.status = aigateway.StatusRunning
	}
	rc.call["_polled"] = true
	out := map[string]any{}
	for k, v := range rc.call {
		if k != "_polled" {
			out[k] = v
		}
	}
	out["status"] = rc.status
	if rc.status == aigateway.StatusRunning {
		out["phase"] = "POLLING"
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	out["updated_at"] = now
	switch rc.status {
	case aigateway.StatusSucceeded:
		out["completed_at"] = now
		out["cost"] = map[string]any{"amount": "0.10", "currency": "CNY", "amount_cny": "0.10", "source": "configured", "final": true}
		if rc.call["operation"] == "chat.completion" {
			content, _ := json.Marshal(map[string]string{"prompt": "（mock 润色）" + rc.text})
			out["result"] = map[string]any{"operation": "chat.completion", "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": string(content)}}
			break
		}
		rid := fmt.Sprintf("mock-%d", rc.n)
		if rc.rid == "" {
			rc.rid = s.ingest(fmt.Sprintf("http://%s/img/%d.png", r.Host, rc.n))
		}
		if rc.rid != "" {
			rid = rc.rid
		}
		if rc.call["operation"] == "video.generate" { // 画布视频节点：video + cover（mock 视频地址也是那张 PNG）
			m := map[string]any{"rid": rid, "url": fmt.Sprintf("http://%s/img/%d.png", r.Host, rc.n),
				"url_expires_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
				"sha256":         fmt.Sprintf("%064x", rc.n), "bytes": 200, "mime": "image/png", "width": 64, "height": 64}
			out["result"] = map[string]any{"operation": "video.generate", "video": m, "cover": m}
			break
		}
		out["result"] = map[string]any{"operation": rc.call["operation"], "outputs": []any{map[string]any{
			"index": 0, "status": "SUCCEEDED",
			"media": map[string]any{
				"rid": rid, "aig_key": rid,
				"url":            fmt.Sprintf("http://%s/img/%d.png", r.Host, rc.n),
				"url_expires_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
				"sha256":         fmt.Sprintf("%064x", rc.n), "bytes": 200, "mime": "image/png", "width": 64, "height": 64,
			}}}}
	case aigateway.StatusFailed:
		out["completed_at"] = now
		class := rc.class
		if class == "" {
			class = "VENDOR_FAILED"
		}
		out["failure"] = map[string]any{"class": class, "code": "mock_failed", "message": "mock: injected failure"}
		out["retry_safe"] = class == "VENDOR_FAILED"
	case aigateway.StatusUnknown:
		out["completed_at"] = now
		out["failure"] = map[string]any{"class": "UNKNOWN_OUTCOME", "code": "mock_unknown", "message": "mock: injected unknown"}
		out["money_at_risk"] = true
	case aigateway.StatusCancelled:
		out["completed_at"] = now
		out["retry_safe"] = true
	}
	return out
}

func isTerminal(st string) bool {
	switch st {
	case aigateway.StatusSucceeded, aigateway.StatusFailed, aigateway.StatusUnknown, aigateway.StatusCancelled:
		return true
	}
	return false
}

func (s *server) callByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/calls/")
	id, action, _ := strings.Cut(rest, "/")
	if _, ok := s.verify(w, r, aigateway.PurposeCalls); !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rc, ok := s.byID[id]
	if !ok {
		errJSON(w, 404, aigateway.CodeNotFound, "call.not_found", id)
		return
	}
	switch {
	case action == "" && r.Method == http.MethodGet:
		_, hasWait := r.URL.Query()["wait"]
		writeJSON(w, 200, s.render(r, rc, hasWait))
	case action == "cancel" && r.Method == http.MethodPost:
		if rc.status == aigateway.StatusPending || (rc.outcome == "HOLD" && rc.status == aigateway.StatusRunning) {
			rc.status = aigateway.StatusCancelled
		}
		writeJSON(w, 200, s.render(r, rc, false))
	default:
		errJSON(w, 404, aigateway.CodeNotFound, "route.not_found", r.URL.Path)
	}
}

func (s *server) batchGet(w http.ResponseWriter, r *http.Request) {
	body, ok := s.verify(w, r, aigateway.PurposeCalls)
	if !ok {
		return
	}
	var req struct {
		IDs []string `json:"ids"`
	}
	if json.Unmarshal(body, &req) != nil {
		errJSON(w, 422, aigateway.CodeValidationFailed, "body.invalid", "")
		return
	}
	if len(req.IDs) > aigateway.MaxBatchGet {
		errJSON(w, 413, aigateway.CodeBatchTooLarge, "aig.batch_get_size", "")
		return
	}
	items, missing := []any{}, []string{}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range req.IDs {
		if rc, ok := s.byID[id]; ok {
			items = append(items, s.render(r, rc, false))
		} else {
			missing = append(missing, id)
		}
	}
	writeJSON(w, 200, map[string]any{"items": items, "missing": missing})
}

func (s *server) modelGroups(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.verify(w, r, aigateway.PurposeCatalog); !ok {
		return
	}
	mg := func(code, name string) map[string]any {
		return map[string]any{"code": code, "operations": []string{"image.generate", "image.edit"}, "display_name": name, "enabled": true,
			"feature_limits": map[string]any{"max_n": 4, "max_reference_images": 10, "resolutions": []string{"1k", "2k", "4k"}}, "default_timeout_s": 300}
	}
	// 画布用到的视频 / 视觉对话组（seedance-2.5 不支持首尾帧，与真实审计一致；enabled=false 的 mini 用来测过滤）
	vid := func(code, name string, enabled bool) map[string]any {
		return map[string]any{"code": code, "operations": []string{"video.generate"}, "display_name": name, "enabled": enabled,
			"feature_limits": map[string]any{"max_reference_images": 3, "max_reference_videos": 1, "resolutions": []string{"480p", "720p", "1080p"}}, "default_timeout_s": 900}
	}
	vlm := map[string]any{"code": "vlm.qwen-3.8-flash", "operations": []string{"chat.completion"}, "display_name": "Qwen VL", "enabled": true,
		"feature_limits": map[string]any{"supports_vision": true}}
	writeJSON(w, 200, map[string]any{"items": []any{
		mg("img.gpt-image-2", "GPT Image 2"), mg("img.nano-banana-2.1", "Nano Banana 2.1"), mg("img.seedream-5-pro", "Seedream 5 Pro"),
		vid("vid.seedance-2.5", "Seedance 2.5", true), vid("vid.seedance-2", "Seedance 2.0", true), vid("vid.seedance-2-fast", "Seedance 2.0 Fast", true),
		vid("vid.seedance-2-mini", "Seedance 2.0 Mini", false), vlm}})
}

func (s *server) readiness(w http.ResponseWriter, r *http.Request) {
	body, ok := s.verify(w, r, aigateway.PurposeCatalog)
	if !ok {
		return
	}
	var req struct {
		Items []struct {
			ModelGroup string `json:"model_group"`
		} `json:"items"`
	}
	_ = json.Unmarshal(body, &req)
	items := []any{}
	for _, it := range req.Items {
		items = append(items, map[string]any{"model_group": it.ModelGroup, "ready": true, "enabled_deployments": 1, "available_deployments": 1})
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

// ingest 把产物图落到媒体服务（MOCK_AIG_MEDIA_URL），返回 rid；未配置或失败返回空串。
func (s *server) ingest(imgURL string) string {
	base := strings.TrimRight(os.Getenv("MOCK_AIG_MEDIA_URL"), "/")
	if base == "" {
		return ""
	}
	body, _ := json.Marshal(map[string]any{"source": map[string]string{"url": imgURL}, "purpose": "intermediate"})
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/mediasvc/ingest", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Media-Token", os.Getenv("MOCK_AIG_MEDIA_TOKEN"))
	req.Header.Set("Idempotency-Key", "mock-aig-"+imgURL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("mock-aigateway: 落媒体服务失败: %v", err)
		return ""
	}
	defer resp.Body.Close()
	var out struct {
		Object struct {
			RID string `json:"rid"`
		} `json:"object"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&out) != nil {
		log.Printf("mock-aigateway: 落媒体服务返回 %d", resp.StatusCode)
		return ""
	}
	return out.Object.RID
}

func (s *server) img(w http.ResponseWriter, r *http.Request) {
	im := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			im.Set(x, y, color.RGBA{uint8(x * 4), uint8(y * 4), 160, 255})
		}
	}
	w.Header().Set("Content-Type", "image/png")
	_ = png.Encode(w, im)
}

func (s *server) debug(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := []any{}
	for _, rc := range s.byID {
		list = append(list, map[string]any{"id": rc.call["id"], "idempotency_key": rc.call["idempotency_key"], "status": rc.status, "model_group": rc.call["model_group"]})
	}
	if r.URL.Query().Get("reset") != "" {
		s.byKey, s.byID, s.seq = map[string]*rec{}, map[string]*rec{}, 0
	}
	writeJSON(w, 200, map[string]any{"calls": list})
}

func (s *server) inject(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Method == http.MethodPost {
		var ru rule
		if err := json.NewDecoder(r.Body).Decode(&ru); err != nil || ru.Match == "" {
			errJSON(w, 422, aigateway.CodeValidationFailed, "body.invalid", "match required")
			return
		}
		switch ru.Outcome {
		case aigateway.StatusFailed, aigateway.StatusUnknown, "HOLD", "SUBMIT_503":
		default:
			errJSON(w, 422, aigateway.CodeValidationFailed, "body.invalid", "outcome: FAILED|UNKNOWN|HOLD|SUBMIT_503")
			return
		}
		ru.Hits = 0
		s.rules = append(s.rules, &ru)
	}
	if r.URL.Query().Get("reset") != "" {
		s.rules = nil
	}
	writeJSON(w, 200, map[string]any{"rules": s.rules})
}

func main() {
	addr := flag.String("addr", ":5059", "listen address")
	flag.Parse()
	log.Printf("mock-aigateway listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, newServer().handler()))
}
