package billing

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

var (
	offerCols   = []string{"offer_code", "entry_code", "feature_code"}
	variantCols = []string{"variant_code", "quality_tier", "default_model_id", "default_resolution", "default_duration_seconds", "settlement_type"}
	compCols    = []string{"component_code", "step_code", "component_type", "mode", "cost_type", "action_type", "model_id", "base_points", "price_multiplier"}
	priceCols   = []string{"id", "price_points", "price_unit", "resolution"}
	ruleCols    = []string{"rule_code", "scope_type", "scope_code", "factor_key"}

	qOffer      = regexp.QuoteMeta("FROM `afv_billing_offer` WHERE `entry_code` = ?")
	qByGroup    = regexp.QuoteMeta("JOIN `afv_ai_model` m ON m.`id` = v.`default_model_id` AND m.`deleted` = 0 WHERE v.`offer_code` = ?")
	qFirst      = regexp.QuoteMeta("FROM `afv_billing_offer_variant` WHERE `offer_code` = ? AND `status` = 'ENABLED' AND `deleted` = 0 ORDER BY `id` LIMIT 1")
	qComps      = regexp.QuoteMeta("FROM `afv_billing_charge_component` WHERE `variant_code` = ?")
	qRules      = regexp.QuoteMeta("FROM `afv_billing_charge_rule`")
	qModelPrice = regexp.QuoteMeta("FROM `afv_model_pricing` WHERE `model_id` = ? AND `action_type` = ?")
)

func mockConn(t *testing.T) (sqlx.SqlConn, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return sqlx.NewSqlConnFromDB(db), mock
}

// expectRest 选中档位之后的步骤 / 规则 / 单价查询：档位 variant 挂一个 CREAFT_V1 步骤，单价 points。
func expectRest(mock sqlmock.Sqlmock, variant string, modelID int64, res string, points int64) {
	mock.ExpectQuery(qComps).WithArgs(variant).WillReturnRows(sqlmock.NewRows(compCols).
		AddRow("gen_"+variant, "studio_tryon", "OUTPUT", 0, "BILLED", "CREAFT_V1", modelID, 0, 1.0))
	mock.ExpectQuery(qRules).WillReturnRows(sqlmock.NewRows(ruleCols))
	mock.ExpectQuery(qModelPrice).WithArgs(modelID, "CREAFT_V1", res).WillReturnRows(sqlmock.NewRows(priceCols).
		AddRow(1, points, "PER_IMAGE", res))
}

// 带模型组：按档位默认模型的 gateway_model_group 选中分模型档位，单价取该档位模型的分辨率行。
func TestPriceByModelGroup(t *testing.T) {
	conn, mock := mockConn(t)
	mock.ExpectQuery(qOffer).WithArgs("studio_tryon").WillReturnRows(sqlmock.NewRows(offerCols).AddRow("studio_tryon", "studio_tryon", "ecom_image_gen"))
	mock.ExpectQuery(qByGroup).WithArgs("studio_tryon", "img.gpt-image-2.6-sunburst").
		WillReturnRows(sqlmock.NewRows(variantCols).AddRow("studio_tryon_gpt_image_2_6", "standard", 60, nil, nil, "PARTIAL"))
	expectRest(mock, "studio_tryon_gpt_image_2_6", 60, "2K", 534)
	q, err := price(context.Background(), conn, Charge{UserID: 7, EntryCode: "studio_tryon", ModelGroup: "img.gpt-image-2.6-sunburst",
		Kind: KindQuick, Count: 1, Resolution: "2K"})
	if err != nil {
		t.Fatal(err)
	}
	if q.PricePoints != 534 || q.VariantCode != "studio_tryon_gpt_image_2_6" || q.ModelGroup != "img.gpt-image-2.6-sunburst" || q.ModelID != 60 {
		t.Fatalf("报价：%+v", q)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 该入口没有这个组的档位（未配分模型价 / 换代前的旧组）：不报错，退回第一个档位，与不带组时同价。
func TestPriceModelGroupFallsBackToDefaultVariant(t *testing.T) {
	conn, mock := mockConn(t)
	mock.ExpectQuery(qOffer).WithArgs("studio_tryon").WillReturnRows(sqlmock.NewRows(offerCols).AddRow("studio_tryon", "studio_tryon", "ecom_image_gen"))
	mock.ExpectQuery(qByGroup).WithArgs("studio_tryon", "img.gpt-image-2").WillReturnRows(sqlmock.NewRows(variantCols))
	mock.ExpectQuery(qFirst).WithArgs("studio_tryon").
		WillReturnRows(sqlmock.NewRows(variantCols).AddRow("studio_tryon_standard", "standard", 14, nil, nil, "PARTIAL"))
	expectRest(mock, "studio_tryon_standard", 14, "1K", 10)
	q, err := price(context.Background(), conn, Charge{UserID: 7, EntryCode: "studio_tryon", ModelGroup: "img.gpt-image-2",
		Kind: KindQuick, Count: 1, Resolution: "1K"})
	if err != nil {
		t.Fatal(err)
	}
	if q.PricePoints != 10 || q.VariantCode != "studio_tryon_standard" || q.ModelGroup != "" {
		t.Fatalf("退回默认档位：%+v", q)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 不带模型组（旧调用方）：不查分模型档位，直接取第一个档位，行为与改动前一致。
func TestPriceWithoutModelGroupUnchanged(t *testing.T) {
	conn, mock := mockConn(t)
	mock.ExpectQuery(qOffer).WithArgs("studio_pose").WillReturnRows(sqlmock.NewRows(offerCols).AddRow("studio_pose", "studio_pose", "ecom_image_gen"))
	mock.ExpectQuery(qFirst).WithArgs("studio_pose").
		WillReturnRows(sqlmock.NewRows(variantCols).AddRow("studio_pose_standard", "standard", 14, nil, nil, "PARTIAL"))
	expectRest(mock, "studio_pose_standard", 14, "4K", 20)
	q, err := price(context.Background(), conn, Charge{UserID: 7, EntryCode: "studio_pose", Kind: KindQuick, Count: 1, Resolution: "4K"})
	if err != nil {
		t.Fatal(err)
	}
	if q.PricePoints != 20 || q.VariantCode != "studio_pose_standard" {
		t.Fatalf("报价：%+v", q)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
