package studio

import (
	"context"
	"testing"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/app/studio/rpc/studio_client"
	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/userquota"
	"aigc-local-life/pkg/xerr"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
)

// fakeStudio 只实现 PreviewPose，记录调用次数。
type fakeStudio struct {
	studio_client.Studio
	calls int
}

func (f *fakeStudio) PreviewPose(context.Context, *studio_client.PreviewPoseReq, ...grpc.CallOption) (*studio_client.PreviewPoseResp, error) {
	f.calls++
	return &studio_client.PreviewPoseResp{Source: &studio_client.PosePreviewImage{PersonCount: 1}}, nil
}

// 按用户配额：两个网关实例共用一台 Redis，合计超过每分钟上限后回 4002「识别太频繁，请稍后再试」，不再调 rpc；别的用户不受影响。
func TestPreviewPoseQuota(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	rpc := &fakeStudio{}
	inst := func() *svc.ServiceContext {
		return &svc.ServiceContext{StudioRpc: rpc, PosePreviewQuota: userquota.New("studio:pose_preview", 2, 200, rc)}
	}
	a, b := inst(), inst()
	ctx := ctxdata.WithUserID(context.Background(), 42)
	req := &types.StudioPosePreviewReq{SourceImageUrl: "https://x/s.jpg", PoseImageUrls: []string{"https://x/p.jpg"}}
	for _, s := range []*svc.ServiceContext{a, b} {
		if _, err := NewPreviewPoseLogic(ctx, s).PreviewPose(req); err != nil {
			t.Fatal(err)
		}
	}
	_, err := NewPreviewPoseLogic(ctx, a).PreviewPose(req)
	if code, msg, _ := xerr.Parse(err); code != xerr.Conflict || msg != msgPreviewTooFrequent {
		t.Fatalf("应超限: %v", err)
	}
	if rpc.calls != 2 {
		t.Fatalf("超限不应调 rpc: calls=%d", rpc.calls)
	}
	if _, err := NewPreviewPoseLogic(ctxdata.WithUserID(context.Background(), 43), b).PreviewPose(req); err != nil {
		t.Fatalf("别的用户不受影响: %v", err)
	}
}
