package logic

import (
	"context"
	"testing"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"

	"github.com/DATA-DOG/go-sqlmock"
)

const csRID = "abcdef-work-nnnnnnnnnnnnnnnnnnnnnnnnnn"

func expectCSRun(mock sqlmock.Sqlmock, userID int64, status int8, kind, cands string, selected int) {
	mock.ExpectQuery("FROM `afv_canvas_node_run` AS `r` WHERE \\(r.id = 41\\)").
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status", "kind", "candidates", "selected_index", "deleted"}).
			AddRow(41, userID, status, kind, cands, selected, 0))
}

// 候选来源：私有引用给 rid（水印视图同样给原件 rid），公开地址给 url；不传下标取当前选中的候选。
func TestCandidateSource(t *testing.T) {
	cands := `[{"url":"https://cdn/a.png"},{"url":"mr://` + csRID + `.png"},{"url":"mrw://` + csRID + `.jpg"}]`
	i1, i2 := int32(1), int32(2)
	for _, tc := range []struct {
		name     string
		idx      *int32
		selected int
		rid, url string
	}{
		{name: "选中的公开候选", selected: 0, url: "https://cdn/a.png"},
		{name: "私有引用", idx: &i1, rid: csRID},
		{name: "水印视图", idx: &i2, rid: csRID},
		{name: "不传下标按选中", selected: 1, rid: csRID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, mock := mockCore(t)
			expectCSRun(mock, 7, model.RunSucceeded, "image", cands, tc.selected)
			out, err := c.CandidateSource(context.Background(), &canvas.CandidateSourceReq{UserId: 7, Id: 41, CandidateIndex: tc.idx})
			if err != nil || out.Rid != tc.rid || out.Url != tc.url {
				t.Fatalf("out=%+v err=%v", out, err)
			}
		})
	}
}

// 别人的 run、未成功、视频、下标越界：一律拒绝，不给 rid。
func TestCandidateSourceRejects(t *testing.T) {
	cands := `[{"url":"mr://` + csRID + `.png"}]`
	i5 := int32(5)
	for _, tc := range []struct {
		name   string
		user   int64
		status int8
		kind   string
		idx    *int32
	}{
		{name: "别人的", user: 8, status: model.RunSucceeded, kind: "image"},
		{name: "未成功", user: 7, status: model.RunRunning, kind: "image"},
		{name: "视频", user: 7, status: model.RunSucceeded, kind: "video"},
		{name: "越界", user: 7, status: model.RunSucceeded, kind: "image", idx: &i5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, mock := mockCore(t)
			expectCSRun(mock, tc.user, tc.status, tc.kind, cands, 0)
			out, err := c.CandidateSource(context.Background(), &canvas.CandidateSourceReq{UserId: 7, Id: 41, CandidateIndex: tc.idx})
			if err == nil || out != nil {
				t.Fatalf("应拒绝: out=%+v", out)
			}
		})
	}
}
