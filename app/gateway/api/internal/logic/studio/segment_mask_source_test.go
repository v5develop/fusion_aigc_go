package studio

import (
	"context"
	"testing"

	"aigc-local-life/app/canvas/rpc/canvas_client"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/userquota"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
)

// fakeCandidateCanvas 只实现 CandidateSource，记录入参。
type fakeCandidateCanvas struct {
	canvas_client.Canvas
	resp *canvas_client.CandidateSourceResp
	got  []*canvas_client.CandidateSourceReq
}

func (f *fakeCandidateCanvas) CandidateSource(_ context.Context, in *canvas_client.CandidateSourceReq, _ ...grpc.CallOption) (*canvas_client.CandidateSourceResp, error) {
	f.got = append(f.got, in)
	return f.resp, nil
}

func maskSourceCtx(t *testing.T, cv *fakeCandidateCanvas) (*svc.ServiceContext, *fakeMaskStudio, context.Context) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	rpc := &fakeMaskStudio{}
	s := &svc.ServiceContext{StudioRpc: rpc, CanvasRpc: cv, MaskQuota: userquota.New("studio:mask", 50, 500, rc)}
	return s, rpc, ctxdata.WithUserID(context.Background(), 42)
}

// 图片来源：task_id 透传给 studio；画布候选先按归属问 canvas-rpc，私有候选给 source_rid（不带前端地址），公开候选给 image_url。
func TestSegmentMaskSourceFields(t *testing.T) {
	cv := &fakeCandidateCanvas{resp: &canvas_client.CandidateSourceResp{Rid: "abcdef-work-nnnnnnnnnnnnnnnnnnnnnnnnnn"}}
	s, rpc, ctx := maskSourceCtx(t, cv)
	if _, err := NewSegmentMaskLogic(ctx, s).SegmentMask(&types.StudioMaskReq{TaskId: "simg_1", Target: "garment"}); err != nil {
		t.Fatal(err)
	}
	if g := rpc.got[0]; g.TaskId != "simg_1" || g.SourceRid != "" || g.UserId != 42 {
		t.Fatalf("task_id 应透传: %+v", g)
	}
	idx := int32(2)
	if _, err := NewSegmentMaskLogic(ctx, s).SegmentMask(&types.StudioMaskReq{CanvasRequestId: "41", CandidateIndex: &idx,
		ImageUrl: "https://priv/x.png?Signature=1", Target: "garment"}); err != nil {
		t.Fatal(err)
	}
	if c := cv.got[0]; c.UserId != 42 || c.Id != 41 || c.CandidateIndex == nil || *c.CandidateIndex != 2 {
		t.Fatalf("canvas 入参 %+v", c)
	}
	if g := rpc.got[1]; g.SourceRid != cv.resp.Rid || g.ImageUrl != "" || g.TaskId != "" {
		t.Fatalf("私有候选应给 source_rid: %+v", g)
	}
	cv.resp = &canvas_client.CandidateSourceResp{Url: "https://cdn/pub.png"}
	if _, err := NewSegmentMaskLogic(ctx, s).SegmentMask(&types.StudioMaskReq{CanvasRequestId: "41", Target: "person"}); err != nil {
		t.Fatal(err)
	}
	if g := rpc.got[2]; g.ImageUrl != "https://cdn/pub.png" || g.SourceRid != "" || cv.got[1].CandidateIndex != nil {
		t.Fatalf("公开候选应给 image_url: %+v", g)
	}
}

// 来源参数不合法：不调 rpc。
func TestSegmentMaskSourceRejects(t *testing.T) {
	cv := &fakeCandidateCanvas{resp: &canvas_client.CandidateSourceResp{Rid: "x"}}
	s, rpc, ctx := maskSourceCtx(t, cv)
	idx := int32(0)
	for _, req := range []*types.StudioMaskReq{
		{Target: "garment"}, // 什么来源都没有
		{TaskId: "a", CanvasRequestId: "1", Target: "garment"},
		{CanvasRequestId: "abc", Target: "garment"},
		{CanvasRequestId: "-1", Target: "garment"},
		{ImageUrl: "https://cdn/r.png", CandidateIndex: &idx, Target: "garment"},
	} {
		if _, err := NewSegmentMaskLogic(ctx, s).SegmentMask(req); err == nil {
			t.Fatalf("应拒: %+v", req)
		}
	}
	if len(rpc.got) != 0 || len(cv.got) != 0 {
		t.Fatalf("不应调 rpc: studio=%d canvas=%d", len(rpc.got), len(cv.got))
	}
}
