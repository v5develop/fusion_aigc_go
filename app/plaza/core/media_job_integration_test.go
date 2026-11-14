package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"aigc-local-life/app/plaza/model"
	"aigc-local-life/pkg/xerr"
)

// 视频处理流水线 / 渠道删除 / 审计检索的集成测试（需 PLAZA_TEST_DSN 指向已执行 Go_V1.0.46 的临时库，见 integration_test.go）。

func mediaTestUploader(cover CoverMaker) *Uploader {
	u := newTestUploader(&fakeStore{})
	u.cover = cover
	return u
}

func newVideoTemplate(t *testing.T, s *Service, title, video, poster string) int64 {
	t.Helper()
	payload := json.RawMessage(`{"result":["https://x.example.com/a.webp"],"prompt":"p"}`)
	id, err := s.AdminCreateTemplate(context.Background(), adm, TemplateInput{Title: title, MediaType: "video", VideoURL: video, PosterURL: poster, Payload: payload})
	return mustID(t, id, err)
}

type mediaRow struct {
	Status   string `bun:"media_status"`
	Attempts int    `bun:"media_attempts"`
	Poster   string `bun:"poster_url"`
	Video    string `bun:"video_url"`
}

func mediaOf(t *testing.T, s *Service, id int64) (mediaRow, *model.Template) {
	t.Helper()
	var tp model.Template
	if err := s.db.NewSelect().Model(&tp).Where("t.id = ?", id).Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	return mediaRow{Status: tp.MediaStatus, Attempts: tp.MediaAttempts, Poster: tp.PosterURL.String, Video: tp.VideoURL.String}, &tp
}

func execSQL(t *testing.T, s *Service, q string, args ...any) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatal(err)
	}
}

func mjAuditCount(t *testing.T, s *Service, action string, id int64) int {
	t.Helper()
	var n int
	if err := s.db.NewSelect().Model((*model.AuditLog)(nil)).ColumnExpr("COUNT(*)").Where("action = ?", action).Where("resource_id = ?", id).Scan(context.Background(), &n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 抽帧临时失败：退避 → 第 3 次转 FAILED（带原因、写审计）→ 「重新处理」→ 成功置 READY，封面与转码地址写回。
func TestIntegrationMediaJobRetryFailReprocess(t *testing.T) {
	s := integrationService(t)
	ctx := context.Background()
	var fail atomic.Bool
	fail.Store(true)
	var calls int32
	s.uploader = mediaTestUploader(fakeCoverFn(func(context.Context, string) (string, error) {
		atomic.AddInt32(&calls, 1)
		if fail.Load() {
			return "", errors.New("媒体服务 503")
		}
		return objURL("auto.jpg"), nil
	}))
	tc := &fakeTranscoder{url: objURL("t.mp4")}
	s.uploader.transcode = tc
	s.uploader.codec = func(context.Context, string) (*VideoCodec, error) {
		return &VideoCodec{Codec: "hevc", PixFmt: "yuv420p"}, nil
	}

	id := newVideoTemplate(t, s, "重试", objURL("v.mp4"), "")
	if r, _ := mediaOf(t, s, id); r.Status != model.MediaStatusProcessing {
		t.Fatalf("新建视频应 PROCESSING: %+v", r)
	}
	// 第 1 次失败：留在 PROCESSING，错误可见，进入退避，本轮立即再扫不会重试
	if n, err := s.RunMediaJobs(ctx); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	r, tp := mediaOf(t, s, id)
	if r.Status != model.MediaStatusProcessing || r.Attempts != 1 || !strings.Contains(tp.MediaError.String, "503") || !tp.MediaNextAt.Valid || tp.MediaLeaseToken.Valid {
		t.Fatalf("第 1 次失败应退避: %+v %+v", r, tp)
	}
	if n, _ := s.RunMediaJobs(ctx); n != 0 {
		t.Fatalf("退避期内不应重试: %d", n)
	}
	// 发布被挡：处理中
	if _, err := s.AdminPublishTemplate(ctx, adm, id, PublishInput{}); err == nil || !strings.Contains(err.Error(), "处理中") {
		t.Fatalf("PROCESSING 不应能发布: %v", err)
	}
	// 重新处理：正在处理中拒绝
	if _, err := s.AdminReprocessTemplateMedia(ctx, adm, id); err == nil {
		t.Fatal("PROCESSING 不应能重新处理")
	}
	// 第 2、3 次（把退避时间推到过去）→ FAILED
	for i := 2; i <= 3; i++ {
		execSQL(t, s, "UPDATE afv_compat_plaza_template SET media_next_at = DATE_SUB(NOW(), INTERVAL 1 SECOND) WHERE id = ?", id)
		if n, _ := s.RunMediaJobs(ctx); n != 1 {
			t.Fatalf("第 %d 次应处理 1 行: %d", i, n)
		}
	}
	r, tp = mediaOf(t, s, id)
	if r.Status != model.MediaStatusFailed || r.Attempts != 3 || !strings.Contains(tp.MediaError.String, "503") || tp.MediaNextAt.Valid {
		t.Fatalf("3 次失败应 FAILED: %+v %+v", r, tp)
	}
	if atomic.LoadInt32(&calls) != 3 || mjAuditCount(t, s, "TEMPLATE_MEDIA_FAILED", id) != 1 {
		t.Fatalf("应恰好尝试 3 次并写 1 条失败审计: calls=%d", calls)
	}
	if n, _ := s.RunMediaJobs(ctx); n != 0 {
		t.Fatalf("FAILED 不再被扫: %d", n)
	}
	det, _ := s.AdminGetTemplate(ctx, id)
	if det.MediaStatus != model.MediaStatusFailed || det.MediaError == nil || det.MediaAttempts != 3 {
		t.Fatalf("后台详情应带失败原因: %+v", det)
	}
	if lst, _ := s.AdminListTemplates(ctx, TemplateFilter{Page: 1, Size: 10}); len(lst.Items) != 1 || lst.Items[0].MediaStatus != model.MediaStatusFailed || lst.Items[0].MediaError == nil {
		t.Fatalf("后台列表应显示处理状态与原因: %+v", lst.Items)
	}
	if _, err := s.AdminPublishTemplate(ctx, adm, id, PublishInput{}); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("FAILED 不能发布且文案带原因: %v", err)
	}
	// 重新处理 → 排队、清空失败；这次成功
	fail.Store(false)
	wr, err := s.AdminReprocessTemplateMedia(ctx, adm, id)
	if err != nil || wr.Revision <= det.Revision {
		t.Fatalf("%+v %v", wr, err)
	}
	if r, tp = mediaOf(t, s, id); r.Status != model.MediaStatusProcessing || r.Attempts != 0 || tp.MediaError.Valid {
		t.Fatalf("重新处理应清空状态: %+v %+v", r, tp)
	}
	if n, _ := s.RunMediaJobs(ctx); n != 1 {
		t.Fatalf("n=%d", n)
	}
	r, tp = mediaOf(t, s, id)
	if r.Status != model.MediaStatusReady || r.Poster != objURL("auto.jpg") || r.Video != objURL("t.mp4") || !strings.Contains(tp.MediaNote.String, "转码") || tp.MediaError.Valid {
		t.Fatalf("成功应 READY 并写回封面 / 转码地址: %+v %+v", r, tp)
	}
	if mjAuditCount(t, s, "TEMPLATE_MEDIA_READY", id) != 1 || mjAuditCount(t, s, "TEMPLATE_MEDIA_REPROCESS", id) != 1 {
		t.Fatal("应写 READY / REPROCESS 审计")
	}
	if _, err := s.AdminPublishTemplate(ctx, adm, id, PublishInput{Force: true}); err != nil {
		t.Fatalf("READY 后应能发布: %v", err)
	}
	// 非视频模板不能重新处理
	img, err := s.AdminCreateTemplate(ctx, adm, TemplateInput{Title: "图", Payload: json.RawMessage(`{"result":["https://x.example.com/a.webp"]}`)})
	if _, err := s.AdminReprocessTemplateMedia(ctx, adm, mustID(t, img, err)); err == nil {
		t.Fatal("图片模板不需要处理")
	}
}

// 围栏：处理期间运营换了视频 → 旧结果丢弃，新视频保持 PROCESSING（attempts 清零），不会把旧视频的封面写给新视频。
func TestIntegrationMediaJobFenceOnVideoChange(t *testing.T) {
	s := integrationService(t)
	ctx := context.Background()
	id := newVideoTemplate(t, s, "围栏", "https://x.example.com/old.mp4", "")
	s.uploader = mediaTestUploader(fakeCoverFn(func(context.Context, string) (string, error) {
		det, _ := s.AdminGetTemplate(ctx, id) // 处理中途：运营整体编辑换了视频
		if _, err := s.AdminUpdateTemplate(ctx, adm, id, TemplateInput{Title: "围栏", MediaType: "video", VideoURL: "https://x.example.com/new.mp4",
			Payload: json.RawMessage(`{"result":["https://x.example.com/a.webp"],"prompt":"p"}`), UpdatedAt: det.UpdateTime, Revision: &det.Revision}); err != nil {
			t.Error(err)
		}
		return objURL("old-cover.jpg"), nil
	}))
	if n, _ := s.RunMediaJobs(ctx); n != 1 {
		t.Fatalf("n=%d", n)
	}
	r, tp := mediaOf(t, s, id)
	if r.Status != model.MediaStatusProcessing || r.Attempts != 0 || r.Poster != "" || r.Video != "https://x.example.com/new.mp4" || tp.MediaLeaseToken.Valid {
		t.Fatalf("旧结果应作废: %+v %+v", r, tp)
	}
	if mjAuditCount(t, s, "TEMPLATE_MEDIA_READY", id) != 0 {
		t.Fatal("作废的结果不应写 READY 审计")
	}
}

// 行租约：两个副本同时扫，每行只被处理一次。
func TestIntegrationMediaJobLeaseExclusive(t *testing.T) {
	s1 := integrationService(t)
	ctx := context.Background()
	s2 := New(s1.db, nil, s1.cfg)
	const rows = 4
	for i := 0; i < rows; i++ {
		newVideoTemplate(t, s1, "并发"+string(rune('A'+i)), objURL("v.mp4"), "")
	}
	var calls int32
	gate := make(chan struct{})
	cov := fakeCoverFn(func(context.Context, string) (string, error) {
		atomic.AddInt32(&calls, 1)
		<-gate
		return objURL("c.jpg"), nil
	})
	s1.uploader, s2.uploader = mediaTestUploader(cov), mediaTestUploader(cov)
	var wg sync.WaitGroup
	for _, s := range []*Service{s1, s2} {
		wg.Add(1)
		go func(s *Service) {
			defer wg.Done()
			_, _ = s.RunMediaJobs(ctx)
		}(s)
	}
	close(gate)
	wg.Wait()
	if got := atomic.LoadInt32(&calls); got != rows {
		t.Fatalf("每行只应被处理一次（两个副本竞争）: calls=%d want %d", got, rows)
	}
	var n int
	if err := s1.db.NewSelect().Model((*model.Template)(nil)).ColumnExpr("COUNT(*)").Where("media_status = 'READY'").Where("media_attempts = 1").Scan(ctx, &n); err != nil || n != rows {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

// 崩溃留下的行：租约过期且次数已满 → reap 成 FAILED；租约未过期的不动。
func TestIntegrationMediaJobReapExhausted(t *testing.T) {
	s := integrationService(t)
	ctx := context.Background()
	s.uploader = mediaTestUploader(fakeCover{url: objURL("c.jpg")})
	dead := newVideoTemplate(t, s, "崩溃", objURL("v.mp4"), "")
	live := newVideoTemplate(t, s, "在跑", objURL("v.mp4"), "")
	execSQL(t, s, "UPDATE afv_compat_plaza_template SET media_attempts = 3, media_lease_until = DATE_SUB(NOW(), INTERVAL 1 MINUTE), media_lease_token = 'dead' WHERE id = ?", dead)
	execSQL(t, s, "UPDATE afv_compat_plaza_template SET media_attempts = 3, media_lease_until = DATE_ADD(NOW(), INTERVAL 10 MINUTE), media_lease_token = 'live' WHERE id = ?", live)
	if n, err := s.RunMediaJobs(ctx); err != nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if r, tp := mediaOf(t, s, dead); r.Status != model.MediaStatusFailed || !strings.Contains(tp.MediaError.String, "中断") || tp.MediaLeaseToken.Valid {
		t.Fatalf("%+v %+v", r, tp)
	}
	if r, _ := mediaOf(t, s, live); r.Status != model.MediaStatusProcessing {
		t.Fatalf("租约未过期的不应动: %+v", r)
	}
	// 租约过期但次数未满：被重新认领（崩溃恢复）
	crash := newVideoTemplate(t, s, "崩溃一次", objURL("v.mp4"), "")
	execSQL(t, s, "UPDATE afv_compat_plaza_template SET media_attempts = 1, media_lease_until = DATE_SUB(NOW(), INTERVAL 1 MINUTE), media_lease_token = 'old' WHERE id = ?", crash)
	if n, _ := s.RunMediaJobs(ctx); n != 1 {
		t.Fatalf("n=%d", n)
	}
	if r, _ := mediaOf(t, s, crash); r.Status != model.MediaStatusReady || r.Attempts != 2 {
		t.Fatalf("%+v", r)
	}
}

// 写权属开关关闭（交接前 Java 是写者）：任务不动数据。
func TestIntegrationMediaJobRespectsWriteGate(t *testing.T) {
	s := integrationService(t)
	id := newVideoTemplate(t, s, "闸", objURL("v.mp4"), "")
	s.cfg.AdminWriteEnabled = false
	if n, err := s.RunMediaJobs(context.Background()); err != nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if r, _ := mediaOf(t, s, id); r.Status != model.MediaStatusProcessing || r.Attempts != 0 {
		t.Fatalf("%+v", r)
	}
	if _, err := s.AdminReprocessTemplateMedia(context.Background(), adm, id); err == nil {
		t.Fatal("交接前不应能重新处理")
	}
}

func TestIntegrationChannelDelete(t *testing.T) {
	s := integrationService(t)
	ctx := context.Background()
	mk := func(code string) int64 {
		id, err := s.AdminCreateChannel(ctx, adm, ChannelInput{Code: code, Name: sp(code), Width: intp(1080), Height: intp(1920), Ratio: sp("9:16")})
		return mustID(t, id, err)
	}
	used, trashed, free := mk("ch_used"), mk("ch_trash"), mk("ch_free")
	payload := json.RawMessage(`{"result":["https://x.example.com/a.webp"],"prompt":"p"}`)
	t1, err := s.AdminCreateTemplate(ctx, adm, TemplateInput{Title: "用渠道", Payload: payload, Channels: json.RawMessage(`["ch_used",{"code":"ch_trash","width":720,"height":1280}]`)})
	t1 = mustID(t, t1, err)
	t2, err := s.AdminCreateTemplate(ctx, adm, TemplateInput{Title: "对象写法", Payload: payload, Channels: json.RawMessage(`[{"code":"ch_used","width":720,"height":1280}]`)})
	t2 = mustID(t, t2, err)
	if err := s.AdminDeleteTemplate(ctx, adm, t2); err != nil { // 进回收站：仍算引用（恢复后会生效）
		t.Fatal(err)
	}

	lst, err := s.AdminListChannels(ctx)
	if err != nil || len(lst) != 3 {
		t.Fatalf("%+v %v", lst, err)
	}
	cnt := map[string][2]int{}
	for _, c := range lst {
		cnt[c.Code] = [2]int{c.TemplateCount, c.TrashCount}
	}
	if cnt["ch_used"] != [2]int{2, 1} || cnt["ch_trash"] != [2]int{1, 0} || cnt["ch_free"] != [2]int{0, 0} {
		t.Fatalf("引用数: %v", cnt)
	}

	// 被引用：拒绝并带引用数 / 回收站数 / 示例模板
	err = s.AdminDeleteChannel(ctx, adm, used)
	if codeOf(err) != xerr.Conflict || !strings.Contains(err.Error(), "2 个模板") || !strings.Contains(err.Error(), "1 个在回收站") {
		t.Fatalf("被引用应拒绝并列出引用数: %v", err)
	}
	if err := s.AdminDeleteChannel(ctx, adm, trashed); codeOf(err) != xerr.Conflict {
		t.Fatalf("%v", err)
	}
	// 未引用：逻辑删除（行还在，deleted=1），列表与保存校验都不再认它，code 不可复用
	if err := s.AdminDeleteChannel(ctx, adm, free); err != nil {
		t.Fatal(err)
	}
	var ch model.Channel
	if err := s.db.NewSelect().Model(&ch).Where("ch.id = ?", free).Scan(ctx); err != nil || ch.Deleted != 1 || !ch.DeletedAt.Valid || ch.Enabled != 0 {
		t.Fatalf("应逻辑删除: %+v %v", ch, err)
	}
	if lst, _ := s.AdminListChannels(ctx); len(lst) != 2 {
		t.Fatalf("删除后列表: %+v", lst)
	}
	if _, err := s.AdminCreateTemplate(ctx, adm, TemplateInput{Title: "引用已删渠道", Payload: payload, Channels: json.RawMessage(`["ch_free"]`)}); codeOf(err) != xerr.ParamError {
		t.Fatalf("已删除渠道不可再被引用: %v", err)
	}
	if _, err := s.AdminCreateChannel(ctx, adm, ChannelInput{Code: "ch_free", Name: sp("x"), Width: intp(1), Height: intp(1), Ratio: sp("1:1")}); codeOf(err) != xerr.Conflict {
		t.Fatalf("code 不可复用: %v", err)
	}
	if err := s.AdminUpdateChannel(ctx, adm, free, ChannelInput{Name: sp("改")}); codeOf(err) != xerr.NotFound {
		t.Fatalf("已删除的不可编辑: %v", err)
	}
	if err := s.AdminDeleteChannel(ctx, adm, free); codeOf(err) != xerr.NotFound {
		t.Fatalf("重复删除: %v", err)
	}
	if mjAuditCount(t, s, "CHANNEL_DELETE", free) != 1 || mjAuditCount(t, s, "CHANNEL_DELETE", used) != 0 {
		t.Fatal("只有真正删除的才写审计")
	}
	// 模板改投其它渠道后，原渠道可删
	if _, err := s.AdminUpdateTemplate(ctx, adm, t1, TemplateInput{Title: "用渠道", Payload: payload, Channels: json.RawMessage(`["ch_used"]`), UpdatedAt: updOf(t, s, t1)}); err != nil {
		t.Fatal(err)
	}
	if err := s.AdminDeleteChannel(ctx, adm, trashed); err != nil {
		t.Fatalf("不再被引用后应可删: %v", err)
	}
}

func TestIntegrationAuditSearch(t *testing.T) {
	s := integrationService(t)
	ctx := context.Background()
	// 临时库里可能已有 Java 建的真表（password_hash NOT NULL 无默认）：先删再建精简 stub（结束时本就 DROP）
	execSQL(t, s, "DROP TABLE IF EXISTS afv_admin_user")
	execSQL(t, s, "CREATE TABLE afv_admin_user (id BIGINT PRIMARY KEY, username VARCHAR(64) NOT NULL, real_name VARCHAR(64) NULL, deleted TINYINT NOT NULL DEFAULT 0)")
	execSQL(t, s, "DELETE FROM afv_admin_user")
	execSQL(t, s, "INSERT INTO afv_admin_user (id, username, real_name) VALUES (101, 'zhangsan', '张三'), (102, 'lisi', '李四'), (103, 'wang_5', NULL)")
	t.Cleanup(func() { _, _ = s.db.ExecContext(ctx, "DROP TABLE IF EXISTS afv_admin_user") })
	// 审计行：101 记了姓名；102 的 operator_name 为空（只有 id，靠关联管理员表）；103 只有 username
	rows := []struct {
		op            Operator
		action, rtype string
		rid, batch    string
	}{
		{Operator{ID: 101, Name: "张三"}, "TEMPLATE_UPDATE", ResTemplate, "1", ""},
		{Operator{ID: 101, Name: "张三"}, "TEMPLATE_PUBLISH", ResTemplate, "2", "batch-aaa"},
		{Operator{ID: 102}, "TEMPLATE_PUBLISH", ResTemplate, "3", "batch-aaa"},
		{Operator{ID: 103}, "CHANNEL_CREATE", ResChannel, "4", ""},
		{Operator{ID: 0, Name: "system"}, "TEMPLATE_MEDIA_READY", ResTemplate, "5", ""},
	}
	for _, r := range rows {
		if err := s.audit(ctx, s.db, r.op, r.action, r.rtype, r.rid, r.batch, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	count := func(f AuditFilter) int {
		t.Helper()
		p, err := s.AdminAudit(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		return p.Total
	}
	for name, c := range map[string]struct {
		f    AuditFilter
		want int
	}{
		"姓名模糊（审计行里的名字）":             {AuditFilter{OperatorName: "张"}, 2},
		"姓名模糊（关联管理员表 real_name）":    {AuditFilter{OperatorName: "李四"}, 1},
		"姓名模糊（关联管理员表 username）":     {AuditFilter{OperatorName: "lis"}, 1},
		"username 里的下划线按字面匹配":       {AuditFilter{OperatorName: "wang_"}, 1},
		"下划线不当通配（wang5 不命中 wang_5）": {AuditFilter{OperatorName: "wang5"}, 0},
		"系统操作人":             {AuditFilter{OperatorName: "system"}, 1},
		"姓名无匹配":             {AuditFilter{OperatorName: "不存在"}, 0},
		"action 精确":         {AuditFilter{Action: "TEMPLATE_PUBLISH"}, 2},
		"action 精确不当前缀":     {AuditFilter{Action: "TEMPLATE"}, 0},
		"action 前缀通配":       {AuditFilter{Action: "TEMPLATE_*"}, 4},
		"action 后缀通配":       {AuditFilter{Action: "*_PUBLISH"}, 2},
		"action 包含通配":       {AuditFilter{Action: "*MEDIA*"}, 1},
		"resourceType 前缀通配": {AuditFilter{ResourceType: ResTemplate[:3] + "*"}, 4},
		"resourceType 精确":   {AuditFilter{ResourceType: ResChannel}, 1},
		"批次 id":             {AuditFilter{BatchID: "batch-aaa"}, 2},
		"批次 id 不匹配":         {AuditFilter{BatchID: "batch-aa"}, 0},
		"组合：批次 + 姓名":        {AuditFilter{BatchID: "batch-aaa", OperatorName: "李"}, 1},
		"组合：姓名 + action 通配": {AuditFilter{OperatorName: "张三", Action: "*_UPDATE"}, 1},
	} {
		if got := count(c.f); got != c.want {
			t.Errorf("%s: got %d want %d", name, got, c.want)
		}
	}
}
