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

// fakeMaskStudio 只实现 SegmentMask，记录入参。
type fakeMaskStudio struct {
	studio_client.Studio
	got []*studio_client.SegmentMaskReq
	// miss 之后的调用都算未命中缓存（识别服务真调用）
	miss bool
}

func (f *fakeMaskStudio) SegmentMask(_ context.Context, in *studio_client.SegmentMaskReq, _ ...grpc.CallOption) (*studio_client.SegmentMaskResp, error) {
	f.got = append(f.got, in)
	resp := &studio_client.SegmentMaskResp{MaskUrl: "https://cdn/m.png", Target: in.Target, Cached: len(f.got) > 1 && !f.miss}
	if in.Target == "garment" {
		resp.AvailableParts = []string{"tops", "coat", "pants", "skirt", "shoes", "bag", "hat"}
	}
	for _, p := range in.Parts {
		resp.Parts = append(resp.Parts, &studio_client.MaskPart{Part: p, MaskUrl: "https://cdn/" + p + ".png"})
	}
	return resp, nil
}

// 部位：parts 透传给 rpc，逐件蒙版 / 可选部位原样回；非服装区域两者都是空数组（不是 null）。
func TestSegmentMaskParts(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	rpc := &fakeMaskStudio{}
	s := &svc.ServiceContext{StudioRpc: rpc, MaskQuota: userquota.New("studio:mask", 10, 500, rc)}
	ctx := ctxdata.WithUserID(context.Background(), 42)
	resp, err := NewSegmentMaskLogic(ctx, s).SegmentMask(&types.StudioMaskReq{ImageUrl: "https://cdn/r.png", Target: "garment",
		Parts: []string{"tops", "pants"}})
	if err != nil || len(resp.Parts) != 2 || resp.Parts[1] != (types.StudioMaskPart{Part: "pants", MaskUrl: "https://cdn/pants.png"}) ||
		len(resp.AvailableParts) != 7 {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if got := rpc.got[0].Parts; len(got) != 2 || got[0] != "tops" {
		t.Fatalf("parts 透传 %v", got)
	}
	resp, err = NewSegmentMaskLogic(ctx, s).SegmentMask(&types.StudioMaskReq{ImageUrl: "https://cdn/r.png", Target: "person"})
	if err != nil || resp.Parts == nil || resp.AvailableParts == nil || len(resp.Parts)+len(resp.AvailableParts) != 0 {
		t.Fatalf("非服装 resp=%+v err=%v", resp, err)
	}
}

// 区域蒙版：透传用户与入参；按用户配额超限回 4002「识别太频繁，请稍后再试」且不调 rpc。
func TestSegmentMaskPassThroughAndQuota(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	rpc := &fakeMaskStudio{}
	s := &svc.ServiceContext{StudioRpc: rpc, MaskQuota: userquota.New("studio:mask", 2, 500, rc)}
	ctx := ctxdata.WithUserID(context.Background(), 42)
	req := &types.StudioMaskReq{ImageUrl: "https://cdn/r.png", Target: "garment"}
	resp, err := NewSegmentMaskLogic(ctx, s).SegmentMask(req)
	if err != nil || resp.MaskUrl != "https://cdn/m.png" || resp.Target != "garment" || resp.Cached {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if g := rpc.got[0]; g.UserId != 42 || g.ImageUrl != req.ImageUrl || g.Target != "garment" {
		t.Fatalf("rpc 入参 %+v", g)
	}
	if resp, _ = NewSegmentMaskLogic(ctx, s).SegmentMask(req); !resp.Cached {
		t.Fatal("cached 应透传")
	}
	// 命中缓存不占额度：额度 2、已经 1 次未命中 + 1 次命中，所以再来一次未命中仍放行，之后才超限
	rpc.miss = true
	if resp, err = NewSegmentMaskLogic(ctx, s).SegmentMask(req); err != nil || resp.Cached {
		t.Fatalf("命中缓存不应占额度: resp=%+v err=%v", resp, err)
	}
	_, err = NewSegmentMaskLogic(ctx, s).SegmentMask(req)
	if code, msg, _ := xerr.Parse(err); code != xerr.Conflict || msg != msgPreviewTooFrequent {
		t.Fatalf("应超限: %v", err)
	}
	if len(rpc.got) != 3 {
		t.Fatalf("超限不应调 rpc: %d", len(rpc.got))
	}
}
