// Package core 广场模板域的领域逻辑：行业目录、模板归类与可见性、偏好、行业建议、搜索缺口、示例素材、匿名报价，
// 以及后台的行业 / 模板 / 渠道 / 示例素材维护。
//
// 目前由 gateway-api 与 admin-api 进程内直接使用（设计里的 plaza-rpc 尚未拆出；这里不依赖任何 http / rpc 类型，
// 将来包一层 gRPC 即可）。设计：fusion-aigc/.scratch/agent-template-industry/DESIGN.md。
package core

import (
	"aigc-local-life/pkg/xerr"
)

// 细分 reason（码不变，只增；登记在 docs/api/api-plaza.md 的错误表）。
const (
	ReasonIndustryNotFound      = "INDUSTRY_NOT_FOUND"      // 4001：行业不存在 / 已停用 / 保留值 ALL / 二级不属于它
	ReasonIndustryHasTemplates  = "INDUSTRY_HAS_TEMPLATES"  // 4002：删除行业时仍有模板或子级且未给 reassignTo
	ReasonIndustryCodeDuplicate = "INDUSTRY_CODE_DUPLICATE" // 4002
	ReasonIndustryDepthExceeded = "INDUSTRY_DEPTH_EXCEEDED" // 1003
	ReasonQuoteUnavailable      = "QUOTE_UNAVAILABLE"       // 6001：报价源不可用且没有可用的缓存值
	ReasonTemplateNotFound      = "TEMPLATE_NOT_FOUND"      // 4001（后台）
	ReasonTemplateConflict      = "TEMPLATE_CONFLICT"       // 4002：他人已修改（updatedAt 校验）
	ReasonWriteDisabled         = "ADMIN_WRITE_DISABLED"    // 7003：模板写权属还在 Java（Plaza.AdminWriteEnabled=false）
	ReasonSuggestionRateLimited = "SUGGESTION_RATE_LIMITED" // 4002：同 sid / IP 当天超 3 条
	ReasonNotReady              = "PLAZA_NOT_READY"         // 7003：库没配
)

// ErrIndustryRequired 用户端接口缺 industry（或只给了 subIndustry）：HTTP 400。
func ErrIndustryRequired() error { return xerr.New(xerr.ParamMissing, "请先选择行业") }

// ErrIndustryNotFound 行业失效：前端据此清缓存回行业门。文案对应体验文案 G-10 的口径（行业名由前端补）。
func ErrIndustryNotFound() error {
	return xerr.NewReason(xerr.NotFound, ReasonIndustryNotFound, "这个行业已调整，请重新选择")
}

// ErrTemplateGone 用户端按 id 取模板：未发布 / 窗口外 / 已删除。文案 E-13。
func ErrTemplateGone() error {
	return xerr.New(xerr.NotFound, "这个模板刚刚下架了，看看相似的吧")
}

// ErrNotReady 库没配（DataSource 为空）。
func ErrNotReady() error {
	return xerr.NewReason(xerr.NotReady, ReasonNotReady, "功能暂未开放")
}

func errParam(msg string) error { return xerr.Param(msg) }
