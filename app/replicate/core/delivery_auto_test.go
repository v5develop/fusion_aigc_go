package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/aigateway"
)

// 合成器 + 入库都装上的场景（多段）。
func autoDeliveryEnv(t *testing.T, mut func(*Config)) (*env, *memOps, *fakeAssets) {
	e := newEnv(t, true)
	ops := newMemOps(func() time.Time { return e.clock })
	assets := &fakeAssets{}
	cfg := DefaultConfig()
	cfg.ASRModelGroup = "asr.paraformer"
	if mut != nil {
		mut(&cfg)
	}
	e.svc = New(cfg, e.store, e.ledger, WithGateway(e.gw), WithMedia(e.media), WithClock(func() time.Time { return e.clock }),
		WithOwner("test-owner"), WithComposer(&fakeComposer{}), WithURLOwner(testOwner), WithSigner(e.signer), WithCutter(e.cutter),
		WithDelivery(ops, &fakeLib{}, assets, DeliveryConfig{MaxAttempts: 3}))
	return e, ops, assets
}

func deliveryKinds(ops *memOps, jobID int64) map[string]*model.Delivery {
	out := map[string]*model.Delivery{}
	for _, d := range ops.dels {
		if d.JobID == jobID {
			out[d.Kind+"/"+string(rune('0'+d.SegIndex))] = d
		}
	}
	return out
}

func assetProps(t *testing.T, a string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(a), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// 部分成功：已扣费的段逐段入库 + 预览版入库（「部分完成」、preview=true）；退点的失败段不入。
// 之后重做成功拼出完整成片：完整版作为新资产入库，预览版保留。
func TestAutoDeliveryPartialPreviewThenFull(t *testing.T) {
	e, ops, assets := autoDeliveryEnv(t, nil)
	jv := e.confirm(e.plan(e.seedAnalysis(32, 12, 25)), "all", "k1")
	e.gw.script["seg/02/generate"] = []outcome{{status: aigateway.StatusFailed, class: "VENDOR_FAILED"}}
	e.gw.script["seg/02/generate#r1"] = []outcome{{status: aigateway.StatusFailed, class: "VENDOR_FAILED"}}
	jv = e.run(jv.JobID, 6)
	if jv.Status != "partial" || jv.Result == nil || jv.Result.Kind != ResultPreview {
		t.Fatalf("应部分成功 + 预览版：%s %+v", jv.Status, jv.Result)
	}
	k := deliveryKinds(ops, jv.JobID)
	if k["segment/1"] == nil || k["segment/3"] == nil || k["segment/2"] != nil || k["preview/0"] == nil || k["final/0"] != nil || len(k) != 3 {
		t.Fatalf("应入：第 1、3 段 + 预览版；第 2 段已退点不入：%v", k)
	}
	if jv.Library == nil || jv.Library.Status != "saving" {
		t.Fatalf("成片素材库状态跟着预览版：%+v", jv.Library)
	}
	e.svc.TickDeliveries(e.ctx, 10)
	var preview, seg1 bool
	for _, a := range assets.rows {
		p := assetProps(t, a.Properties)
		if strings.Contains(a.Name, "部分完成") && p["preview"] == true && p["kind"] == model.DeliveryPreview {
			preview = true
		}
		if strings.Contains(a.Name, "第 1 段") && p["preview"] == false {
			seg1 = true
		}
	}
	if !preview || !seg1 || len(assets.rows) != 3 {
		t.Fatalf("素材库应有 第 1 段 / 第 3 段 / 部分完成（preview）：%+v", assets.rows)
	}

	if _, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 2, IdempotencyKey: "r1"}); err != nil {
		t.Fatal(err)
	}
	jv = e.run(jv.JobID, 6)
	if jv.Status != "succeeded" || jv.Result.Kind != ResultFull {
		t.Fatalf("重做成功应拼出完整成片：%s %+v", jv.Status, jv.Result)
	}
	k = deliveryKinds(ops, jv.JobID)
	if k["segment/2"] == nil || k["final/0"] == nil || k["preview/0"] == nil || len(k) != 5 {
		t.Fatalf("重做成功的段 + 完整版另入，预览版保留：%v", k)
	}
	e.svc.TickDeliveries(e.ctx, 10)
	if len(assets.rows) != 5 {
		t.Fatalf("完整版作为新资产入库（预览版保留）：%d", len(assets.rows))
	}
	e.assertMoney(jv.JobID)
}

// 只有已确认扣费的产物入库：不计费（Billing off）的 job 什么都不入，手动保存回 SEGMENT_NOT_PAID。
func TestAutoDeliveryOnlyPaid(t *testing.T) {
	e, ops, _ := autoDeliveryEnv(t, nil)
	e.ledger.off = true
	jv := e.run(e.confirm(e.plan(e.seedAnalysis(10)), "", "k1").JobID, 3)
	if jv.Status != "succeeded" {
		t.Fatalf("%s", jv.Status)
	}
	if len(ops.dels) != 0 {
		t.Fatalf("没收钱的产物不入库：%d", len(ops.dels))
	}
	if _, err := e.svc.SaveSegment(e.ctx, uid, jv.JobID, 1); reasonOf(err) != "SEGMENT_NOT_PAID" {
		t.Fatalf("未收费的段手动保存应拒绝：%v", err)
	}
}

// 强制收尾（待继续 → 只交付第 1 段）后：已扣费的第 1 段与部分成片自动入库。
func TestAutoDeliveryAfterForceFinish(t *testing.T) {
	e, ops, _ := autoDeliveryEnv(t, nil)
	jv := e.run(e.confirm(e.plan(e.seedAnalysis(32, 12, 25)), "", "k1").JobID, 3)
	if jv.Status != "waiting" {
		t.Fatalf("应待继续：%s", jv.Status)
	}
	if _, err := e.svc.ForceFinish(e.ctx, jv.JobID); err != nil {
		t.Fatal(err)
	}
	e.run(jv.JobID, 2) // 轮询器补合成
	k := deliveryKinds(ops, jv.JobID)
	f := k["final/0"]
	if f == nil {
		t.Fatalf("强制收尾后部分成片应入库：%v", k)
	}
	seg1 := jv.Segments[0].VideoRid
	switch {
	case f.SrcRid == seg1 && len(k) != 1: // 部分成片就是第 1 段（同 rid）：只入 final
		t.Fatalf("同内容只入一条：%v", k)
	case f.SrcRid != seg1 && (k["segment/1"] == nil || len(k) != 2):
		t.Fatalf("成片与第 1 段内容不同：两条都入：%v", k)
	}
}
