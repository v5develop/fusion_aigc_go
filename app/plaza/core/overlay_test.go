package core

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"testing"

	"aigc-local-life/pkg/mediasvc"

	"github.com/DATA-DOG/go-sqlmock"
)

const (
	ovPlainRid = "abc123-work-aaaaaaaaaaaaaaaaaaaaaaaaaa"
	ovOutRid   = "abc123-work-bbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// fakeOverlayMedia 记录调用的媒体假实现；任何一步都可以指定失败。
type fakeOverlayMedia struct {
	ingestErr, textErr, copyErr error
	height                      int
	calls                       []string
	slots                       []mediasvc.TextSlot
}

func (f *fakeOverlayMedia) IngestURL(_ context.Context, _, _, _ string) (mediasvc.Resource, error) {
	f.calls = append(f.calls, "ingest")
	if f.ingestErr != nil {
		return mediasvc.Resource{}, f.ingestErr
	}
	return mediasvc.Resource{RID: ovPlainRid, Width: 1000, Height: f.height}, nil
}
func (f *fakeOverlayMedia) ImageText(_ context.Context, _ string, sl []mediasvc.TextSlot, _ string) (mediasvc.Resource, error) {
	f.calls = append(f.calls, "text")
	f.slots = sl
	if f.textErr != nil {
		return mediasvc.Resource{}, f.textErr
	}
	return mediasvc.Resource{RID: ovOutRid}, nil
}
func (f *fakeOverlayMedia) CopyToLibrary(_ context.Context, _, _, _ string) (mediasvc.Resource, error) {
	f.calls = append(f.calls, "copy")
	if f.copyErr != nil {
		return mediasvc.Resource{}, f.copyErr
	}
	return mediasvc.Resource{RID: "abc123-asset-cccccccccccccccccccccccccc", URL: "https://cdn/x.png"}, nil
}

type fakeOverlayAssets struct {
	calls int
	err   error
	saved bool
}

func (f *fakeOverlayAssets) SaveTextOverlayVariant(context.Context, int64, string, string, string) (bool, error) {
	f.calls++
	return f.saved, f.err
}

const ovSlotsJSON = `{"schemaVersion":1,"items":[
 {"key":"shop","type":"text","label":"店名","order":1,"maxLength":10,"layout":{"box":{"x":0.1,"y":0.8,"w":0.8,"h":0.1},"size":0.05,"color":"#FFFFFF","fontRid":"` + goodRid + `"}},
 {"key":"note","type":"text","label":"备注","order":2},
 {"key":"pic","type":"image","label":"图","order":3}]}`

func overlayService(t *testing.T, textOverlay int8, m *fakeOverlayMedia, a *fakeOverlayAssets) (*Service, sqlmock.Sqlmock) {
	t.Helper()
	s, mock := sqlmockService(t)
	fx := stdFixture()
	fx.tpls[0].SlotsJSON = json.RawMessage(ovSlotsJSON)
	fx.tpls[0].TextOverlay = textOverlay
	s.src = fx
	s.SetOverlay(m, a)
	s.ovOwner = func(context.Context, int64, string) (bool, error) { return true, nil }
	return s, mock
}

var ovCols = []string{"id", "user_id", "template_id", "source_url", "source_hash", "values_json", "plain_rid", "rendered_rid", "rendered_url", "status", "asset_synced", "fail_reason", "update_time"}

func expectOverlayWrite(mock sqlmock.Sqlmock, status int, url any, synced ...bool) {
	mock.ExpectQuery(`FROM afv_plaza_overlay WHERE user_id = \d+ AND template_id`).WillReturnRows(sqlmock.NewRows(ovCols)) // 先查有没有
	mock.ExpectQuery(`FROM afv_plaza_overlay WHERE user_id = \d+ AND template_id`).WillReturnRows(sqlmock.NewRows(ovCols)) // 取旧行
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO afv_plaza_overlay")).WillReturnResult(sqlmock.NewResult(7, 1))
	mock.ExpectQuery(`SELECT id FROM afv_plaza_overlay`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(7))
	if len(synced) > 0 && synced[0] { // 登记素材库成功 → 回写 asset_synced=1
		mock.ExpectExec(`UPDATE afv_plaza_overlay SET asset_synced = 1`).WillReturnResult(sqlmock.NewResult(0, 1))
	}
	var reason any
	if status != overlayStatusOK {
		reason = "x"
	}
	mock.ExpectQuery(`FROM afv_plaza_overlay WHERE id = \d+ AND user_id`).WillReturnRows(sqlmock.NewRows(ovCols).
		AddRow(7, 9, 101, "https://cdn/p.png", "h", `{"shop":"某某足浴"}`, ovPlainRid, nil, url, status, 0, reason, t0))
}

func TestOverlaySuccessRendersAndRegistersAssetsWithoutBilling(t *testing.T) {
	m, a := &fakeOverlayMedia{height: 2000}, &fakeOverlayAssets{saved: true}
	s, mock := overlayService(t, 1, m, a)
	expectOverlayWrite(mock, overlayStatusOK, "https://cdn/x.png", true)
	rec, err := s.CreateOverlay(ctx0, 9, 101, "https://cdn/p.png", map[string]string{"shop": " 某某足浴 ", "note": ""})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != OverlayOK || rec.RenderedURL == nil || *rec.RenderedURL != "https://cdn/x.png" {
		t.Fatalf("应叠字成功: %+v", rec)
	}
	if got := m.calls; len(got) != 3 || got[0] != "ingest" || got[1] != "text" || got[2] != "copy" {
		t.Fatalf("媒体调用顺序: %v", got)
	}
	// layout 相对值按画面高度换算：0.05 * 2000 = 100 像素；只带有值且有 layout 的位
	if len(m.slots) != 1 || m.slots[0].Text != "某某足浴" || m.slots[0].Size != 100 || m.slots[0].FontRid != goodRid {
		t.Fatalf("槽位不对: %+v", m.slots)
	}
	if a.calls != 1 {
		t.Fatalf("成功后应登记素材库一次: %d", a.calls)
	}
	// 叠字全程只走媒体与素材登记，没有报价 / 计费依赖（Service 上根本没有扣点入口）；sqlmock 没有多余的语句也印证了这一点
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestOverlayFailureKeepsOriginalAndIsRetryable(t *testing.T) {
	for name, m := range map[string]*fakeOverlayMedia{
		"摄入失败": {height: 2000, ingestErr: errors.New("boom")},
		"叠字失败": {height: 2000, textErr: errors.New("boom")},
		"转存失败": {height: 2000, copyErr: errors.New("boom")},
	} {
		a := &fakeOverlayAssets{saved: true}
		s, mock := overlayService(t, 1, m, a)
		expectOverlayWrite(mock, overlayStatusFailed, nil)
		rec, err := s.CreateOverlay(ctx0, 9, 101, "https://cdn/p.png", map[string]string{"shop": "某某足浴"})
		if err != nil {
			t.Fatalf("%s: 叠字失败不应抛错（原图照常交付）: %v", name, err)
		}
		if rec.Status != OverlayFailed || rec.RenderedURL != nil || rec.FailReason == "" {
			t.Fatalf("%s: 应是 failed 记录: %+v", name, rec)
		}
		if a.calls != 0 {
			t.Fatalf("%s: 失败不登记素材", name)
		}
	}
}

func TestOverlayRejects(t *testing.T) {
	m := &fakeOverlayMedia{height: 2000}
	s, _ := overlayService(t, 0, m, nil)
	if _, err := s.CreateOverlay(ctx0, 9, 101, "https://cdn/p.png", map[string]string{"shop": "x"}); err == nil {
		t.Fatal("模板没开 text_overlay 应拒绝")
	}
	s, _ = overlayService(t, 1, m, nil)
	cases := []struct {
		name string
		src  string
		vals map[string]string
	}{
		{"非 http 地址", "ftp://x/p.png", map[string]string{"shop": "x"}},
		{"没有 layout 的位", "https://cdn/p.png", map[string]string{"note": "x"}},
		{"全是空值", "https://cdn/p.png", map[string]string{"shop": "  "}},
		{"超长", "https://cdn/p.png", map[string]string{"shop": "一二三四五六七八九十十一"}},
	}
	for _, c := range cases {
		if _, err := s.CreateOverlay(ctx0, 9, 101, c.src, c.vals); err == nil {
			t.Errorf("%s: 应被拒", c.name)
		}
	}
	if len(m.calls) != 0 {
		t.Fatalf("被拒的请求不能碰媒体服务: %v", m.calls)
	}
	// 媒体服务没配 → 暂未开放
	s2, _ := sqlmockService(t)
	if _, err := s2.CreateOverlay(ctx0, 9, 101, "https://cdn/p.png", map[string]string{"shop": "x"}); reasonOf(err) != ReasonOverlayNotReady {
		t.Fatalf("没配媒体服务: %v", err)
	}
	// 不是自己的图
	s3, _ := overlayService(t, 1, m, nil)
	s3.ovOwner = func(context.Context, int64, string) (bool, error) { return false, nil }
	if _, err := s3.CreateOverlay(ctx0, 9, 101, "https://cdn/p.png", map[string]string{"shop": "x"}); err == nil {
		t.Fatal("别人的成品图应拒绝")
	}
}

func TestOverlayOwnershipCheckedBeforeQuota(t *testing.T) {
	m, a := &fakeOverlayMedia{height: 2000}, &fakeOverlayAssets{saved: true}
	s, mock := overlayService(t, 1, m, a)
	s.cfg.Overlay.PerMinute = 1
	s.SetOverlay(m, a) // 重建限频：每分钟 1 次
	owned := false
	s.ovOwner = func(context.Context, int64, string) (bool, error) { return owned, nil }
	for i := 0; i < 3; i++ { // 试探别人的图：不应消耗自己的配额
		mock.ExpectQuery(`FROM afv_plaza_overlay WHERE user_id = \d+ AND template_id`).WillReturnRows(sqlmock.NewRows(ovCols))
		if _, err := s.CreateOverlay(ctx0, 9, 101, "https://cdn/p.png", map[string]string{"shop": "x"}); err == nil {
			t.Fatal("别人的图应拒绝")
		}
	}
	owned = true
	expectOverlayWrite(mock, overlayStatusOK, "https://cdn/x.png", true)
	if rec, err := s.CreateOverlay(ctx0, 9, 101, "https://cdn/p.png", map[string]string{"shop": "x"}); err != nil || rec.Status != OverlayOK {
		t.Fatalf("试探没扣配额，自己的图应成功: %v %+v", err, rec)
	}
	mock.ExpectQuery(`FROM afv_plaza_overlay WHERE user_id = \d+ AND template_id`).WillReturnRows(sqlmock.NewRows(ovCols))
	if _, err := s.CreateOverlay(ctx0, 9, 101, "https://cdn/p2.png", map[string]string{"shop": "x"}); reasonOf(err) != ReasonOverlayTooFrequent {
		t.Fatalf("第二次成功调用应被限频: %v", err)
	}
}

func TestOverlayAssetRegistrationFailureIsHealedOnRead(t *testing.T) {
	m, a := &fakeOverlayMedia{height: 2000}, &fakeOverlayAssets{err: errors.New("db down"), saved: true}
	s, mock := overlayService(t, 1, m, a)
	expectOverlayWrite(mock, overlayStatusOK, "https://cdn/x.png")
	rec, err := s.CreateOverlay(ctx0, 9, 101, "https://cdn/p.png", map[string]string{"shop": "某某足浴"})
	if err != nil || rec.Status != OverlayOK {
		t.Fatalf("登记失败不影响交付: %v %+v", err, rec)
	}
	if a.calls != 1 {
		t.Fatal("渲染后应尝试登记一次")
	}
	// 之后读取：asset_synced=0 的成功记录被补登记，成功后置 1
	a.err = nil
	mock.ExpectQuery(`FROM afv_plaza_overlay WHERE id = \d+ AND user_id`).WillReturnRows(sqlmock.NewRows(ovCols).
		AddRow(7, 9, 101, "https://cdn/p.png", "h", `{"shop":"某某足浴"}`, ovPlainRid, nil, "https://cdn/x.png", overlayStatusOK, 0, nil, t0))
	mock.ExpectExec(`UPDATE afv_plaza_overlay SET asset_synced = 1`).WillReturnResult(sqlmock.NewResult(0, 1))
	if _, err := s.GetOverlay(ctx0, 9, 7); err != nil {
		t.Fatal(err)
	}
	if a.calls != 2 {
		t.Fatalf("读取时应补登记: %d", a.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	// 已登记（asset_synced=1）的不再试
	mock.ExpectQuery(`FROM afv_plaza_overlay WHERE id = \d+ AND user_id`).WillReturnRows(sqlmock.NewRows(ovCols).
		AddRow(7, 9, 101, "https://cdn/p.png", "h", `{}`, ovPlainRid, nil, "https://cdn/x.png", overlayStatusOK, 1, nil, t0))
	if _, err := s.GetOverlay(ctx0, 9, 7); err != nil || a.calls != 2 {
		t.Fatalf("已登记不应再试: %v %d", err, a.calls)
	}
}

func TestOverlayPromptWarnings(t *testing.T) {
	good := []byte(`{"prompt":"高端足浴门头，画面中不要出现任何文字"}`)
	bad := []byte(`{"prompt":"高端足浴门头，暖色调"}`)
	if w := overlayPromptWarnings(1, good); len(w) != 0 {
		t.Fatalf("含约束不应提示: %v", w)
	}
	if w := overlayPromptWarnings(1, bad); len(w) != 1 {
		t.Fatalf("缺约束应提示: %v", w)
	}
	if w := overlayPromptWarnings(0, bad); len(w) != 0 {
		t.Fatal("未开叠字不提示")
	}
}
