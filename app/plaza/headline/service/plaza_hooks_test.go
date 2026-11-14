package service

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/plaza/headline/algo"
	"aigc-local-life/app/plaza/headline/signals"
	"aigc-local-life/app/plaza/headline/store"
	"aigc-local-life/pkg/bundb"

	"github.com/uptrace/bun"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 行业 / 模板模块的三个头条钩子对真实 MySQL：PLAZA_TEST_DSN 指向已执行 Go_V1.0.33~35 的临时库（库名含 scratch / test），否则跳过。
func TestIntegrationPlazaHooks(t *testing.T) {
	dsn := os.Getenv("PLAZA_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 PLAZA_TEST_DSN，跳过")
	}
	if !strings.Contains(dsn, "scratch") && !strings.Contains(dsn, "test") {
		t.Fatalf("PLAZA_TEST_DSN 必须指向临时库：%s", dsn)
	}
	db, err := bundb.New(sqlx.NewMysql(dsn))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, tb := range []string{store.TableHeadline, store.TableBlock, store.TableSnapshotItem, store.TableSnapshot} {
		if _, err := db.ExecContext(ctx, "DELETE FROM "+tb); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	s := New(store.New(db), signals.New(nil), fakeCatalog{testCatalog()}, Conf{})
	const tpl = int64(910001)
	day := func(d int) store.CSTTime { return store.CSTTime{Time: now.AddDate(0, 0, d)} }
	for _, scope := range []string{"foot_bath", "*"} {
		r := &store.HeadlineRule{PlazaTemplateID: tpl, ScopeCode: scope, StartDate: day(-1), EndDate: day(3), Why: "运营推荐", Enabled: 1}
		if err := s.repo.InsertRule(ctx, db, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.repo.UpsertBlock(ctx, db, &store.Block{PlazaTemplateID: tpl, ScopeCode: "apparel", Kind: "CANCEL", Reason: "x"}); err != nil {
		t.Fatal(err)
	}
	snap := &store.Snapshot{ComputedAt: store.CSTTime{Time: now}, TriggerType: TriggerOps, ParamsHash: "h", ParamsJSON: []byte(`{}`),
		SignalSource: SourceDB, WindowStart: store.CSTTime{Time: now.Add(-time.Hour)}, WindowEnd: store.CSTTime{Time: now}}
	ver, err := s.repo.InsertSnapshot(ctx, snap, []algo.Item{{Scope: "foot_bath", TemplateID: tpl, Rank: 2, Source: "MANUAL", EnteredAt: now}})
	if err != nil {
		t.Fatal(err)
	}
	briefs, err := s.Briefs(ctx, []int64{tpl, 910002})
	if err != nil {
		t.Fatal(err)
	}
	b := briefs[tpl]
	if b == nil || !b.OnBoard || b.Version != ver || len(b.Scopes) != 1 || b.Scopes[0].Rank != 2 || b.Scopes[0].Name != "足浴" || b.ActiveRules != 2 || !b.Suppressed {
		t.Fatalf("头条摘要：%+v", b)
	}
	if o := briefs[910002]; o == nil || o.OnBoard || o.ActiveRules != 0 {
		t.Fatalf("不在榜的模板：%+v", o)
	}
	// 彻底删除：同一事务里规则逻辑删除、抑制物理删除；事务回滚则都不生效
	_ = db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := s.PurgeTemplate(ctx, tx, tpl); err != nil {
			t.Fatal(err)
		}
		return errRollback
	})
	if rules, _ := s.repo.RulesOfTemplate(ctx, tpl); len(rules) != 2 {
		t.Fatalf("回滚后规则仍在：%d", len(rules))
	}
	if err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error { return s.PurgeTemplate(ctx, tx, tpl) }); err != nil {
		t.Fatal(err)
	}
	if rules, _ := s.repo.RulesOfTemplate(ctx, tpl); len(rules) != 0 {
		t.Fatalf("规则应逻辑删除：%d", len(rules))
	}
	if blocks, _ := s.repo.ListBlocks(ctx, "", tpl, false, now); len(blocks) != 0 {
		t.Fatalf("抑制应删除：%d", len(blocks))
	}
	var deleted int
	_ = db.NewRaw("SELECT COUNT(*) FROM afv_plaza_headline WHERE plaza_template_id = ? AND deleted = 1", tpl).Scan(ctx, &deleted)
	if deleted != 2 {
		t.Fatalf("规则行保留并标记删除：%d", deleted)
	}
	// 详情联动：模板不在目录里 → nil（不报错）
	if v, err := s.StatusForAdmin(ctx, tpl); err != nil || v != nil {
		t.Fatalf("不在目录：%+v %v", v, err)
	}
	if v, err := s.StatusForAdmin(ctx, 1); err != nil || v == nil || v.Version != ver {
		t.Fatalf("在目录：%+v %v", v, err)
	}
}

type rollbackErr struct{}

func (rollbackErr) Error() string { return "rollback" }

var errRollback = rollbackErr{}
