package logic

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/studio/model"
)

const (
	amOK   = "adm001-work-aaaaaaaaaaaaaaaaaaaaaaaaaa"
	amBad  = "adm001-work-bbbbbbbbbbbbbbbbbbbbbbbbbb"
	amMask = "adm001-work-cccccccccccccccccccccccccc"
)

// 后台批次详情：组图 / 送生图图序 / 成品 / 蒙版里的 mr:// 都解析成预览地址，原始 ref 保留；
// 签不出、aig:// 这类媒体服务签不了的引用 url 为 null；公开地址原样返回。
func TestBuildAdminMediaResolvesRefs(t *testing.T) {
	resetSignedCache()
	s := &fakeBothSigner{bad: map[string]bool{amBad: true}}
	sel, _ := json.Marshal([]model.SelectedImages{{ModelIndex: 0, Images: []model.InputImage{
		{ID: "g-0-front-0", URL: "mr://" + amOK + ".png", Role: "garment"},
		{ID: "m-0", URL: "mr://" + amBad + ".png", Role: "model"},
	}}})
	in, _ := json.Marshal([]model.InputImage{
		{ID: "m-0", URL: "mr://" + amOK + ".png", Role: "model"},
		{ID: "x", URL: "aig://gw/abc", Role: "ref"},
		{ID: "p", URL: "https://cdn.example.com/p.png", Role: "pose"},
	})
	parts, _ := json.Marshal([]map[string]string{{"part": "top", "mask_url": "mr://" + amMask + ".png"}})
	b := &model.Batch{ID: 9, SelectedImages: sel}
	tasks := []*model.Task{{
		ID: 1, TaskID: sql.NullString{String: "tyo_1", Valid: true}, InputImages: in,
		ResultURL: sql.NullString{String: "mr://" + amOK + ".png", Valid: true},
		MaskURL:   sql.NullString{String: "mr://" + amMask + ".png", Valid: true},
		MaskParts: parts, CreatedAt: time.Now(),
	}}

	m := buildAdminMedia(context.Background(), s, wmOn, b, tasks, func(*model.Task) string { return "AiCreation_LF_x" })

	g := m.SelectedImages[0].Images
	if g[0].Ref != "mr://"+amOK+".png" || g[0].URL == nil || !strings.Contains(*g[0].URL, amOK) {
		t.Fatalf("组图第 1 张应保留 ref 并签出 url: %+v", g[0])
	}
	if g[1].URL != nil || g[1].Ref == "" {
		t.Fatalf("签失败应 url=null 且保留 ref: %+v", g[1])
	}
	tm := m.Tasks[0]
	if tm.TraceTaskID != "AiCreation_LF_x" || tm.TaskID != "tyo_1" {
		t.Fatalf("链路任务号: %+v", tm)
	}
	if tm.Inputs[0].URL == nil || tm.Inputs[1].URL != nil || tm.Inputs[2].URL == nil || *tm.Inputs[2].URL != "https://cdn.example.com/p.png" {
		t.Fatalf("图序: mr 签出 / aig null / http 原样: %+v", tm.Inputs)
	}
	if tm.Output == nil || tm.Output.URL == nil || tm.Mask == nil || tm.Mask.URL == nil {
		t.Fatalf("成品与蒙版应签出: %+v %+v", tm.Output, tm.Mask)
	}
	if len(tm.MaskParts) != 1 || tm.MaskParts[0].Image.URL == nil {
		t.Fatalf("逐件蒙版应签出: %+v", tm.MaskParts)
	}
}

// 没有媒体服务（signer=nil）时全部 url=null，不 panic；序列化后 url 为 JSON null。
func TestBuildAdminMediaNoSigner(t *testing.T) {
	resetSignedCache()
	sel, _ := json.Marshal([]model.SelectedImages{{Images: []model.InputImage{{URL: "mr://adm001-work-dddddddddddddddddddddddddd.png"}}}})
	raw := encodeAdminDiagnosticsWithMedia(context.Background(), nil, wmOn, nil, &model.Batch{ID: 1, SelectedImages: sel}, nil, time.Now())
	var d struct {
		Media struct {
			SelectedImages []struct {
				Images []struct {
					Ref string  `json:"ref"`
					URL *string `json:"url"`
				} `json:"images"`
			} `json:"selectedImages"`
		} `json:"media"`
	}
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatal(err)
	}
	im := d.Media.SelectedImages[0].Images[0]
	if im.Ref == "" || im.URL != nil {
		t.Fatalf("无 signer 应 url=null 保留 ref: %+v", im)
	}
}
