package core

import (
	"context"
	"testing"

	"aigc-local-life/app/plaza/model"
	"aigc-local-life/pkg/xerr"
)

// 中：编辑权限不能绕过发布权限——已发布或已有行业的模板改行业关系要 plaza:publish；未发布且未归类的草稿不受影响。
func TestRequireIndustryRelationPerm(t *testing.T) {
	ctx := context.Background()
	editor := Operator{ID: 1}
	publisher := Operator{ID: 2, CanPublish: true}
	cases := []struct {
		name          string
		state         string
		before, after []int64
		needPublish   bool
	}{
		{"已发布模板挂新行业", model.StatePublished, nil, []int64{3}, true},
		{"已发布模板摘行业", model.StatePublished, []int64{3}, nil, true},
		{"已归类的草稿换行业", model.StateDraft, []int64{3}, []int64{4}, true},
		{"已归类的下线模板加行业", model.StateOffline, []int64{3}, []int64{3, 4}, true},
		{"未发布、未归类的草稿首次归类", model.StateDraft, nil, []int64{3}, false},
		{"关系没变化", model.StatePublished, []int64{3, 4}, []int64{4, 3}, false},
	}
	for _, c := range cases {
		tpl := &model.Template{ID: 9, State: c.state}
		err := requireIndustryRelationPerm(ctx, editor, tpl, c.before, c.after)
		if c.needPublish {
			if code, _, _ := xerr.Parse(err); err == nil || code != xerr.Forbidden {
				t.Errorf("%s：只有编辑权限应拒绝（Forbidden）：%v", c.name, err)
			}
		} else if err != nil {
			t.Errorf("%s：不需要发布权限：%v", c.name, err)
		}
		if err := requireIndustryRelationPerm(ctx, publisher, tpl, c.before, c.after); err != nil {
			t.Errorf("%s：有发布权限应放行：%v", c.name, err)
		}
	}
}
