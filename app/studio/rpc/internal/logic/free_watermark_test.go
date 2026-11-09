package logic

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"testing"
	"time"

	"aigc-local-life/app/studio/rpc/internal/config"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/asset"
	"aigc-local-life/pkg/mediasvc"

	"github.com/DATA-DOG/go-sqlmock"
)

const (
	wmRID     = "abcdef-work-aaaaaaaaaaaaaaaaaaaaaaaaaa"
	wmRID2    = "abcdef-work-bbbbbbbbbbbbbbbbbbbbbbbbbb"
	wmProcess = "image/watermark,text_5YWN6LS5,t_60,g_se,x_10,y_10"
)

var wmOn = config.FreeWatermarkConf{Enabled: true, Process: wmProcess, SignedTTLSeconds: 600}

func resetSignedCache() {
	signedCache.Lock()
	signedCache.m = map[string]signedEntry{}
	signedCache.Unlock()
}

// fakeSigner 带处理参数的签名：记录收到的处理串；bad 里的 rid 签名失败。
type fakeSigner struct {
	processes []string
	calls     int
	bad       map[string]bool
	err       error
}

func (s *fakeSigner) ResourceURLsWithProcess(_ context.Context, rids []string, _ time.Duration, process string) (map[string]string, error) {
	s.calls++
	s.processes = append(s.processes, process)
	if s.err != nil {
		return nil, s.err
	}
	out := map[string]string{}
	for _, r := range rids {
		if s.bad[r] {
			return nil, &mediasvc.Error{Status: 404, Code: "not_found"}
		}
		out[r] = "https://private.oss/" + r + ".png?x-oss-process=" + process + "&Signature=s"
	}
	return out, nil
}

type fakeTier struct{ free bool }

func (t fakeTier) WatermarkedImage(context.Context, int64) bool { return t.free }

func TestWatermarkRefHelpers(t *testing.T) {
	if got := watermarkRef(wmRID, "image/jpeg"); got != "mrw://"+wmRID+".jpg" {
		t.Fatal(got)
	}
	if got := watermarkRef(wmRID, ""); got != "mrw://"+wmRID+".png" {
		t.Fatal(got)
	}
	if watermarkRidOf("mrw://"+wmRID+".png") != wmRID || watermarkRidOf("mr://"+wmRID+".png") != "" ||
		watermarkRidOf("mrw://bad.png") != "" || watermarkRidOf("https://x/y.png") != "" {
		t.Fatal("rid 解析不对")
	}
	if !(config.FreeWatermarkConf{Process: "style/wm-free"}).ValidProcess() ||
		(config.FreeWatermarkConf{Process: "a&b"}).ValidProcess() || (config.FreeWatermarkConf{}).ValidProcess() {
		t.Fatal("处理串校验不对")
	}
	if (config.FreeWatermarkConf{Enabled: true}).Active() || (config.FreeWatermarkConf{Process: wmProcess}).Active() || !wmOn.Active() {
		t.Fatal("Active 应要求开关 + 合法处理串")
	}
}

// 读：mrw:// 签成带处理参数的地址；https / mr:// 不动；签名失败只空掉那一个；没配处理串整批为空。
func TestResolveWatermarkViews(t *testing.T) {
	resetSignedCache()
	ctx := context.Background()
	s := &fakeSigner{bad: map[string]bool{wmRID2: true}}
	a, b, c, d, e := "mrw://"+wmRID+".png", "https://cdn/x.png", "mrw://"+wmRID2+".png", "mrw://"+wmRID+".png", "mrw://broken"
	resolveWatermarkViews(ctx, s, wmOn, []*string{&a, &b, &c, &d, &e})
	want := "https://private.oss/" + wmRID + ".png?x-oss-process=" + wmProcess + "&Signature=s"
	if a != want || d != want || b != "https://cdn/x.png" || c != "" || e != "" {
		t.Fatalf("a=%q b=%q c=%q d=%q e=%q", a, b, c, d, e)
	}
	for _, p := range s.processes {
		if p != wmProcess {
			t.Fatalf("必须带处理串签名: %q", p)
		}
	}

	// 缓存命中：不再打媒体服务
	calls := s.calls
	x := "mrw://" + wmRID + ".png"
	resolveWatermarkViews(ctx, s, wmOn, []*string{&x})
	if x != want || s.calls != calls {
		t.Fatalf("应命中缓存: %q calls=%d→%d", x, calls, s.calls)
	}

	// 没配处理串 / 没有媒体服务：水印视图为空，绝不原样或干净地下发
	resetSignedCache()
	y, z := "mrw://"+wmRID+".png", "mrw://"+wmRID+".png"
	resolveWatermarkViews(ctx, s, config.FreeWatermarkConf{}, []*string{&y})
	resolveWatermarkViews(ctx, nil, wmOn, []*string{&z})
	if y != "" || z != "" {
		t.Fatalf("y=%q z=%q", y, z)
	}

	// 媒体服务整体失败 → 空
	resetSignedCache()
	w := "mrw://" + wmRID + ".png"
	resolveWatermarkViews(ctx, &fakeSigner{err: errors.New("down")}, wmOn, []*string{&w})
	if w != "" {
		t.Fatalf("w=%q", w)
	}
}

// 对外结构：批次封面与子任务结果都会被处理；关掉开关（Enabled=false）已落库的 mrw:// 仍按 Process 签水印地址。
func TestDetailViewsCollectsAllFields(t *testing.T) {
	resetSignedCache()
	d := &studio.BatchDetail{
		Batch: &studio.BatchInfo{CoverUrl: "mrw://" + wmRID + ".png"},
		Tasks: []*studio.TaskInfo{{ResultUrl: "mrw://" + wmRID + ".png"}, {ResultUrl: "https://cdn/paid.png"}, nil},
	}
	bs, ts := detailViews([]*studio.BatchDetail{d, nil})
	off := wmOn
	off.Enabled = false
	resolveWatermarkViews(context.Background(), &fakeSigner{}, off, viewPtrs(bs, ts))
	if !regexp.MustCompile(`x-oss-process=image/watermark`).MatchString(d.Batch.CoverUrl) ||
		d.Batch.CoverUrl != d.Tasks[0].ResultUrl || d.Tasks[1].ResultUrl != "https://cdn/paid.png" {
		t.Fatalf("cover=%q task0=%q task1=%q", d.Batch.CoverUrl, d.Tasks[0].ResultUrl, d.Tasks[1].ResultUrl)
	}
	// 没有服务上下文：水印视图置空
	t2 := &studio.TaskInfo{ResultUrl: "mrw://" + wmRID + ".png"}
	signViews(context.Background(), nil, nil, []*studio.TaskInfo{t2})
	if t2.ResultUrl != "" {
		t.Fatal(t2.ResultUrl)
	}
}

// copyMedia 记录转存调用的媒体服务假实现。
type copyMedia struct {
	fakeMedia
	copies []string
	signed []string
}

func (m *copyMedia) CopyToLibrary(_ context.Context, rid, purpose, idem string) (mediasvc.Resource, error) {
	m.copies = append(m.copies, rid+"|"+purpose+"|"+idem)
	return mediasvc.Resource{RID: "abcdef-asset-cccccccccccccccccccccccccc", URL: "https://cdn/x.png", Width: 1024, Height: 1024}, nil
}

func (m *copyMedia) ResourceURLs(_ context.Context, rids []string, _ time.Duration) (map[string]string, error) {
	m.signed = append(m.signed, rids...)
	return map[string]string{rids[0]: "https://private.oss/" + rids[0] + ".png?Signature=clean"}, nil
}

func freeCall() *aigateway.Call {
	c := succeededCall()
	c.Result.Outputs[0].Media.Rid = wmRID
	c.Result.Outputs[0].Media.Mime = "image/png"
	return c
}

// 免费档 + 开关开：不转存公有桶，result_url 与素材库行都是 mrw://{rid}.png，result_rid 是私有原件 rid。
func TestDeliverFreeTierStoresWatermarkView(t *testing.T) {
	bill := onBill()
	f, mock, _ := newTestFlow(t, bill)
	media := &copyMedia{}
	f.media = media
	f.tier = fakeTier{free: true}
	f.cfg.FreeWatermark = wmOn
	f.cfg.Asset.Enabled = true
	var saved []asset.Asset
	f.asset = func(_ context.Context, list []asset.Asset) error { saved = append(saved, list...); return nil }
	ref := regexp.QuoteMeta("'mrw://" + wmRID + ".png'")
	mock.ExpectExec(regexp.QuoteMeta("SET `status` = 5") + ".*`result_rid` = '" + wmRID + "'.*`result_url` = " + ref).WillReturnResult(ok1)
	mock.ExpectQuery(regexp.QuoteMeta("FROM afv_asset AS a")).WillReturnRows(sqlmock.NewRows([]string{"source_task_id", "id"}))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 2") + ".*`result_url` = " + ref).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_done = task_done + 1")).WillReturnResult(ok1)
	mock.ExpectExec("SET step_log = JSON_ARRAY_APPEND").WillReturnResult(ok1)
	expectConvergeFinal(mock)
	f.deliver(context.Background(), runningTask(), batch9, freeCall(), time.Now())
	met(t, mock)
	if len(media.copies) != 0 {
		t.Fatalf("免费档不应转存公有桶: %v", media.copies)
	}
	if len(saved) != 1 || saved[0].ResultURL != "mrw://"+wmRID+".png" || saved[0].CoverURL != saved[0].ResultURL {
		t.Fatalf("素材库行应写水印视图: %+v", saved)
	}
}

// 付费用户 / 开关关：照旧转存公有桶。
func TestDeliverPaidOrSwitchOffUnchanged(t *testing.T) {
	cases := map[string]struct {
		tier freeTier
		cfg  config.FreeWatermarkConf
	}{
		"paid":           {fakeTier{free: false}, wmOn},
		"switch off":     {fakeTier{free: true}, config.FreeWatermarkConf{Process: wmProcess}},
		"no process":     {fakeTier{free: true}, config.FreeWatermarkConf{Enabled: true}},
		"tier not wired": {nil, wmOn},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f, mock, _ := newTestFlow(t, onBill())
			media := &copyMedia{}
			f.media = media
			f.tier = c.tier
			f.cfg.FreeWatermark = c.cfg
			mock.ExpectExec(regexp.QuoteMeta("SET `status` = 5") + ".*`result_url` = 'https://cdn/x.png'").WillReturnResult(ok1)
			mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 2") + ".*`result_url` = 'https://cdn/x.png'").WillReturnResult(ok1)
			mock.ExpectExec(regexp.QuoteMeta("task_done = task_done + 1")).WillReturnResult(ok1)
			mock.ExpectExec("SET step_log = JSON_ARRAY_APPEND").WillReturnResult(ok1)
			expectConvergeFinal(mock)
			f.deliver(context.Background(), runningTask(), batch9, freeCall(), time.Now())
			met(t, mock)
			if !reflect.DeepEqual(media.copies, []string{wmRID + "|asset_library|"}) {
				t.Fatalf("应转存公有桶: %v", media.copies)
			}
		})
	}
}

// 自动蒙版的源地址：水印视图签干净短期地址只给网关；普通地址原样。
func TestMaskSourceURL(t *testing.T) {
	f, _, _ := newTestFlow(t, onBill())
	media := &copyMedia{}
	f.media = media
	if got := f.maskSourceURL(context.Background(), "https://cdn/x.png"); got != "https://cdn/x.png" {
		t.Fatal(got)
	}
	if got := f.maskSourceURL(context.Background(), "mrw://"+wmRID+".png"); got != "https://private.oss/"+wmRID+".png?Signature=clean" {
		t.Fatal(got)
	}
	f.media = nil
	if got := f.maskSourceURL(context.Background(), "mrw://"+wmRID+".png"); got != "" {
		t.Fatal(got)
	}
}

// 付费转存：已付费用户的 mrw:// → 复制到公有素材库桶，先改素材库再 CAS 改子任务；扫描间隔内不重复。
func TestPromoteFreeOriginals(t *testing.T) {
	f, mock, _ := newTestFlow(t, onBill())
	media := &copyMedia{}
	f.media = media
	f.cfg.FreeWatermark.PromoteMinutes = 10
	old := "mrw://" + wmRID + ".png"
	newRID, newURL := "abcdef-asset-cccccccccccccccccccccccccc", "https://cdn/x.png"
	mock.ExpectQuery(regexp.QuoteMeta("WHERE t.result_url LIKE 'mrw://%' AND t.deleted = 0 AND t.id > 0 AND EXISTS (SELECT 1 FROM afv_points_batch p WHERE p.user_id = t.user_id AND p.source IN ('RECHARGE', 'UNION_RECHARGE') AND p.deleted = 0) ORDER BY t.id LIMIT 50")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "task_id", "result_url"}).AddRow(1, 7, "simg_1", old))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE afv_asset SET result_url = IF(result_url = '" + old + "', '" + newURL + "', result_url), cover_url = IF(cover_url = '" + old + "', '" + newURL + "', cover_url) WHERE source_task_id = 'simg_1' AND user_id = 7")).
		WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET result_rid = '" + newRID + "', result_url = '" + newURL + "' WHERE (id = 1) AND (result_url = '" + old + "')")).
		WillReturnResult(ok1)
	if n := f.promoteFreeOriginals(context.Background()); n != 1 {
		t.Fatalf("promoted=%d", n)
	}
	met(t, mock)
	if !reflect.DeepEqual(media.copies, []string{wmRID + "|asset_library|studio-free-promote:" + wmRID}) {
		t.Fatalf("copies=%v", media.copies)
	}
	// 间隔内不再扫
	if n := f.promoteFreeOriginals(context.Background()); n != 0 {
		t.Fatal("间隔内不应再扫")
	}
	met(t, mock)
}

// 素材库改写失败：不改子任务（留着 mrw:// 下一轮重来）；关闭（PromoteMinutes<=0）时不扫。
func TestPromoteFreeOriginalsAssetFailureKeepsTask(t *testing.T) {
	f, mock, _ := newTestFlow(t, onBill())
	f.media = &copyMedia{}
	if f.promoteFreeOriginals(context.Background()) != 0 {
		t.Fatal("PromoteMinutes=0 应关闭")
	}
	f.cfg.FreeWatermark.PromoteMinutes = 10
	mock.ExpectQuery(regexp.QuoteMeta("t.result_url LIKE 'mrw://%'")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "task_id", "result_url"}).AddRow(1, 7, "simg_1", "mrw://"+wmRID+".png"))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE afv_asset")).WillReturnError(errors.New("db blip"))
	if n := f.promoteFreeOriginals(context.Background()); n != 0 {
		t.Fatalf("promoted=%d", n)
	}
	met(t, mock)
}

// 免费档用户把自己成品的签名水印地址当输入：反查 rid + 归属校验放行；没签过的 / 别人的成品 / 过期的一律拒。
func TestOwnsSignedWatermarkURL(t *testing.T) {
	d, mock := newTestDeps(t, &fakeBill{})
	now := time.Now()
	d.now = func() time.Time { return now }
	signed := "https://priv.oss.example.com/v1/work/2026/11/abc.png?x-oss-process=style%2Fwm&Signature=s1"
	indexSigned(wmRID, signed, 10*time.Minute, now)
	mock.ExpectQuery(regexp.QuoteMeta("FROM `afv_studio_task`")).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	// 同一对象换了签名参数也认（键不含查询串）
	if !d.ownsSigned(context.Background(), 7, "https://priv.oss.example.com/v1/work/2026/11/abc.png?x-oss-process=style%2Fwm&Signature=s2") {
		t.Fatal("自己的成品应放行")
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM `afv_studio_task`")).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	if d.ownsSigned(context.Background(), 8, signed) {
		t.Fatal("别人的成品不应放行")
	}
	if d.ownsSigned(context.Background(), 7, "https://priv.oss.example.com/v1/work/2026/11/other.png?Signature=x") {
		t.Fatal("没签过的地址不应放行")
	}
	d.now = func() time.Time { return now.Add(time.Hour) }
	if d.ownsSigned(context.Background(), 7, signed) {
		t.Fatal("过期的索引不应放行")
	}
	met(t, mock)
}

type delMedia struct {
	copyMedia
	deleted []string
}

func (m *delMedia) Delete(_ context.Context, rid, idem string) error {
	m.deleted = append(m.deleted, rid+"|"+idem)
	return nil
}

// 到期清理：默认关；开了才扫，只处理终态成功 / 未付费 / 超保留期的水印视图任务；删原件 → 删素材库行 → 子任务软删（CAS）。
func TestCleanupExpiredFreeOriginals(t *testing.T) {
	f, mock, _ := newTestFlow(t, onBill())
	media := &delMedia{}
	f.media = media
	var dropped []string
	f.dropAssets = func(_ context.Context, uid int64, tid string) error {
		dropped = append(dropped, fmt.Sprintf("%d|%s", uid, tid))
		return nil
	}
	if f.cleanupExpiredFreeOriginals(context.Background()) != 0 {
		t.Fatal("默认关闭，不应扫描")
	}
	f.cfg.FreeWatermark.CleanupEnabled, f.cfg.FreeWatermark.RetentionDays, f.cfg.FreeWatermark.CleanupMinutes = true, 90, 60
	old := "mrw://" + wmRID + ".png"
	mock.ExpectQuery(regexp.QuoteMeta("t.deleted = 0 AND t.status = 2 AND t.finished_at IS NOT NULL AND t.finished_at <= ")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "task_id", "result_url"}).AddRow(1, 7, "simg_1", old))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET deleted = 1 WHERE (id = 1) AND (result_url = '" + old + "') AND (deleted = 0)")).
		WillReturnResult(ok1)
	if n := f.cleanupExpiredFreeOriginals(context.Background()); n != 1 {
		t.Fatalf("cleaned=%d", n)
	}
	met(t, mock)
	if !reflect.DeepEqual(media.deleted, []string{wmRID + "|studio-free-expire:" + wmRID}) || !reflect.DeepEqual(dropped, []string{"7|simg_1"}) {
		t.Fatalf("deleted=%v dropped=%v", media.deleted, dropped)
	}
}
