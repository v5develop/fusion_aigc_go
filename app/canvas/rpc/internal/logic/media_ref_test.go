package logic

import (
	"context"
	"encoding/json"
	"testing"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/internal/repo"
	"aigc-local-life/app/studio/rpc/studio"
)

func TestClassifyStudioKeepsWatermarkRefRaw(t *testing.T) {
	ref := "mrw://" + testRID + ".png"
	o := ClassifyStudio(&studio.BatchDetail{Batch: &studio.BatchInfo{Status: 4},
		Tasks: []*studio.TaskInfo{{Status: 2, ResultUrl: ref}}})
	if !o.ok || o.outputs[0].URL != ref || !o.outputs[0].Stable {
		t.Fatalf("应原样保留引用: %+v", o)
	}
}

func TestResolveMediaRefsFailClosed(t *testing.T) {
	c := testCore()
	ref := "mrw://" + testRID + ".png"
	// 没配处理串 / 媒体服务：水印视图置空，普通地址不动
	a, b := ref, "https://cdn/x.png"
	c.resolveMediaRefs(context.Background(), []*string{&a, &b})
	if a != "" || b != "https://cdn/x.png" {
		t.Fatalf("a=%q b=%q", a, b)
	}
	// 候选读出口：引用签不出 → 空 url，不报错
	cands, _ := json.Marshal([]model.Candidate{{URL: ref, W: 1, H: 1}})
	r := &model.NodeRun{Candidates: cands, Status: model.RunSucceeded}
	got := c.resolvedRun(context.Background(), r).CandidateList()
	if len(got) != 1 || got[0].URL != "" {
		t.Fatalf("got=%+v", got)
	}
	if string(repo.MarshalCandidates(r.CandidateList())) != string(cands) {
		t.Fatalf("原 run 不应被改写")
	}
}
