package core

import (
	"context"
	"strconv"
	"strings"

	"aigc-local-life/pkg/xerr"
)

// AdminSkill 技能下拉项（只读 Java 的技能目录表 afv_compat_agent_skill；不含执行配置 config，它很大且与选择无关）。
type AdminSkill struct {
	AgentSkillID string  `json:"agentSkillId"` // 字符串：模板里 agentSkillId 是字符串
	Code         string  `json:"code"`
	Title        string  `json:"title"`
	Description  *string `json:"description"`
	Icon         *string `json:"icon"`
	Category     *string `json:"category"`
	Enabled      bool    `json:"enabled"`
}

type skillRow struct {
	AgentSkillID int64   `bun:"agent_skill_id"`
	Code         *string `bun:"agent_skill_code"`
	Title        string  `bun:"title"`
	Description  *string `bun:"description"`
	Icon         *string `bun:"icon"`
	Category     *string `bun:"category"`
	Enabled      int     `bun:"enabled"`
}

// AdminListSkills 技能下拉：按分类、权重排序，keyword 模糊匹配标题 / 编码。停用的技能也列出（enabled=false），
// 因为已绑定停用技能的模板需要在下拉里能回显；新绑定时由前端置灰。
func (s *Service) AdminListSkills(ctx context.Context, keyword string) ([]AdminSkill, error) {
	if err := s.requireDB(); err != nil {
		return nil, err
	}
	var rows []skillRow
	q := s.db.NewSelect().TableExpr("afv_compat_agent_skill").
		Column("agent_skill_id", "agent_skill_code", "title", "description", "icon", "category", "enabled").
		Where("deleted = 0")
	if kw := strings.TrimSpace(keyword); kw != "" {
		if runeLen(kw) > 64 {
			return nil, xerr.Param("keyword 最多 64 个字")
		}
		like := "%" + likeEscape(kw) + "%"
		q = q.Where("(title LIKE ? OR agent_skill_code LIKE ?)", like, like)
	}
	if err := q.OrderExpr("category IS NULL, category ASC, weight DESC, id ASC").Limit(500).Scan(ctx, &rows); err != nil {
		return nil, err
	}
	out := make([]AdminSkill, 0, len(rows))
	for _, r := range rows {
		code := ""
		if r.Code != nil {
			code = *r.Code
		}
		out = append(out, AdminSkill{AgentSkillID: strconv.FormatInt(r.AgentSkillID, 10), Code: code, Title: r.Title,
			Description: r.Description, Icon: r.Icon, Category: r.Category, Enabled: r.Enabled == 1})
	}
	return out, nil
}
