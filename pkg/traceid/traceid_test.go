package traceid

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

func TestMiddlewareKeepsValidAndGeneratesOtherwise(t *testing.T) {
	var seen string
	h := Middleware(func(w http.ResponseWriter, r *http.Request) { seen = From(r.Context()) })

	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set(Header, "AiCreation_123")
	rec := httptest.NewRecorder()
	h(rec, req)
	if seen != "AiCreation_123" || rec.Header().Get(Header) != "AiCreation_123" {
		t.Fatalf("合法号应沿用：ctx=%q resp=%q", seen, rec.Header().Get(Header))
	}

	for _, bad := range []string{"", "a b", "x\ny", string(make([]byte, 200))} {
		req := httptest.NewRequest("GET", "/x", nil)
		if bad != "" {
			req.Header.Set(Header, bad)
		}
		rec := httptest.NewRecorder()
		h(rec, req)
		if len(seen) != 32 || rec.Header().Get(Header) != seen {
			t.Fatalf("非法/缺失应生成新号：bad=%q ctx=%q resp=%q", bad, seen, rec.Header().Get(Header))
		}
	}
}

func TestInject(t *testing.T) {
	ctx := With(httptest.NewRequest("GET", "/", nil).Context(), "t-1")
	h := http.Header{}
	Inject(ctx, h)
	if h.Get(Header) != "t-1" {
		t.Fatal("应从 ctx 注入")
	}
	h2 := http.Header{Header: {"explicit"}}
	Inject(ctx, h2)
	if h2.Get(Header) != "explicit" {
		t.Fatal("显式头优先")
	}
	h3 := http.Header{}
	Inject(httptest.NewRequest("GET", "/", nil).Context(), h3)
	if len(h3) != 0 {
		t.Fatal("ctx 无号不注入")
	}
}

func TestGRPCRoundTrip(t *testing.T) {
	out := With(context.Background(), "tr-9")
	var md metadata.MD
	_ = UnaryClientInterceptor(out, "/x", nil, nil, nil, func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		md, _ = metadata.FromOutgoingContext(ctx)
		return nil
	})
	in := metadata.NewIncomingContext(context.Background(), md)
	var got string
	_, _ = UnaryServerInterceptor(in, nil, nil, func(ctx context.Context, _ any) (any, error) { got = From(ctx); return nil, nil })
	if got != "tr-9" {
		t.Fatalf("got %q", got)
	}
}
