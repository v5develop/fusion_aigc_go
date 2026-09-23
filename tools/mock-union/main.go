// mock-union 本地模拟联盟（xxlm）的用户服务，仅开发/测试用，接口与 mall_user_svc_api 一致：
//
//	GET /api/v1/user/auth/token        带 Authorization: Bearer <联盟token> → 临时 token（5 分钟、用一次就失效）
//	GET /api/v1/auth/{临时token}/user   → 用户信息（手机号脱敏）
//
// 联盟 token 里带什么用户由 token 本身决定：token 形如 "u<数字>" 时就是该用户，
// 其他值按固定用户返回；token 为 "bad" 时一律当登录失效。
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type tempToken struct {
	userID  string
	expires time.Time
}

var (
	mu     sync.Mutex
	tokens = map[string]tempToken{}
)

func main() {
	addr := flag.String("addr", ":5056", "listen address")
	flag.Parse()
	http.HandleFunc("/api/v1/user/auth/token", authToken)
	http.HandleFunc("/api/v1/auth/", userInfo)
	log.Printf("mock-union listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func authToken(w http.ResponseWriter, r *http.Request) {
	bearer := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if bearer == "" || bearer == "bad" {
		writeJSON(w, 200, map[string]any{"code": 401, "message": "token 无效或已过期"})
		return
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	t := hex.EncodeToString(b)

	// 默认用一个明显是假的 ID：真实的联盟 ID 会命中库里已有的真人，
	// 登录会把昵称/头像同步成 mock 的，别污染真实数据
	userID := "999000000000000001"
	if strings.HasPrefix(bearer, "u") && len(bearer) > 1 {
		userID = bearer[1:]
	}
	mu.Lock()
	tokens[t] = tempToken{userID: userID, expires: time.Now().Add(5 * time.Minute)}
	mu.Unlock()
	writeJSON(w, 200, map[string]any{"code": 1, "message": "ok", "data": map[string]any{"tempToken": t}})
}

func userInfo(w http.ResponseWriter, r *http.Request) {
	// /api/v1/auth/{token}/user
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/auth/")
	t := strings.TrimSuffix(path, "/user")
	mu.Lock()
	rec, ok := tokens[t]
	delete(tokens, t) // 真实行为：用一次就删
	mu.Unlock()
	if !ok || time.Now().After(rec.expires) {
		writeJSON(w, 200, map[string]any{"code": 401, "message": "临时 token 不存在或已过期"})
		return
	}
	writeJSON(w, 200, map[string]any{"code": 1, "message": "ok", "data": map[string]any{
		"userId":         jsonNumber(rec.userID),
		"phone":          "130****7881",
		"nickname":       "mock 用户" + rec.userID[len(rec.userID)-4:],
		"avatar":         "https://mock-oss.example.com/avatar.png",
		"gender":         1,
		"birthday":       "1998-01-01",
		"province":       "浙江省",
		"city":           "杭州市",
		"realNameStatus": "approved",
		"status":         "active",
		"createdAt":      time.Now().Format(time.DateTime),
		"roles":          []string{"consumer"},
	}})
}

// jsonNumber 让 userId 以数字形式输出（联盟那边是 uint64）
func jsonNumber(s string) json.RawMessage { return json.RawMessage(s) }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		fmt.Println("mock-union: write:", err)
	}
}
