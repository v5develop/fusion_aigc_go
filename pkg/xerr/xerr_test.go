package xerr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"
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

// 细分 reason 经真实 gRPC 链路（server → wire → client）保持：码不变，reason 取细分值；无细分按码派生。
func TestReasonOfSurvivesGRPCWire(t *testing.T) {
	lis := bufconn.Listen(1 << 16)
	srv := grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, st grpc.ServerStream) error {
		method, _ := grpc.MethodFromServerStream(st)
		if method == "/t/Detailed" {
			return NewReason(NotFound, ReasonMaskSourceNotFound, "执行记录不存在")
		}
		return New(NotFound, "批次不存在")
	}))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	for method, want := range map[string]string{"/t/Detailed": "MASK_SOURCE_NOT_FOUND", "/t/Plain": "RESOURCE_NOT_FOUND"} {
		err := conn.Invoke(context.Background(), method, &emptypb.Empty{}, &emptypb.Empty{})
		if code, _, ok := Parse(err); !ok || code != NotFound {
			t.Fatalf("%s 码应为 4001: %v", method, err)
		}
		if got := ReasonOf(err); got != want {
			t.Fatalf("%s reason=%q want %q", method, got, want)
		}
	}
	if ReasonOf(errors.New("plain")) != "" {
		t.Fatal("非业务错误不应有 reason")
	}
	if got := ReasonOf(WithReason(New(Conflict, "x"), ReasonMaskSourceNotReady)); got != "MASK_SOURCE_NOT_READY" {
		t.Fatal(got)
	}
}
