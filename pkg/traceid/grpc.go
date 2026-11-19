package traceid

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// mdKey gRPC metadata 键（小写）。
const mdKey = "x-trace-id"

// UnaryClientInterceptor 出站 gRPC：ctx 里有追踪号就放进 metadata，网关 → 各 rpc 服务同一个号。
func UnaryClientInterceptor(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	if id := From(ctx); id != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, mdKey, id)
	}
	return invoker(ctx, method, req, reply, cc, opts...)
}

// UnaryServerInterceptor 入站 gRPC：metadata 里的追踪号（合法才收）写进 ctx 与日志字段。
func UnaryServerInterceptor(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get(mdKey); len(v) > 0 && Valid(v[0]) {
			ctx = With(ctx, v[0])
		}
	}
	return handler(ctx, req)
}
