package fusion

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

const (
	appKey    = "12345678910"
	appSecret = "s3cr3t"
)

// verify 站在对方（Java）的角度独立验签：按文档自己拼一遍待签串，
// 而不是复用被测代码的 stringToSign，否则拼错了两边一起错也测不出来。
func verify(t *testing.T, r *http.Request, body []byte) {
	t.Helper()
	ts := r.Header.Get("X-Union-Timestamp")
	nonce := r.Header.Get("X-Union-Nonce")
	if r.Header.Get("X-Union-App-Key") != appKey {
		t.Errorf("appKey 头不对: %q", r.Header.Get("X-Union-App-Key"))
	}
	if len(nonce) != 32 {
		t.Errorf("nonce 应该是 32 个字符: %q", nonce)
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || time.Since(time.Unix(sec, 0)) > 5*time.Minute {
		t.Errorf("时间戳不在 5 分钟窗口内: %q", ts)
	}
	bodyHash := sha256.Sum256(body)
	src := r.Method + "\n" + r.URL.Path + "\n" + r.URL.RawQuery + "\n" +
		hex.EncodeToString(bodyHash[:]) + "\n" + ts + "\n" + nonce + "\n" + appKey
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write([]byte(src))
	if want := hex.EncodeToString(mac.Sum(nil)); r.Header.Get("X-Union-Signature") != want {
		t.Errorf("签名对不上\n待签串: %q\n期望: %s\n实际: %s", src, want, r.Header.Get("X-Union-Signature"))
	}
}

func TestUserRegister(t *testing.T) {
	var got RegisterReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		verify(t, r, body)
		if r.URL.Path != "/api/v1/union/user-register" || r.Method != http.MethodPost {
			t.Errorf("路径或方法不对: %s %s", r.Method, r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json;charset=utf-8" {
			t.Errorf("Content-Type: %q", ct)
		}
		_ = json.Unmarshal(body, &got)
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"userId":25,"nickname":"小林"}}`))
	}))
	defer srv.Close()

	c := New(Conf{BaseURL: srv.URL, AppKey: appKey, AppSecret: appSecret})
	id, err := c.UserRegister(context.Background(), RegisterReq{
		UnionUserID: "999000000000000042", Nickname: "小林", Avatar: "https://x/a.png",
		Gender: 1, Roles: []string{"consumer"}, Status: "active",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != 25 {
		t.Fatalf("userId = %d，应该是 25", id)
	}
	if got.UnionUserID != "999000000000000042" || got.Gender != 1 || len(got.Roles) != 1 {
		t.Fatalf("入参没送对: %+v", got)
	}
}

// GET 的参数要进待签串（canonicalQuery），漏了对方一律 401
func TestUserExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		verify(t, r, body)
		if r.URL.Query().Get("unionUserId") != "206757090755100672" {
			t.Errorf("query 没带上: %q", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":true}`))
	}))
	defer srv.Close()

	ok, err := New(Conf{BaseURL: srv.URL, AppKey: appKey, AppSecret: appSecret}).
		UserExists(context.Background(), "206757090755100672")
	if err != nil || !ok {
		t.Fatalf("exists = %v, err = %v", ok, err)
	}
}

// 409 是并发撞车：人已经建好了，不能当失败
func TestRegisterDuplicate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":4003,"msg":"用户已存在"}`))
	}))
	defer srv.Close()

	_, err := New(Conf{BaseURL: srv.URL, AppKey: appKey, AppSecret: appSecret}).
		UserRegister(context.Background(), RegisterReq{UnionUserID: "1"})
	if !IsDuplicate(err) {
		t.Fatalf("409 应该判成撞车: %v", err)
	}
	var api *APIError
	if !errorsAs(err, &api) || api.Permanent() {
		t.Fatalf("409 不该是永久失败: %v", err)
	}
}

func TestErrors(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		permanent bool
	}{
		{"签名错", http.StatusUnauthorized, `{"code":4001,"msg":"签名错误"}`, true},
		{"参数不合法", http.StatusBadRequest, `{"code":1001,"msg":"unionUserId 不能为空"}`, true},
		{"限流", http.StatusTooManyRequests, `{"code":429,"msg":"慢点"}`, false},
		{"对端异常", http.StatusInternalServerError, `{"code":500,"msg":"boom"}`, false},
		{"业务拒绝", http.StatusOK, `{"code":2007,"msg":"账号已禁用"}`, false},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		_, err := New(Conf{BaseURL: srv.URL, AppKey: appKey, AppSecret: appSecret}).
			UserRegister(context.Background(), RegisterReq{UnionUserID: "1"})
		srv.Close()
		var api *APIError
		if !errorsAs(err, &api) {
			t.Errorf("%s: 应该是 APIError，实际 %v", tc.name, err)
			continue
		}
		if api.Permanent() != tc.permanent {
			t.Errorf("%s: Permanent = %v，应该是 %v", tc.name, api.Permanent(), tc.permanent)
		}
	}
}

func TestEnabled(t *testing.T) {
	for _, c := range []Conf{
		{},
		{BaseURL: "http://x"},
		{BaseURL: "http://x", AppKey: appKey},
		{AppKey: appKey, AppSecret: appSecret},
	} {
		if New(c).Enabled() {
			t.Errorf("缺东西不该启用: %+v", c)
		}
	}
	if !New(Conf{BaseURL: "http://x", AppKey: appKey, AppSecret: appSecret}).Enabled() {
		t.Error("三样齐了应该启用")
	}
}

func errorsAs(err error, target **APIError) bool {
	for err != nil {
		if a, ok := err.(*APIError); ok {
			*target = a
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
