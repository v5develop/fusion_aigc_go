package billing

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"aigc-local-life/pkg/auditdb"
)

// 迁到 sqlc 之前 audit.go 手写的两条 SQL（原文）。迁移后语句只允许在引号 / 空白上不同，语义与参数必须一致。
const (
	legacyModelGroupsSQL = "SELECT DISTINCT m.`gateway_model_group` FROM `afv_billing_offer` o " +
		"JOIN `afv_billing_offer_variant` v ON v.`offer_code` = o.`offer_code` AND v.`status` = 'ENABLED' AND v.`deleted` = 0 " +
		"JOIN `afv_ai_model` m ON m.`id` = v.`default_model_id` AND m.`deleted` = 0 " +
		"WHERE o.`entry_code` = ? AND o.`status` = 'ENABLED' AND o.`deleted` = 0 " +
		"AND m.`gateway_model_group` IS NOT NULL AND m.`gateway_model_group` <> '' ORDER BY 1"
	legacyIssuedCountSQL = "SELECT COUNT(*) FROM `afv_app_user_feature` WHERE `feature_code` = ? AND `status` = 'ACTIVE' AND `deleted` = 0 " +
		"AND (`expire_time` IS NULL OR `expire_time` > NOW())"
)

// normSQL 去掉反引号并折叠空白，便于与 sqlc 生成的多行语句逐字比对。
func normSQL(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "`", "")), " ")
}

// exactSQL 把原文 SQL 转成整句匹配的正则：sqlc 生成的语句只是去引号、换行，其余逐字相同。
func exactSQL(legacy string) string {
	words := strings.Fields(normSQL(legacy))
	for i, w := range words {
		words[i] = regexp.QuoteMeta(w)
	}
	return `(?s)^\s*(?:--[^\n]*\s*)*` + strings.Join(words, `\s+`) + `\s*$`
}

func TestAuditSQLCModelGroupsSameAsLegacy(t *testing.T) {
	conn, mock := mockConn(t)
	aq, err := auditdb.NewFromSqlConn(conn)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(exactSQL(legacyModelGroupsSQL)).WithArgs("studio_tryon").
		WillReturnRows(sqlmock.NewRows([]string{"gateway_model_group"}).AddRow("img.a").AddRow("img.b"))
	got, err := aq.BillingAuditModelGroups(context.Background(), "studio_tryon")
	if err != nil || len(got) != 2 || got[0].String != "img.a" || got[1].String != "img.b" {
		t.Fatalf("got=%v err=%v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAuditSQLCActiveFeatureCountSameAsLegacy(t *testing.T) {
	conn, mock := mockConn(t)
	aq, _ := auditdb.NewFromSqlConn(conn)
	mock.ExpectQuery(exactSQL(legacyIssuedCountSQL)).WithArgs("ecom_image_gen").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))
	n, err := aq.BillingAuditActiveFeatureCount(context.Background(), "ecom_image_gen")
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// auditGroups 行为不变：没有分模型档位 -> WARN；查询失败 -> ERROR。
func TestAuditGroupsNoGroupsAndError(t *testing.T) {
	tg := Target{Entry: "studio_tryon", Name: "商拍换装", Kind: KindQuick}
	conn, mock := mockConn(t)
	aq, _ := auditdb.NewFromSqlConn(conn)
	mock.ExpectQuery(exactSQL(legacyModelGroupsSQL)).WithArgs("studio_tryon").
		WillReturnRows(sqlmock.NewRows([]string{"gateway_model_group"}))
	fs := auditGroups(context.Background(), conn, aq, tg)
	if len(fs) != 1 || fs[0].Level != LevelWarn || fs[0].Tier != "组 -" {
		t.Fatalf("无分模型档位应为 WARN：%+v", fs)
	}

	conn, mock = mockConn(t)
	aq, _ = auditdb.NewFromSqlConn(conn)
	mock.ExpectQuery(exactSQL(legacyModelGroupsSQL)).WithArgs("studio_tryon").WillReturnError(context.DeadlineExceeded)
	fs = auditGroups(context.Background(), conn, aq, tg)
	if len(fs) != 1 || fs[0].Level != LevelError || !strings.Contains(fs[0].Detail, "查分模型档位失败") {
		t.Fatalf("查询失败应为 ERROR：%+v", fs)
	}
}
