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

type fakeAutoFixStudio struct {
	studio_client.Studio
	got  []*studio_client.AutoColorFixReq
	resp *studio_client.AutoColorFixResp
	err  error
}

func (f *fakeAutoFixStudio) AutoColorFix(_ context.Context, in *studio_client.AutoColorFixReq, _ ...grpc.CallOption) (*studio_client.AutoColorFixResp, error) {
	f.got = append(f.got, in)
	return f.resp, f.err
}

func newAutoFixSvc(t *testing.T, rpc *fakeAutoFixStudio, perMin int) *svc.ServiceContext {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	return &svc.ServiceContext{StudioRpc: rpc, ColorFixAutoQuota: userquota.New("studio:colorfix_auto", perMin, 500, rc)}
}

// 用户来自会话、不收客户端传的用户；结果原样映射（空 skipped 是 []，不是 null）。
func TestAutoColorFixPassThrough(t *testing.T) {
	rpc := &fakeAutoFixStudio{resp: &studio_client.AutoColorFixResp{TaskId: "tyo1", Version: "v",
		Pieces: []*studio_client.AutoColorFixPiece{{Key: "top", Score: 88.5,
			Params: &studio_client.ColorFixParams{Hue: -9, Saturation: 2, Value: 1, Brightness: -7, Contrast: 3}}}}}
	s := newAutoFixSvc(t, rpc, 5)
	ctx := ctxdata.WithUserID(context.Background(), 42)
	resp, err := NewAutoColorFixLogic(ctx, s).AutoColorFix(&types.StudioAutoColorFixReq{TaskId: " tyo1 ", Pieces: []string{"top"}})
	if err != nil || len(resp.Pieces) != 1 || resp.Skipped == nil || len(resp.Skipped) != 0 ||
		resp.Pieces[0].Params != (types.StudioColorFixParams{Hue: -9, Saturation: 2, Value: 1, Brightness: -7, Contrast: 3}) || resp.Pieces[0].Score != 88.5 {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if g := rpc.got[0]; g.UserId != 42 || g.TaskId != "tyo1" || len(g.Pieces) != 1 {
		t.Fatalf("透传 %+v", g)
	}
	if _, err = NewAutoColorFixLogic(ctx, s).AutoColorFix(&types.StudioAutoColorFixReq{}); err == nil || len(rpc.got) != 1 {
		t.Fatalf("缺任务号应拒绝且不调 rpc: %v", err)
	}
}

// 配额：超限 4002 · COLORFIX_AUTO_TOO_FREQUENT 且不调 rpc；没真正调到求解的失败退回额度，真调用过的（忙 / 超时）不退。
func TestAutoColorFixQuotaAndRefund(t *testing.T) {
	rpc := &fakeAutoFixStudio{resp: &studio_client.AutoColorFixResp{}}
	s := newAutoFixSvc(t, rpc, 2)
	ctx := ctxdata.WithUserID(context.Background(), 42)
	req := &types.StudioAutoColorFixReq{TaskId: "tyo1"}
	for i := 0; i < 2; i++ {
		if _, err := NewAutoColorFixLogic(ctx, s).AutoColorFix(req); err != nil {
			t.Fatalf("第 %d 次: %v", i, err)
		}
	}
	_, err := NewAutoColorFixLogic(ctx, s).AutoColorFix(req)
	if code, _, _ := xerr.Parse(err); code != xerr.Conflict || xerr.ReasonOf(err) != xerr.ReasonColorFixAutoTooFrequent || len(rpc.got) != 2 {
		t.Fatalf("超限 err=%v calls=%d", err, len(rpc.got))
	}

	// 功能没开：退回额度，可以一直探测不占名额
	rpc2 := &fakeAutoFixStudio{err: xerr.NewReason(xerr.NotReady, xerr.ReasonColorFixAutoDisabled, "x")}
	s2 := newAutoFixSvc(t, rpc2, 1)
	for i := 0; i < 3; i++ {
		_, err = NewAutoColorFixLogic(ctx, s2).AutoColorFix(req)
		if xerr.ReasonOf(err) != xerr.ReasonColorFixAutoDisabled {
			t.Fatalf("功能关闭不该占额度，第 %d 次 err=%v", i, err)
		}
	}
	// 求解忙：已占用资源，不退
	rpc3 := &fakeAutoFixStudio{err: xerr.NewReason(xerr.ThirdPartyError, xerr.ReasonColorFixAutoBusy, "x")}
	s3 := newAutoFixSvc(t, rpc3, 1)
	_, _ = NewAutoColorFixLogic(ctx, s3).AutoColorFix(req)
	_, err = NewAutoColorFixLogic(ctx, s3).AutoColorFix(req)
	if xerr.ReasonOf(err) != xerr.ReasonColorFixAutoTooFrequent {
		t.Fatalf("忙不该退额度: %v", err)
	}
}
