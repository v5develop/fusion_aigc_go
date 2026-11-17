package admin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/app/replicate/repo"
	"aigc-local-life/pkg/bundb"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/xerr"

	"github.com/uptrace/bun"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 管理端投稿审核流 + 真库仓储的集成测试（REPLICATE_TEST_DSN，库名须含 scratch / test；未设置时跳过）。

const (
	shITUser  = 990079
	shITAdmin = 770002
	shITMark  = "https://share-admin-it.example/"
)

type itMedia struct {
	calls []string
	fail  bool
}

func (m *itMedia) CopyToLibrary(_ context.Context, rid, _ string, idem string) (mediasvc.Resource, error) {
	m.calls = append(m.calls, rid)
	if m.fail {
		return mediasvc.Resource{}, errors.New("boom")
	}
	return mediasvc.Resource{RID: "mr://lib/" + strings.TrimPrefix(rid, "mr://")}, nil
}

func shITDB(t *testing.T) *bun.DB {
	t.Helper()
	dsn := os.Getenv("REPLICATE_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 REPLICATE_TEST_DSN，跳过")
	}
	if !strings.Contains(dsn, "scratch") && !strings.Contains(dsn, "test") {
		t.Fatalf("REPLICATE_TEST_DSN 必须指向临时库：%s", dsn)
	}
	db, err := bundb.New(sqlx.NewMysql(dsn))
	if err != nil {
		t.Fatal(err)
	}
	clean := func() {
		_, _ = db.ExecContext(ctx, "DELETE FROM afv_replicate_sample WHERE title LIKE 'ITA-%'")
		_, _ = db.ExecContext(ctx, "DELETE FROM afv_replicate_share WHERE user_id = ?", shITUser)
		_, _ = db.ExecContext(ctx, "DELETE FROM afv_replicate_analysis WHERE user_id = 0 AND source_url LIKE ?", shITMark+"%")
		_, _ = db.ExecContext(ctx, "DELETE FROM afv_replicate_analysis WHERE user_id = ?", shITUser)
		_, _ = db.ExecContext(ctx, "DELETE FROM afv_replicate_plan WHERE user_id = ?", shITUser)
		_, _ = db.ExecContext(ctx, "DELETE FROM afv_replicate_job WHERE user_id = ?", shITUser)
	}
	clean()
	t.Cleanup(clean)
	return db
}

const shITAnn = `{"dims":{"model":{"mode":"replace","refs":[1],"note":"私人备注"},"scene":{"mode":"describe","note":"私人场景"}},` +
	`"audio":"revoice","texts":[{"text":"我的店名"}],"voice":{"voiceId":5,"lines":[{"index":1,"text":"私人台词"}]}}`

// shITSeed 种数据并返回投稿号；docJSON 是 job.result_document。
func shITSeed(t *testing.T, db *bun.DB, tag, docJSON string) (*repo.Repo, int64) {
	t.Helper()
	r := repo.New(db)
	src := &model.Analysis{AnalysisNo: "VRA-ITA-" + tag, UserID: shITUser, SourceKey: "ita-src-" + tag, SourceURL: shITMark + tag,
		SourceRid: ptrS("mr://src-" + tag), Status: model.AnalysisSucceeded, Stage: model.StageDone, Attempt: 1,
		State: json.RawMessage(`{"motion":[1]}`), Result: json.RawMessage(`{"summary":"分析摘要` + tag + `"}`)}
	if _, _, err := r.InsertAnalysis(ctx, src); err != nil {
		t.Fatal(err)
	}
	plan, _, err := r.InsertPlan(ctx, &model.Plan{PlanNo: "VRP-ITA-" + tag, UserID: shITUser, AnalysisID: src.ID, Status: model.PlanConfirmed,
		ModelGroup: "m", Resolution: "720p", AspectRatio: "9:16", PlanHash: "ita-h-" + tag, ExpiresAt: time.Now().Add(time.Hour),
		Annotations: json.RawMessage(shITAnn), Segments: json.RawMessage(`[]`), Prompts: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	res, err := db.ExecContext(ctx, "INSERT INTO afv_replicate_job (user_id, request_id, plan_id, plan_no, plan_hash, status, input, seg_total, result_document) "+
		"VALUES (?, ?, ?, 'p', 'h', ?, '{}', 1, ?)", shITUser, "ita-"+tag, plan.ID, model.JobSucceeded, docJSON)
	if err != nil {
		t.Fatal(err)
	}
	jobID, _ := res.LastInsertId()
	sh := &model.Share{UserID: shITUser, JobID: jobID, PlanID: plan.ID, Status: model.SharePending, Industry: "ecom", RewardStatus: model.RewardNone}
	if ok, err := r.InsertShare(ctx, sh); err != nil || !ok {
		t.Fatal(ok, err)
	}
	return r, sh.ID
}

func ptrS(s string) *string { return &s }

const shITDoc = `{"status":"ready","kind":"full","videoRid":"mr://res-%s","coverRid":"mr://cov-%s"}`

func shITSvc(r *repo.Repo, m ShareMedia, now time.Time) *Service {
	s := New(r, nil, nil)
	s.SetShareMedia(m)
	s.now = func() time.Time { return now }
	return s
}

func reasonOf(err error) string {
	var xe *xerr.CodeError
	if errors.As(err, &xe) {
		return xe.Reason
	}
	return ""
}

func TestIntegrationAdminShareApproveRejectFlow(t *testing.T) {
	db := shITDB(t)
	doc := strings.ReplaceAll(shITDoc, "%s", "a")
	r, id := shITSeed(t, db, "a", doc)
	m := &itMedia{}
	s := shITSvc(r, m, time.Now())

	// 媒体复制失败：整条通过失败，投稿保持待审核，没有示例
	m.fail = true
	if _, err := s.ApproveShare(ctx, id, shITAdmin, "ITA-t"); err == nil {
		t.Fatal("复制失败应整条失败")
	}
	if cur, _ := r.GetShare(ctx, id); cur.Status != model.SharePending || cur.SampleID != nil {
		t.Fatalf("失败后应仍待审核：%+v", cur)
	}
	m.fail = false

	row, err := s.ApproveShare(ctx, id, shITAdmin, "ITA-title")
	if err != nil || row.Status != model.ShareApproved {
		t.Fatalf("通过：%+v %v", row, err)
	}
	cur, _ := r.GetShare(ctx, id)
	if cur.SampleID == nil || cur.ReviewedBy != shITAdmin || cur.ReviewedAt == nil {
		t.Fatalf("投稿行：%+v", cur)
	}
	var sm model.Sample
	if err := db.NewSelect().Model(&sm).Where("sm.id = ?", *cur.SampleID).Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if !sm.Enabled || sm.Title != "ITA-title" || sm.Industry != "ecom" || deref(sm.ResultRid) != "mr://lib/res-a" || deref(sm.CoverRid) != "mr://lib/cov-a" ||
		sm.SourceRid != "mr://src-a" || sm.CreatedBy != shITAdmin || sm.AnalysisID == nil {
		t.Fatalf("示例：%+v", sm)
	}
	cp, _ := r.GetAnalysis(ctx, *sm.AnalysisID)
	if cp == nil || cp.UserID != model.SystemUserID || cp.SourceURL != shITMark+"a" || !strings.Contains(string(cp.Result), "分析摘要a") {
		t.Fatalf("分析副本：%+v", cp)
	}
	for _, bad := range []string{"refs", "note", "私人", "texts", "我的店名", "voice", "lines"} {
		if strings.Contains(string(sm.Annotations), bad) {
			t.Fatalf("示例改法含用户内容 %q：%s", bad, sm.Annotations)
		}
	}
	var gotAnn struct {
		Audio string `json:"audio"`
	}
	if json.Unmarshal(sm.Annotations, &gotAnn) != nil || gotAnn.Audio != "regenerate" {
		t.Fatalf("revoice 应退回默认配乐：%s", sm.Annotations)
	}

	// 重复点通过：直接返回，不再复制媒体、不新增示例
	calls := len(m.calls)
	if again, err := s.ApproveShare(ctx, id, shITAdmin, "ITA-other"); err != nil || again.Status != model.ShareApproved || len(m.calls) != calls {
		t.Fatalf("重复通过：%+v %v calls %d→%d", again, err, calls, len(m.calls))
	}
	var n int
	_ = db.NewRaw("SELECT COUNT(*) FROM afv_replicate_sample WHERE title LIKE 'ITA-%'").Scan(ctx, &n)
	if n != 1 {
		t.Fatalf("示例数 %d", n)
	}
	// 通过后驳回：不行
	if _, err := s.RejectShare(ctx, id, shITAdmin, "晚了"); reasonOf(err) != ReasonShareNotPending {
		t.Fatalf("已通过不能驳回：%v", err)
	}
}

func TestIntegrationAdminShareRejectAndWithdrawAfterApprove(t *testing.T) {
	db := shITDB(t)
	r, id := shITSeed(t, db, "b", strings.ReplaceAll(shITDoc, "%s", "b"))
	s := shITSvc(r, &itMedia{}, time.Now())

	if _, err := s.RejectShare(ctx, id, shITAdmin, "  "); err == nil {
		t.Fatal("原因必填")
	}
	row, err := s.RejectShare(ctx, id, shITAdmin, "不合规")
	if err != nil || row.Status != model.ShareRejected {
		t.Fatalf("驳回：%+v %v", row, err)
	}
	if again, err := s.RejectShare(ctx, id, shITAdmin, "不合规"); err != nil || again.Status != model.ShareRejected { // 同原因幂等
		t.Fatalf("同原因重复驳回：%v", err)
	}
	if _, err := s.RejectShare(ctx, id, shITAdmin, "换个原因"); reasonOf(err) != ReasonShareNotPending {
		t.Fatalf("不同原因应拒：%v", err)
	}
	if _, err := s.ApproveShare(ctx, id, shITAdmin, ""); reasonOf(err) != ReasonShareNotPending {
		t.Fatalf("已驳回不能通过：%v", err)
	}

	// 通过后用户撤回：示例下架；再审核 / 发奖励都不行
	r2, id2 := shITSeed(t, db, "b2", strings.ReplaceAll(shITDoc, "%s", "b2"))
	s2 := shITSvc(r2, &itMedia{}, time.Now())
	if _, err := s2.ApproveShare(ctx, id2, shITAdmin, "ITA-b2"); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := r2.WithdrawShare(ctx, shITUser, id2); err != nil || !changed {
		t.Fatal(changed, err)
	}
	var n int
	_ = db.NewRaw("SELECT COUNT(*) FROM afv_replicate_sample WHERE title = 'ITA-b2' AND deleted = 0 AND enabled = 1").Scan(ctx, &n)
	if n != 0 {
		t.Fatal("撤回后示例应下架")
	}
	if _, err := s2.ApproveShare(ctx, id2, shITAdmin, ""); reasonOf(err) != ReasonShareNotPending {
		t.Fatalf("已撤回不能通过：%v", err)
	}
	if _, err := s2.ClaimReward(ctx, id2, shITAdmin); reasonOf(err) != ReasonShareNotApproved {
		t.Fatalf("已撤回不能发奖励：%v", err)
	}
	// 成片不是完整版：拒绝
	r3, id3 := shITSeed(t, db, "b3", `{"status":"ready","kind":"preview","videoRid":"mr://x"}`)
	if _, err := shITSvc(r3, &itMedia{}, time.Now()).ApproveShare(ctx, id3, shITAdmin, ""); err == nil {
		t.Fatal("预览版不能通过")
	}
}

type itRewardSrc struct{ v RewardSettings }

func (s *itRewardSrc) Load(context.Context) RewardSettings { return s.v }

func TestIntegrationAdminShareRewardTwoPhase(t *testing.T) {
	db := shITDB(t)
	now := time.Now()
	var ids []int64
	var r *repo.Repo
	for i, tag := range []string{"r1", "r2", "r3", "r4"} {
		var id int64
		r, id = shITSeed(t, db, tag, strings.ReplaceAll(shITDoc, "%s", tag))
		if _, err := shITSvc(r, &itMedia{}, now).ApproveShare(ctx, id, shITAdmin, "ITA-"+tag); err != nil {
			t.Fatal(i, err)
		}
		ids = append(ids, id)
	}
	src := &itRewardSrc{RewardSettings{Enabled: true, Points: 30, MonthlyLimit: 3}}
	s := shITSvc(r, &itMedia{}, now)
	s.SetRewardSettings(src)

	// 关闭后再占位：不占，状态不变
	src.v.Enabled = false
	if _, err := s.ClaimReward(ctx, ids[0], shITAdmin); reasonOf(err) != ReasonRewardDisabled {
		t.Fatalf("关闭应拒占位：%v", err)
	}
	if cur, _ := r.GetShare(ctx, ids[0]); cur.RewardStatus != model.RewardNone {
		t.Fatalf("关闭时不该占位：%+v", cur)
	}
	if _, err := s.MarkRewardGranted(ctx, ids[0], shITAdmin); reasonOf(err) != ReasonRewardNotClaimed {
		t.Fatalf("没占位不能标记：%v", err)
	}
	src.v.Enabled = true

	// 占位 → 占位后运营关闭奖励 / 改点数 → 重试占位沿用快照 → mark 仍成功
	c, err := s.ClaimReward(ctx, ids[0], shITAdmin)
	if err != nil || c.RewardStatus != model.RewardGranting || c.RewardPoints != 30 || c.Granted {
		t.Fatalf("占位：%+v %v", c, err)
	}
	src.v.Enabled, src.v.Points = false, 88
	if again, err := s.ClaimReward(ctx, ids[0], shITAdmin); err != nil || again.RewardPoints != 30 || again.RewardStatus != model.RewardGranting {
		t.Fatalf("发放中重试占位：%+v %v", again, err)
	}
	row, err := s.MarkRewardGranted(ctx, ids[0], shITAdmin)
	if err != nil || row.RewardStatus != model.RewardGranted || row.RewardReplay || row.RewardPoints != 30 {
		t.Fatalf("关闭后 mark 发放中的应成功：%+v %v", row, err)
	}
	if row, err := s.MarkRewardGranted(ctx, ids[0], shITAdmin); err != nil || !row.RewardReplay {
		t.Fatalf("mark 重放：%+v %v", row, err)
	}
	src.v = RewardSettings{Enabled: true, Points: 30, MonthlyLimit: 3}

	// 月上限：发放 + 发放中都占额度；release 让出
	for _, id := range ids[1:3] {
		if _, err := s.ClaimReward(ctx, id, shITAdmin); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ClaimReward(ctx, ids[3], shITAdmin); reasonOf(err) != ReasonRewardMonthlyLimit {
		t.Fatalf("第 4 条应达上限：%v", err)
	}
	rel, err := s.ReleaseReward(ctx, ids[2], shITAdmin)
	if err != nil || rel.RewardStatus != model.RewardNone || rel.MonthRewardCount != 2 {
		t.Fatalf("release：%+v %v", rel, err)
	}
	if _, err := s.ReleaseReward(ctx, ids[0], shITAdmin); reasonOf(err) != ReasonRewardAlreadyGranted {
		t.Fatalf("已发放不能 release：%v", err)
	}
	if _, err := s.ClaimReward(ctx, ids[3], shITAdmin); err != nil {
		t.Fatalf("让出额度后应可占位：%v", err)
	}
	// 次月额度重置
	next := shITSvc(r, &itMedia{}, now.AddDate(0, 1, 0))
	next.SetRewardSettings(src)
	if _, err := next.ClaimReward(ctx, ids[2], shITAdmin); err != nil {
		t.Fatalf("次月应可占位：%v", err)
	}
	if _, err := s.ClaimReward(ctx, 987654321, shITAdmin); err == nil {
		t.Fatal("不存在的投稿应报错")
	}
}
