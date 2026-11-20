package aigateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// GetByKey：GET /v1/calls?idempotency_key=…（只查不建），scope=calls；404 → ErrNotFound；5xx → 暂时性错误。
func TestGetByKey(t *testing.T) {
	status := http.StatusOK
	var method, path, rawQuery, purpose string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, rawQuery, purpose = r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get(HeaderPurpose)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		switch status {
		case http.StatusOK:
			_, _ = io.WriteString(w, callJSON)
		case http.StatusNotFound:
			_, _ = io.WriteString(w, `{"error":{"code":"NOT_FOUND","check":"call.exists","message":"no call with this idempotency key"}}`)
		default:
			_, _ = io.WriteString(w, `{"error":{"code":"unavailable","message":"down"}}`)
		}
	}))
	t.Cleanup(srv.Close)
	c := New(Config{BaseURL: srv.URL, Secret: testSecret, Timeout: 5 * time.Second})

	call, err := c.GetByKey(context.Background(), "canvas:cv_t:0")
	if err != nil || call.ID != "call_01J8Z3K6W9ABCDEF" {
		t.Fatalf("%+v %v", call, err)
	}
	if method != http.MethodGet || path != "/v1/calls" || rawQuery != "idempotency_key=canvas%3Acv_t%3A0" || purpose != PurposeCalls {
		t.Fatalf("route = %s %s ?%s purpose=%s", method, path, rawQuery, purpose)
	}
	status = http.StatusNotFound
	if _, err := c.GetByKey(context.Background(), "canvas:cv_t:0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("404 应为 ErrNotFound: %v", err)
	}
	status = http.StatusServiceUnavailable
	_, err = c.GetByKey(context.Background(), "canvas:cv_t:0")
	var ge *Error
	if errors.Is(err, ErrNotFound) || !errors.As(err, &ge) || !ge.Temporary() {
		t.Fatalf("5xx 应为暂时性错误、不能当不存在: %v", err)
	}
	if _, err := c.GetByKey(context.Background(), "bad key with spaces"); err == nil {
		t.Fatal("非法幂等键应本地拒绝")
	}
}
