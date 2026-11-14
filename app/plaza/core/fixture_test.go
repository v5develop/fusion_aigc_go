package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"aigc-local-life/app/plaza/model"
)

// fakeSource 内存数据源。
type fakeSource struct {
	inds  []model.Industry
	rels  []model.TemplateIndustry
	tpls  []model.Template
	chans []model.Channel
}

func (f *fakeSource) Industries(context.Context) ([]model.Industry, error) { return f.inds, nil }
func (f *fakeSource) Relations(context.Context) ([]model.TemplateIndustry, error) {
	return f.rels, nil
}
func (f *fakeSource) PublishedTemplates(context.Context) ([]model.Template, error) {
	var out []model.Template
	for _, t := range f.tpls {
		if t.Deleted == 0 && t.State == model.StatePublished {
			out = append(out, t)
		}
	}
	return out, nil
}
func (f *fakeSource) Channels(context.Context) ([]model.Channel, error) { return f.chans, nil }

var t0 = time.Date(2026, 11, 13, 12, 0, 0, 0, cst)

func ind(id int64, code, name string, parent int64, sort int, enabled int8) model.Industry {
	i := model.Industry{ID: id, Code: code, Name: name, SortOrder: sort, Enabled: enabled,
		Hint: sql.NullString{String: name + "提示", Valid: true}}
	if parent > 0 {
		i.ParentID = sql.NullInt64{Int64: parent, Valid: true}
	}
	return i
}

func tpl(id int64, title, media string, created time.Time) model.Template {
	return model.Template{
		ID: id, Title: title, MediaType: media, State: model.StatePublished, Enabled: 1, MediaStatus: model.MediaStatusReady,
		NeedsProductImage: 1,
		DataJSON:          json.RawMessage(`{"result":["https://x.oss-cn-hangzhou.aliyuncs.com/a` + title + `.webp"],"prompt":"SECRET","description":"d-` + title + `","agentSkillId":"1876543210987654321"}`),
		TagsJSON:          json.RawMessage(`{"tags":["标签-` + title + `"]}`),
		CreateTime:        created, UpdateTime: created,
	}
}

func rel(tid, iid int64, sort int) model.TemplateIndustry {
	return model.TemplateIndustry{TemplateID: tid, IndustryID: iid, SortOrder: sort}
}

// stdFixture 行业：foot_bath(1) 含 bluev(2)、creator(3 停用)；spa(4)；dead(5 停用，下挂 dead_sub(6))；empty(7，无模板)；forced(8，无模板但 show_when_empty)。
func stdFixture() *fakeSource {
	forced := ind(8, "forced", "预热行业", 0, 80, 1)
	forced.ShowWhenEmpty = 1
	return &fakeSource{
		inds: []model.Industry{
			ind(1, "foot_bath", "足浴", 0, 20, 1),
			ind(2, "foot_bath_bluev", "蓝V号", 1, 10, 1),
			ind(3, "foot_bath_creator", "达人号", 1, 20, 0),
			ind(4, "spa", "SPA", 0, 10, 1),
			ind(5, "dead", "停用行业", 0, 30, 0),
			ind(6, "dead_sub", "停用行业子级", 5, 10, 1),
			ind(7, "empty", "空行业", 0, 70, 1),
			forced,
		},
		tpls: []model.Template{
			tpl(101, "A", "image", t0.Add(-48*time.Hour)),
			tpl(102, "B", "image", t0.Add(-24*time.Hour)),
			tpl(103, "C", "video", t0.Add(-1*time.Hour)),
			tpl(104, "D", "image", t0.Add(-100*24*time.Hour)),
			tpl(105, "E", "image", t0.Add(-2*time.Hour)),
		},
		rels: []model.TemplateIndustry{
			rel(101, 1, 20), rel(102, 1, 10), rel(103, 2, 5), rel(104, 4, 0), rel(105, 3, 0), rel(105, 6, 0),
		},
		chans: []model.Channel{
			{ID: 1, Code: "douyin_9_16", Name: "抖音 9:16", Width: 1080, Height: 1920, Ratio: "9:16", Enabled: 1},
			{ID: 2, Code: "off", Name: "停用渠道", Width: 100, Height: 100, Ratio: "1:1", Enabled: 0},
		},
	}
}

func newTestService(src Source) *Service {
	cfg := Config{}
	cfg.Industry.Enabled = true
	return NewWithSource(src, cfg, func() time.Time { return t0 })
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

var ctx0 = context.Background()
