package xerr

import (
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestParse(t *testing.T) {
	// 本地错误（含包装）
	code, msg, ok := Parse(fmt.Errorf("wrap: %w", New(NotFound, "音色不存在")))
	if !ok || code != NotFound || msg != "音色不存在" {
		t.Fatalf("local: %d %s %v", code, msg, ok)
	}
	// 经过 gRPC 传输后的错误
	st, _ := status.FromError(New(Conflict, "冲突"))
	code, msg, ok = Parse(st.Err())
	if !ok || code != Conflict || msg != "冲突" {
		t.Fatalf("rpc: %d %s %v", code, msg, ok)
	}
	// 框架错误不是业务错误
	if _, _, ok := Parse(status.Error(codes.Unavailable, "down")); ok {
		t.Fatal("grpc framework error treated as biz error")
	}
	if _, _, ok := Parse(errors.New("plain")); ok {
		t.Fatal("plain error treated as biz error")
	}
}
