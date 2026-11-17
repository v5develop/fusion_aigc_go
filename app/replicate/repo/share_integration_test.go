package repo

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/app/replicate/model"

	"github.com/uptrace/bun"
)

// 投稿审核事务的真库集成测试（REPLICATE_TEST_DSN，库名须含 scratch / test；未设置时跳过）。

const (
	shareITUser  = 990078
	shareITAdmin = 770001
	shareITMark  = "https://share-it.example/" // 系统分析行的 source_url 标记，清理用
	shareITTitle = "IT-share"
)

// shareITAnnotations 用户的改法：含参考图 / 备注 / 叠字 / 换声音，通过后示例里必须全部去掉。
const shareITAnnotations = `{"dims":{"model":{"mode":"replace","refs":[1,2],"note":"私人备注A"},` +
	`"garment":{"mode":"describe","note":"私人备注B","occurrences":[{"index":1,"refs":[3]}]},` +
	`"scene":{"mode":"keep","note":"x"},"line":{"mode":"keep"}},` +
	`"audio":"revoice","texts":[{"text":"我的店名"}],"voice":{"voiceId":5,"lines":[{"index":1,"text":"私人台词"}]}}`

func shareITDB(t *testing.T) *bun.DB {
	t.Helper()
	_, db := integrationRepo(t) // 复用 DSN 检查（含 scratch / test 才放行）
	clean := func() {
		_, _ = db.ExecContext(ctx, "DELETE FROM afv_replicate_sample WHERE title LIKE ?", shareITTitle+"%")
		_, _ = db.ExecContext(ctx, "DELETE FROM afv_replicate_share WHERE user_id = ?", shareITUser)
		_, _ = db.ExecContext(ctx, "DELETE FROM afv_replicate_analysis WHERE user_id = 0 AND source_url LIKE ?", shareITMark+"%")
		_, _ = db.ExecContext(ctx, "DELETE FROM afv_replicate_analysis WHERE user_id = ?", shareITUser)
		_, _ = db.ExecContext(ctx, "DELETE FROM afv_replicate_plan WHERE user_id = ?", shareITUser)
		_, _ = db.ExecContext(ctx, "DELETE FROM afv_replicate_job WHERE user_id = ?", shareITUser)
	}
	clean()
	t.Cleanup(clean)
	return db
}

// shareITSeed 种一条「用户已出完整成片并投稿」的数据，返回仓储、投稿、源分析。
func shareITSeed(t *testing.T, db *bun.DB, tag string) (*Repo, *model.Share, *model.Analysis) {
	t.Helper()
	r := New(db)
	src := &model.Analysis{AnalysisNo: "VRA-IT-" + tag, UserID: shareITUser, SourceKey: "it-src-" + tag, SourceURL: shareITMark + tag,
		SourceRid: ptr("mr://src-" + tag), Status: model.AnalysisSucceeded, Stage: model.StageDone, Attempt: 1,
		State: json.RawMessage(`{"motion":[1,2,3]}`), Result: json.RawMessage(`{"summary":"摘要` + tag + `"}`)}
	if _, _, err := r.InsertAnalysis(ctx, src); err != nil {
		t.Fatal(err)
	}
	plan, _, err := r.InsertPlan(ctx, &model.Plan{PlanNo: "VRP-IT-" + tag, UserID: shareITUser, AnalysisID: src.ID, Status: model.PlanConfirmed,
		ModelGroup: "m", Resolution: "720p", AspectRatio: "9:16", PlanHash: "it-h-" + tag, ExpiresAt: time.Now().Add(time.Hour),
		Annotations: json.RawMessage(shareITAnnotations), Segments: json.RawMessage(`[]`), Prompts: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	doc := `{"status":"ready","kind":"full","videoRid":"mr://user-result-` + tag + `","coverRid":"mr://user-cover-` + tag + `"}`
	res, err := db.ExecContext(ctx, "INSERT INTO afv_replicate_job (user_id, request_id, plan_id, plan_no, plan_hash, status, input, seg_total, result_document) "+
		"VALUES (?, ?, ?, 'p', 'h', ?, '{}', 1, ?)", shareITUser, "it-share-"+tag, plan.ID, model.JobSucceeded, doc)
	if err != nil {
		t.Fatal(err)
	}
	jobID, _ := res.LastInsertId()
	if _, err := db.ExecContext(ctx, "UPDATE afv_replicate_plan SET job_id = ? WHERE id = ?", jobID, plan.ID); err != nil {
		t.Fatal(err)
	}
	sh := &model.Share{UserID: shareITUser, JobID: jobID, PlanID: plan.ID, Status: model.SharePending, PortraitConsent: true, Industry: "ecom", RewardStatus: model.RewardNone}
	if ok, err := r.InsertShare(ctx, sh); err != nil || !ok {
		t.Fatalf("InsertShare ok=%v err=%v", ok, err)
	}
	return r, sh, src
}

func ptr[T any](v T) *T { return &v }

func shareITSample(src *model.Analysis, p *model.Plan, tag string) *model.Sample {
	return &model.Sample{Title: shareITTitle + "-" + tag, Industry: "ecom", SourceRid: *src.SourceRid, ResultRid: ptr("mr://lib-result-" + tag),
		CoverRid: ptr("mr://lib-cover-" + tag), Annotations: core.ShareSampleAnnotations(p.Annotations), Enabled: true, CreatedBy: shareITAdmin, UpdatedBy: shareITAdmin}
}

func shareITApprove(t *testing.T, r *Repo, sh *model.Share, src *model.Analysis, tag string) (*model.Share, bool, *model.Sample, error) {
	t.Helper()
	p, err := r.GetPlan(ctx, sh.PlanID)
	if err != nil || p == nil {
		t.Fatalf("GetPlan %v %v", p, err)
	}
	smp := shareITSample(src, p, tag)
	got, ok, err := r.ApproveShare(ctx, sh.ID, shareITAdmin, core.NewSharedAnalysis(sh.ID, src), smp)
	return got, ok, smp, err
}

func TestIntegrationApproveShare(t *testing.T) {
	db := shareITDB(t)
	r, sh, src := shareITSeed(t, db, "a")

	got, ok, smp, err := shareITApprove(t, r, sh, src, "a")
	if err != nil || !ok {
		t.Fatalf("应通过：ok=%v err=%v", ok, err)
	}
	if got.Status != model.ShareApproved || got.SampleID == nil || *got.SampleID != smp.ID {
		t.Fatalf("返回的投稿行不对：%+v", got)
	}

	// 投稿行（重新读库）：状态 / sample_id / 审核人 / 审核时间一起落地
	db2, _ := r.GetShare(ctx, sh.ID)
	if db2.Status != model.ShareApproved || db2.SampleID == nil || *db2.SampleID != smp.ID || db2.ReviewedBy != shareITAdmin || db2.ReviewedAt == nil || db2.RejectReason != "" {
		t.Fatalf("投稿行：%+v", db2)
	}
	if db2.RewardStatus != model.RewardNone || db2.RewardAt != nil {
		t.Fatalf("通过不该动奖励：%+v", db2)
	}

	// 示例：启用、指向系统身份的分析副本，不是用户的那行
	var sm model.Sample
	if err := db.NewSelect().Model(&sm).Where("sm.id = ?", smp.ID).Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if !sm.Enabled || sm.Deleted || sm.AnalysisID == nil || *sm.AnalysisID == src.ID || sm.SourceRid != *src.SourceRid || deref(sm.ResultRid) != "mr://lib-result-a" {
		t.Fatalf("示例：%+v", sm)
	}
	cp, err := r.GetAnalysis(ctx, *sm.AnalysisID)
	if err != nil || cp == nil {
		t.Fatalf("分析副本：%v %v", cp, err)
	}
	if cp.UserID != model.SystemUserID || cp.ID == src.ID || cp.Status != model.AnalysisSucceeded || cp.Stage != model.StageDone ||
		deref(cp.SourceRid) != *src.SourceRid || !strings.Contains(string(cp.State), "motion") || !strings.Contains(string(cp.Result), "摘要a") {
		t.Fatalf("分析副本应为系统身份的整行复制：%+v", cp)
	}
	if o, _ := r.GetAnalysis(ctx, src.ID); o == nil || o.UserID != shareITUser {
		t.Fatalf("用户自己的分析行不该被动：%+v", o)
	}

	// 去用户内容：参考图 / 备注 / 逐套换装 / 叠字 / 台词都不在；revoice → 默认配乐
	raw := string(sm.Annotations)
	for _, bad := range []string{"refs", "note", "私人", "occurrences", "texts", "我的店名", "voice", "lines"} {
		if strings.Contains(raw, bad) {
			t.Fatalf("示例改法含用户内容 %q：%s", bad, raw)
		}
	}
	var ann core.Annotations
	if err := json.Unmarshal(sm.Annotations, &ann); err != nil {
		t.Fatal(err)
	}
	if len(ann.Dims) != 2 || ann.Dims["model"].Mode != core.ModeReplace || ann.Dims["garment"].Mode != core.ModeDescribe || ann.Audio != core.AudioRegenerate {
		t.Fatalf("示例改法：%s", raw)
	}
}

func TestIntegrationApproveShareIdempotentAndConcurrent(t *testing.T) {
	db := shareITDB(t)
	r, sh, src := shareITSeed(t, db, "b")

	// 并发点两次：只通过一次，只有一个示例
	var wg sync.WaitGroup
	oks := make([]bool, 2)
	errs := make([]error, 2)
	for i := range oks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, oks[i], _, errs[i] = shareITApprove(t, r, sh, src, fmt.Sprintf("b%d", i))
		}()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || oks[0] == oks[1] {
		t.Fatalf("并发通过应恰好一个成功：oks=%v errs=%v", oks, errs)
	}
	var n int
	_ = db.NewRaw("SELECT COUNT(*) FROM afv_replicate_sample WHERE title LIKE ?", shareITTitle+"-b%").Scan(ctx, &n)
	if n != 1 {
		t.Fatalf("示例应只有 1 条，实际 %d", n)
	}
	_ = db.NewRaw("SELECT COUNT(*) FROM afv_replicate_analysis WHERE user_id = 0 AND source_url = ?", shareITMark+"b").Scan(ctx, &n)
	if n != 1 {
		t.Fatalf("系统分析副本应只有 1 行，实际 %d", n)
	}

	// 已通过后再点：不通过、不新增，返回已通过的行
	got, ok, _, err := shareITApprove(t, r, sh, src, "b-again")
	if err != nil || ok || got == nil || got.Status != model.ShareApproved {
		t.Fatalf("重复通过：got=%+v ok=%v err=%v", got, ok, err)
	}
	_ = db.NewRaw("SELECT COUNT(*) FROM afv_replicate_sample WHERE title LIKE ?", shareITTitle+"-b%").Scan(ctx, &n)
	if n != 1 {
		t.Fatalf("重复通过不该多建示例：%d", n)
	}

	// 找不到的投稿
	if g, ok, err := r.ApproveShare(ctx, 987654321, shareITAdmin, core.NewSharedAnalysis(987654321, src), shareITSample(src, &model.Plan{}, "x")); err != nil || ok || g != nil {
		t.Fatalf("不存在：%v %v %v", g, ok, err)
	}
}

// 事务原子：示例插入失败（标题超长）时，分析副本与投稿状态一起回滚。
func TestIntegrationApproveShareRollsBackOnSampleFailure(t *testing.T) {
	db := shareITDB(t)
	r, sh, src := shareITSeed(t, db, "c")
	p, _ := r.GetPlan(ctx, sh.PlanID)
	smp := shareITSample(src, p, "c")
	smp.Title = shareITTitle + strings.Repeat("x", 500)
	if _, ok, err := r.ApproveShare(ctx, sh.ID, shareITAdmin, core.NewSharedAnalysis(sh.ID, src), smp); err == nil || ok {
		t.Fatalf("应失败回滚：ok=%v err=%v", ok, err)
	}
	cur, _ := r.GetShare(ctx, sh.ID)
	var n int
	_ = db.NewRaw("SELECT COUNT(*) FROM afv_replicate_analysis WHERE user_id = 0 AND source_url = ?", shareITMark+"c").Scan(ctx, &n)
	if cur.Status != model.SharePending || cur.SampleID != nil || cur.ReviewedBy != 0 || n != 0 {
		t.Fatalf("回滚后应原样：%+v analysisCopies=%d", cur, n)
	}
}

// 分析副本已存在（上次通过到一半 / 撤回后重投再通过）：复用已有那行，不重复建。
func TestIntegrationApproveShareReopenReusesAnalysisCopy(t *testing.T) {
	db := shareITDB(t)
	r, sh, src := shareITSeed(t, db, "d")
	_, ok, smp1, err := shareITApprove(t, r, sh, src, "d1")
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	// 撤回：示例下架，投稿 withdrawn
	prev, changed, err := r.WithdrawShare(ctx, shareITUser, sh.ID)
	if err != nil || !changed || prev.Status != model.ShareApproved {
		t.Fatalf("撤回：%+v %v %v", prev, changed, err)
	}
	var sm model.Sample
	if err := db.NewSelect().Model(&sm).Where("sm.id = ?", smp1.ID).Scan(ctx); err != nil || sm.Enabled || !sm.Deleted {
		t.Fatalf("撤回后示例应停用 + 逻辑删除：%+v %v", sm, err)
	}
	cur, _ := r.GetShare(ctx, sh.ID)
	if cur.Status != model.ShareWithdrawn {
		t.Fatalf("撤回后：%+v", cur)
	}
	// 撤回后审核通过：不是 pending，不动
	if _, ok, _, err := shareITApprove(t, r, sh, src, "d-w"); err != nil || ok {
		t.Fatalf("已撤回的不能通过：ok=%v err=%v", ok, err)
	}
	// 再撤回一次：没有变化
	if _, changed, err := r.WithdrawShare(ctx, shareITUser, sh.ID); err != nil || changed {
		t.Fatalf("重复撤回：%v %v", changed, err)
	}
	// 别人撤不了
	if p, changed, err := r.WithdrawShare(ctx, shareITUser+1, sh.ID); err != nil || changed || p != nil {
		t.Fatalf("非本人：%v %v %v", p, changed, err)
	}
	// 重新投 → 再通过
	if ok, err := r.ReopenShare(ctx, sh.ID, true, "ecom"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if c, _ := r.GetShare(ctx, sh.ID); c.Status != model.SharePending || c.SampleID != nil || c.ReviewedBy != 0 || c.ReviewedAt != nil {
		t.Fatalf("重投应清审核痕迹：%+v", c)
	}
	got, ok, smp2, err := shareITApprove(t, r, sh, src, "d2")
	if err != nil || !ok || smp2.ID == smp1.ID || got.SampleID == nil || *got.SampleID != smp2.ID {
		t.Fatalf("重投后通过：%+v ok=%v err=%v", got, ok, err)
	}
	var n int
	_ = db.NewRaw("SELECT COUNT(*) FROM afv_replicate_analysis WHERE user_id = 0 AND source_url = ?", shareITMark+"d").Scan(ctx, &n)
	if n != 1 || smp2.AnalysisID == nil || smp1.AnalysisID == nil || *smp1.AnalysisID != *smp2.AnalysisID {
		t.Fatalf("两次通过应共用一行系统分析副本：n=%d %v %v", n, smp1.AnalysisID, smp2.AnalysisID)
	}
}

func TestIntegrationRejectShare(t *testing.T) {
	db := shareITDB(t)
	r, sh, src := shareITSeed(t, db, "e")
	if ok, err := r.RejectShare(ctx, sh.ID, shareITAdmin, "画面不合规"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	cur, _ := r.GetShare(ctx, sh.ID)
	if cur.Status != model.ShareRejected || cur.RejectReason != "画面不合规" || cur.ReviewedBy != shareITAdmin || cur.ReviewedAt == nil || cur.SampleID != nil {
		t.Fatalf("驳回后：%+v", cur)
	}
	if ok, _ := r.RejectShare(ctx, sh.ID, shareITAdmin, "再来"); ok {
		t.Fatal("已驳回的不能再驳回（CAS）")
	}
	if _, ok, _, err := shareITApprove(t, r, sh, src, "e"); err != nil || ok {
		t.Fatalf("已驳回的不能通过：ok=%v err=%v", ok, err)
	}
	var n int
	_ = db.NewRaw("SELECT COUNT(*) FROM afv_replicate_sample WHERE title LIKE ?", shareITTitle+"-e%").Scan(ctx, &n)
	if n != 0 {
		t.Fatalf("驳回不该建示例：%d", n)
	}
	// 驳回的不可撤回
	if _, changed, _ := r.WithdrawShare(ctx, shareITUser, sh.ID); changed {
		t.Fatal("已驳回不改")
	}
}

func TestIntegrationGrantShareReward(t *testing.T) {
	db := shareITDB(t)
	r := New(db)
	now := time.Now()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	to := from.AddDate(0, 1, 0)

	// 5 条已通过的投稿 + 1 条待审核（同一用户）
	var ids []int64
	for i := 0; i < 5; i++ {
		sh := &model.Share{UserID: shareITUser, JobID: int64(880000 + i), PlanID: 1, Status: model.ShareApproved, RewardStatus: model.RewardNone}
		if ok, err := r.InsertShare(ctx, sh); err != nil || !ok {
			t.Fatal(ok, err)
		}
		ids = append(ids, sh.ID)
	}
	pend := &model.Share{UserID: shareITUser, JobID: 880009, PlanID: 1, Status: model.SharePending, RewardStatus: model.RewardNone}
	if _, err := r.InsertShare(ctx, pend); err != nil {
		t.Fatal(err)
	}

	grant := func(id int64) (*model.Share, RewardOutcome) {
		sh, oc, err := r.GrantShareReward(ctx, id, 3, from, to, now)
		if err != nil {
			t.Fatal(err)
		}
		return sh, oc
	}
	if _, oc := grant(pend.ID); oc != RewardNotApproved {
		t.Fatalf("待审核的不能发：%v", oc)
	}
	if _, oc := grant(987654321); oc != RewardNotFound {
		t.Fatalf("不存在：%v", oc)
	}
	for i := 0; i < 3; i++ {
		if sh, oc := grant(ids[i]); oc != RewardGranted || sh.RewardStatus != model.RewardGranted || sh.RewardAt == nil {
			t.Fatalf("第 %d 次应发放：%v %+v", i, oc, sh)
		}
	}
	if _, oc := grant(ids[0]); oc != RewardAlready { // 幂等重放，不占额度
		t.Fatalf("重放：%v", oc)
	}
	if _, oc := grant(ids[3]); oc != RewardLimitHit {
		t.Fatalf("第 4 次应达上限：%v", oc)
	}
	if cur, _ := r.GetShare(ctx, ids[3]); cur.RewardStatus != model.RewardNone || cur.RewardAt != nil {
		t.Fatalf("被拒的不该落奖励：%+v", cur)
	}
	if m, err := r.RewardCounts(ctx, []int64{shareITUser}, from, to); err != nil || m[shareITUser] != 3 {
		t.Fatalf("本月计数：%v %v", m, err)
	}
	// 上个月的区间里没有（reward_at 在本月）
	if m, _ := r.RewardCounts(ctx, []int64{shareITUser}, from.AddDate(0, -1, 0), from); m[shareITUser] != 0 {
		t.Fatalf("上月不该有：%v", m)
	}
	// 次月额度重新计：同一投稿换个月窗口可以发
	nf, nt := to, to.AddDate(0, 1, 0)
	if _, oc, err := r.GrantShareReward(ctx, ids[3], 3, nf, nt, to.Add(time.Hour)); err != nil || oc != RewardGranted {
		t.Fatalf("次月应可发：%v %v", oc, err)
	}

	// 并发：同一用户剩下的一条与另一条同时抢最后额度（limit=5：已发 4 条，只剩 1 个名额）
	var wg sync.WaitGroup
	got := make([]RewardOutcome, 2)
	extra := &model.Share{UserID: shareITUser, JobID: 880010, PlanID: 1, Status: model.ShareApproved, RewardStatus: model.RewardNone}
	if _, err := r.InsertShare(ctx, extra); err != nil {
		t.Fatal(err)
	}
	for i, id := range []int64{ids[4], extra.ID} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, oc, err := r.GrantShareReward(ctx, id, 4, from, to, now)
			if err != nil {
				t.Error(err)
			}
			got[i] = oc
		}()
	}
	wg.Wait()
	if !(got[0] == RewardGranted && got[1] == RewardLimitHit) && !(got[1] == RewardGranted && got[0] == RewardLimitHit) {
		t.Fatalf("并发抢最后一个名额应恰好一个成功：%v", got)
	}
	var n int
	_ = db.NewRaw("SELECT COUNT(*) FROM afv_replicate_share WHERE user_id = ? AND reward_status = 'granted' AND reward_at >= ? AND reward_at < ?",
		shareITUser, msExpr(from), msExpr(to)).Scan(ctx, &n)
	if n != 4 {
		t.Fatalf("本月应恰好 4 条已发放：%d", n)
	}
}
