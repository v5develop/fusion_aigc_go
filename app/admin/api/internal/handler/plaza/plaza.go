// Package plaza 广场模板域后台接口（/api/v1/plaza/admin/*）：行业、模板、渠道、示例素材、行业建议、搜索缺口、审计。
//
// 手写路由（不走 admin.api 的 goctl 生成），响应沿用 Go 默认信封（pkg/result，HTTP 恒 200，业务看 code）。
// 权限：前缀映射 creaft_catalog:view / edit；发布 / 删除 / 审计走路由级覆盖（middleware.routePerm，迁移 Go_V1.0.36）。
// 模板写接口受 Plaza.AdminWriteEnabled 保护（交接前 Java 仍是 afv_compat_plaza_template 的写者）；
// 只写 Go 独占表 / 列的「A 类」写（行业关系、行业内排序、slots、渠道 / 媒体类型、示例素材）交接期也放行，见 docs/api/api-plaza.md。
// 设计：fusion-aigc/.scratch/agent-template-industry/DESIGN.md 5.2、6 节。
package plaza

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"aigc-local-life/app/admin/api/internal/middleware"
	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/plaza/core"
	"aigc-local-life/pkg/clientip"
	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/result"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/rest"
	"github.com/zeromicro/go-zero/rest/httpx"
	"github.com/zeromicro/go-zero/rest/pathvar"
)

const prefix = "/api/v1/plaza/admin"

// Register 注册后台路由（全部挂 AdminAuth）。
func Register(server *rest.Server, c *svc.ServiceContext) {
	h := &handlers{c: c}
	r := func(m, p string, fn http.HandlerFunc) rest.Route {
		return rest.Route{Method: m, Path: prefix + p, Handler: fn}
	}
	g, po, pu, de := http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete
	server.AddRoutes(rest.WithMiddlewares([]rest.Middleware{c.AdminAuth},
		// 行业总开关（数据库 > 配置文件）：查看 = view，改 = edit
		r(g, "/industry-switch", h.industrySwitchGet),
		r(pu, "/industry-switch", h.industrySwitchSet),
		r(de, "/industry-switch", h.industrySwitchReset),
		// 行业
		r(g, "/industries", h.industryTree),
		r(po, "/industries", h.industryCreate),
		r(pu, "/industries/sort", h.industrySort),
		r(pu, "/industries/:id", h.industryUpdate),
		r(de, "/industries/:id", h.industryDelete),
		r(g, "/industries/:id/templates", h.industryTemplates),
		r(pu, "/industries/:id/templates/order", h.industryOrder),
		// 模板
		r(g, "/templates", h.templateList),
		r(po, "/templates", h.templateCreate),
		r(g, "/templates/trash", h.templateTrash),
		r(g, "/templates/unassigned-summary", h.unassignedSummary),
		r(po, "/templates/batch/:action", h.templateBatch),
		r(g, "/templates/:id", h.templateGet),
		r(pu, "/templates/:id", h.templateUpdate),
		r(pu, "/templates/:id/slots", h.templateSlots),
		r(pu, "/templates/:id/industries", h.templateIndustries),
		r(pu, "/templates/:id/attrs", h.templateAttributes),
		r(de, "/templates/:id", h.templateDelete),
		r(po, "/templates/:id/duplicate", h.templateDuplicate),
		r(po, "/templates/:id/reprocess-media", h.templateReprocessMedia),
		r(po, "/templates/:id/publish", h.templatePublish),
		r(po, "/templates/:id/offline", h.templateOffline),
		r(po, "/templates/:id/restore", h.templateRestore),
		r(de, "/templates/:id/purge", h.templatePurge),
		r(g, "/templates/:id/audit", h.templateAudit),
		r(g, "/capabilities", h.capabilities),
		// 模板编辑器：素材直传（公有桶）与技能下拉
		r(po, "/uploads/presign", h.uploadPresign),
		r(po, "/uploads/confirm", h.uploadConfirm),
		r(g, "/skills", h.skillList),
		r(g, "/audit", h.audit),
		r(g, "/preview/templates", h.preview),
		// 渠道字典
		r(g, "/channels", h.channelList),
		r(po, "/channels", h.channelCreate),
		r(pu, "/channels/:id", h.channelUpdate),
		r(de, "/channels/:id", h.channelDelete),
		// 示例素材
		r(g, "/samples", h.sampleList),
		r(po, "/samples", h.sampleCreate),
		r(pu, "/samples/:id", h.sampleUpdate),
		r(de, "/samples/:id", h.sampleDelete),
		// 行业建议 / 搜索缺口
		r(g, "/industry-suggestions", h.suggestionList),
		r(po, "/industry-suggestions/:normText/adopt", h.suggestionAdopt),
		r(po, "/industry-suggestions/:normText/ignore", h.suggestionIgnore),
		r(g, "/search-misses", h.missList),
		r(po, "/search-misses/status", h.missStatus),
		// 营销节点
		r(g, "/nodes", h.nodeList),
		r(po, "/nodes", h.nodeCreate),
		r(g, "/nodes/:id", h.nodeGet),
		r(pu, "/nodes/:id", h.nodeUpdate),
		r(pu, "/nodes/:id/enabled", h.nodeEnabled),
		r(de, "/nodes/:id", h.nodeDelete),
		r(g, "/nodes/:id/templates", h.nodeTemplates),
		r(po, "/nodes/:id/templates", h.nodeTemplateAdd),
		r(pu, "/nodes/:id/templates/order", h.nodeTemplateOrder),
		r(de, "/nodes/:id/templates/:templateId", h.nodeTemplateRemove),
	))
}

type handlers struct{ c *svc.ServiceContext }

// operatorName 审计 operator_name：鉴权中间件只往上下文放管理员 id（没有名字），这里按 id 查管理员表，
// 取 real_name，没有则 username。查不到 / 出错留空（审计仍有 operator_id），不影响本次写操作。
func (h *handlers) operatorName(r *http.Request, id int64) string {
	if id <= 0 || h.c.AdminUser == nil {
		return ""
	}
	a, err := h.c.AdminUser.FindByID(r.Context(), id)
	if err != nil {
		logx.WithContext(r.Context()).Errorf("plaza: 审计取管理员名失败 判据=afv_admin_user 查询出错 id=%d，operator_name 留空: %v", id, err)
		return ""
	}
	if a == nil {
		return ""
	}
	if a.RealName.Valid && strings.TrimSpace(a.RealName.String) != "" {
		return a.RealName.String
	}
	return a.Username
}

// do 统一包装：取服务、操作人，调用 fn，按默认信封写出。
func (h *handlers) do(fn func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.c.Plaza == nil {
			httpx.ErrorCtx(r.Context(), w, core.ErrNotReady())
			return
		}
		op := core.Operator{ID: ctxdata.AdminID(r.Context()), IP: clientIP(r), RequestID: r.Header.Get("X-Request-Id")}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			op.Name = h.operatorName(r, op.ID) // 只有写请求会落审计，读请求不多查一次
		}
		out, err := fn(w, r, h.c.Plaza, op)
		if err != nil {
			writeErr(w, r, err)
			return
		}
		httpx.OkJsonCtx(r.Context(), w, out)
	}
}

// writeErr 默认信封；入参 schema 校验失败（*core.SlotsError）额外在 data 里带 {field, errors}，前端据 field 定位到表单项。
func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	if se := core.AsSlotsError(err); se != nil {
		httpx.WriteJsonCtx(r.Context(), w, http.StatusOK, result.Body{Code: xerr.ParamError, Reason: se.Reason, Msg: se.Msg, Data: se.Data()})
		return
	}
	httpx.ErrorCtx(r.Context(), w, err)
}

// withPublishPerm 改行业关系的接口：中间件只要求 creaft_catalog:edit（未发布草稿归类不需要发布权限），
// 这里查当前管理员是否有 plaza:publish 填进 op.CanPublish，由 core 在事务内按模板状态（已发布 / 已有行业）决定是否必须。
func withPublishPerm(r *http.Request, op core.Operator) (core.Operator, error) {
	ok, err := middleware.Allowed(r.Context(), core.PermPublish)
	if err != nil {
		return op, xerr.New(xerr.ServerError, "权限校验失败")
	}
	op.CanPublish = ok
	return op, nil
}

type fnT = func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error)

func (h *handlers) wrap(fn fnT) http.HandlerFunc { return h.do(fn) }

// clientIP 调用方 IP：只信最近一跳可信代理写的头（X-Forwarded-For 首段可伪造），见 pkg/clientip。
func clientIP(r *http.Request) string { return clientip.From(r) }

func idParam(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(pathvar.Vars(r)["id"], 10, 64)
	if err != nil || id <= 0 {
		return 0, xerr.Param("id 不合法")
	}
	return id, nil
}

func decode(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		return xerr.Param("读取请求体失败")
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, v); err != nil {
		return xerr.Param("请求体不是合法 JSON")
	}
	return nil
}

func qInt(q url.Values, k string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(q.Get(k)))
	return n
}

func qInt64(q url.Values, k string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(q.Get(k)), 10, 64)
	return n
}

func qBool(q url.Values, k string) bool {
	b, _ := strconv.ParseBool(q.Get(k))
	return b
}

// ---------------------------------------------------------------- 行业

func (h *handlers) industryTree(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, _ core.Operator) (any, error) {
		return s.AdminIndustryTree(r.Context())
	})(w, r)
}

func (h *handlers) industryCreate(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error) {
		var in core.IndustryInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		id, err := s.AdminCreateIndustry(r.Context(), op, in)
		return map[string]any{"id": id}, err
	})(w, r)
}

func (h *handlers) industryUpdate(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error) {
		id, err := idParam(r)
		if err != nil {
			return nil, err
		}
		var in core.IndustryInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return s.AdminUpdateIndustry(r.Context(), op, id, in)
	})(w, r)
}

func (h *handlers) industrySort(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error) {
		var items []core.SortItem
		if err := decode(r, &items); err != nil {
			return nil, err
		}
		return nil, s.AdminSortIndustries(r.Context(), op, items)
	})(w, r)
}

func (h *handlers) industryDelete(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error) {
		id, err := idParam(r)
		if err != nil {
			return nil, err
		}
		return nil, s.AdminDeleteIndustry(r.Context(), op, id, qInt64(r.URL.Query(), "reassignTo"))
	})(w, r)
}

func (h *handlers) industryTemplates(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, _ core.Operator) (any, error) {
		id, err := idParam(r)
		if err != nil {
			return nil, err
		}
		return s.AdminIndustryTemplates(r.Context(), id)
	})(w, r)
}

func (h *handlers) industryOrder(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error) {
		id, err := idParam(r)
		if err != nil {
			return nil, err
		}
		var body struct {
			OrderedTemplateIDs []int64 `json:"orderedTemplateIds"`
		}
		if err := decode(r, &body); err != nil {
			return nil, err
		}
		return nil, s.AdminSetIndustryOrder(r.Context(), op, id, body.OrderedTemplateIDs)
	})(w, r)
}

// ---------------------------------------------------------------- 模板

func (h *handlers) templateList(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, _ core.Operator) (any, error) {
		q := r.URL.Query()
		return s.AdminListTemplates(r.Context(), core.TemplateFilter{
			Keyword: q.Get("keyword"), IndustryID: qInt64(q, "industryId"), IncludeChildren: qBool(q, "includeChildren"),
			MediaType: q.Get("mediaType"), State: q.Get("state"), CreatedBy: qInt64(q, "createdBy"),
			OnlineFrom: q.Get("onlineFrom"), OnlineTo: q.Get("onlineTo"), OfflineFrom: q.Get("offlineFrom"), OfflineTo: q.Get("offlineTo"),
			Unassigned: qBool(q, "unassigned"), Sort: q.Get("sort"), Order: q.Get("order"), Page: qInt(q, "page"), Size: qInt(q, "size"),
		})
	})(w, r)
}

func (h *handlers) templateTrash(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, _ core.Operator) (any, error) {
		q := r.URL.Query()
		return s.AdminTrash(r.Context(), qInt(q, "page"), qInt(q, "size"))
	})(w, r)
}

func (h *handlers) templateGet(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, _ core.Operator) (any, error) {
		id, err := idParam(r)
		if err != nil {
			return nil, err
		}
		return s.AdminGetTemplate(r.Context(), id)
	})(w, r)
}

func (h *handlers) templateCreate(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error) {
		var in core.TemplateInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		id, err := s.AdminCreateTemplate(r.Context(), op, in)
		return map[string]any{"id": id}, err
	})(w, r)
}

func (h *handlers) templateUpdate(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error) {
		id, err := idParam(r)
		if err != nil {
			return nil, err
		}
		var in core.TemplateInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return s.AdminUpdateTemplate(r.Context(), op, id, in) // {id, updatedAt, revision}
	})(w, r)
}

// templateSlots PUT /templates/{id}/slots：只改入参 schema。body {slots:{schemaVersion,items}, updatedAt(必填), revision?}；
// 返回 {id, slots, updatedAt, revision}。冲突 4002 · CONFLICT；校验失败 1003 · SLOTS_INVALID + data.field。
func (h *handlers) templateSlots(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error) {
		id, err := idParam(r)
		if err != nil {
			return nil, err
		}
		var in core.TemplateSlotsInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return s.AdminUpdateTemplateSlots(r.Context(), op, id, in)
	})(w, r)
}

// templateIndustries PUT /templates/{id}/industries：给单个模板增删行业（A 类写，交接期放行）。
// body {assign?:[id], unassign?:[id]} 增量；或 {industryIds:[id], revision}（整体替换，revision 必填）。
func (h *handlers) templateIndustries(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error) {
		id, err := idParam(r)
		if err != nil {
			return nil, err
		}
		var in core.TemplateIndustriesInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		if op, err = withPublishPerm(r, op); err != nil {
			return nil, err
		}
		return s.AdminSetTemplateIndustries(r.Context(), op, id, in)
	})(w, r)
}

// templateAttributes PUT /templates/{id}/attrs：渠道 / 媒体类型 / 预览视频 / 封面 / 需要产品图 / 文字叠加（A 类写，交接期放行；revision 必填）。
func (h *handlers) templateAttributes(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error) {
		id, err := idParam(r)
		if err != nil {
			return nil, err
		}
		var in core.TemplateAttributesInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return s.AdminSetTemplateAttributes(r.Context(), op, id, in)
	})(w, r)
}

// capabilities GET /capabilities：{adminWriteEnabled, writable{status,recycle,fullEdit,slots,industryRelation,industryOrder,attrs,samples}}。
func (h *handlers) capabilities(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, _ core.Operator) (any, error) {
		return s.AdminCapabilities(), nil
	})(w, r)
}

// unassignedSummary GET /templates/unassigned-summary：未分配行业的模板数（后台首页角标）+ 当前写权属模式。
func (h *handlers) unassignedSummary(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, _ core.Operator) (any, error) {
		return s.AdminUnassignedSummary(r.Context())
	})(w, r)
}

func (h *handlers) templateDuplicate(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error) {
		id, err := idParam(r)
		if err != nil {
			return nil, err
		}
		nid, err := s.AdminDuplicateTemplate(r.Context(), op, id)
		return map[string]any{"id": nid}, err
	})(w, r)
}

func (h *handlers) templatePublish(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error) {
		id, err := idParam(r)
		if err != nil {
			return nil, err
		}
		var in core.PublishInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return s.AdminPublishTemplate(r.Context(), op, id, in)
	})(w, r)
}

func idAction(f func(s *core.Service, r *http.Request, op core.Operator, id int64) error) fnT {
	return func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error) {
		id, err := idParam(r)
		if err != nil {
			return nil, err
		}
		return nil, f(s, r, op, id)
	}
}

func (h *handlers) templateOffline(w http.ResponseWriter, r *http.Request) {
	h.wrap(idAction(func(s *core.Service, r *http.Request, op core.Operator, id int64) error {
		return s.AdminOfflineTemplate(r.Context(), op, id)
	}))(w, r)
}

func (h *handlers) templateDelete(w http.ResponseWriter, r *http.Request) {
	h.wrap(idAction(func(s *core.Service, r *http.Request, op core.Operator, id int64) error {
		return s.AdminDeleteTemplate(r.Context(), op, id)
	}))(w, r)
}

func (h *handlers) templateRestore(w http.ResponseWriter, r *http.Request) {
	h.wrap(idAction(func(s *core.Service, r *http.Request, op core.Operator, id int64) error {
		return s.AdminRestoreTemplate(r.Context(), op, id)
	}))(w, r)
}

func (h *handlers) templatePurge(w http.ResponseWriter, r *http.Request) {
	h.wrap(idAction(func(s *core.Service, r *http.Request, op core.Operator, id int64) error {
		return s.AdminPurgeTemplate(r.Context(), op, id)
	}))(w, r)
}

func (h *handlers) templateBatch(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error) {
		var in core.BatchInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		in.Action = pathvar.Vars(r)["action"] // 动作取自路径（权限按路径判，body 里的 action 无效）
		if middleware.IndustryRelationAction(in.Action) {
			var err error
			if op, err = withPublishPerm(r, op); err != nil {
				return nil, err
			}
		}
		return s.AdminBatch(r.Context(), op, in)
	})(w, r)
}

func (h *handlers) templateAudit(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, _ core.Operator) (any, error) {
		id, err := idParam(r)
		if err != nil {
			return nil, err
		}
		q := r.URL.Query()
		return s.AdminAudit(r.Context(), core.AuditFilter{ResourceType: core.ResTemplate, ResourceID: strconv.FormatInt(id, 10), Page: qInt(q, "page"), Size: qInt(q, "size")})
	})(w, r)
}

func (h *handlers) audit(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, _ core.Operator) (any, error) {
		q := r.URL.Query()
		return s.AdminAudit(r.Context(), core.AuditFilter{ResourceType: q.Get("resourceType"), ResourceID: q.Get("resourceId"), Action: q.Get("action"),
			OperatorID: qInt64(q, "operatorId"), OperatorName: q.Get("operatorName"), BatchID: q.Get("batchId"), From: q.Get("from"), To: q.Get("to"), Page: qInt(q, "page"), Size: qInt(q, "size")})
	})(w, r)
}

func (h *handlers) preview(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, _ core.Operator) (any, error) {
		q := r.URL.Query()
		p, err := core.ParseListParams(q)
		if err != nil {
			return nil, err
		}
		return s.AdminPreview(r.Context(), p, q.Get("asOf"), qBool(q, "includeDraft"))
	})(w, r)
}

// ---------------------------------------------------------------- 渠道 / 示例素材

func (h *handlers) channelList(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, _ core.Operator) (any, error) {
		return s.AdminListChannels(r.Context())
	})(w, r)
}

func (h *handlers) channelCreate(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error) {
		var in core.ChannelInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		id, err := s.AdminCreateChannel(r.Context(), op, in)
		return map[string]any{"id": id}, err
	})(w, r)
}

func (h *handlers) channelUpdate(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error) {
		id, err := idParam(r)
		if err != nil {
			return nil, err
		}
		var in core.ChannelInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return nil, s.AdminUpdateChannel(r.Context(), op, id, in)
	})(w, r)
}

func (h *handlers) sampleList(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, _ core.Operator) (any, error) {
		q := r.URL.Query()
		return s.AdminListSamples(r.Context(), q.Get("scopeType"), qInt64(q, "industryId"), qInt64(q, "templateId"))
	})(w, r)
}

func (h *handlers) sampleCreate(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error) {
		var in core.SampleInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		id, err := s.AdminCreateSample(r.Context(), op, in)
		return map[string]any{"id": id}, err
	})(w, r)
}

func (h *handlers) sampleUpdate(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error) {
		id, err := idParam(r)
		if err != nil {
			return nil, err
		}
		var in core.SampleInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return nil, s.AdminUpdateSample(r.Context(), op, id, in)
	})(w, r)
}

func (h *handlers) sampleDelete(w http.ResponseWriter, r *http.Request) {
	h.wrap(idAction(func(s *core.Service, r *http.Request, op core.Operator, id int64) error {
		return s.AdminDeleteSample(r.Context(), op, id)
	}))(w, r)
}

// ---------------------------------------------------------------- 行业建议 / 搜索缺口

func (h *handlers) suggestionList(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, _ core.Operator) (any, error) {
		q := r.URL.Query()
		return s.AdminSuggestions(r.Context(), core.SuggestionFilter{Status: q.Get("status"), From: q.Get("from"), To: q.Get("to"),
			Sort: q.Get("sort"), Page: qInt(q, "page"), Size: qInt(q, "size")})
	})(w, r)
}

func (h *handlers) suggestionAdopt(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error) {
		var body struct {
			IndustryID int64 `json:"industryId"`
		}
		if err := decode(r, &body); err != nil {
			return nil, err
		}
		return nil, s.AdminHandleSuggestion(r.Context(), op, pathvar.Vars(r)["normText"], "ADOPTED", body.IndustryID)
	})(w, r)
}

func (h *handlers) suggestionIgnore(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error) {
		return nil, s.AdminHandleSuggestion(r.Context(), op, pathvar.Vars(r)["normText"], "IGNORED", 0)
	})(w, r)
}

func (h *handlers) missList(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, _ core.Operator) (any, error) {
		q := r.URL.Query()
		return s.AdminSearchMisses(r.Context(), core.MissFilter{Industry: q.Get("industry"), From: q.Get("from"), To: q.Get("to"),
			Status: q.Get("status"), Sort: q.Get("sort"), Page: qInt(q, "page"), Size: qInt(q, "size")})
	})(w, r)
}

func (h *handlers) missStatus(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error) {
		var body struct {
			Industry string `json:"industry"`
			Term     string `json:"term"`
			Status   string `json:"status"`
		}
		if err := decode(r, &body); err != nil {
			return nil, err
		}
		return nil, s.AdminHandleSearchMiss(r.Context(), op, body.Industry, body.Term, body.Status)
	})(w, r)
}

// ---------------------------------------------------------------- 模板编辑器：素材上传 / 技能下拉

func (h *handlers) uploadPresign(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, _ core.Operator) (any, error) {
		var in core.PresignInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return s.AdminUploadPresign(r.Context(), in)
	})(w, r)
}

func (h *handlers) uploadConfirm(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, _ core.Operator) (any, error) {
		var in core.ConfirmInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return s.AdminUploadConfirm(r.Context(), in)
	})(w, r)
}

func (h *handlers) skillList(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, _ core.Operator) (any, error) {
		items, err := s.AdminListSkills(r.Context(), r.URL.Query().Get("keyword"))
		return map[string]any{"items": items}, err
	})(w, r)
}

// ---------------------------------------------------------------- 营销节点

func (h *handlers) nodeList(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, _ core.Operator) (any, error) {
		q := r.URL.Query()
		return s.AdminListNodes(r.Context(), q.Get("keyword"), q.Get("enabled"), qInt(q, "page"), qInt(q, "size"))
	})(w, r)
}

func (h *handlers) nodeGet(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, _ core.Operator) (any, error) {
		id, err := idParam(r)
		if err != nil {
			return nil, err
		}
		return s.AdminGetNode(r.Context(), id)
	})(w, r)
}

func (h *handlers) nodeCreate(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error) {
		var in core.NodeInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		id, err := s.AdminCreateNode(r.Context(), op, in)
		if err != nil {
			return nil, err
		}
		return map[string]any{"id": id}, nil
	})(w, r)
}

func (h *handlers) nodeUpdate(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error) {
		id, err := idParam(r)
		if err != nil {
			return nil, err
		}
		var in core.NodeInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return nil, s.AdminUpdateNode(r.Context(), op, id, in)
	})(w, r)
}

func (h *handlers) nodeEnabled(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error) {
		id, err := idParam(r)
		if err != nil {
			return nil, err
		}
		var body struct {
			Enabled *bool `json:"enabled"`
		}
		if err := decode(r, &body); err != nil {
			return nil, err
		}
		if body.Enabled == nil {
			return nil, xerr.Param("enabled 必填")
		}
		return nil, s.AdminSetNodeEnabled(r.Context(), op, id, *body.Enabled)
	})(w, r)
}

func (h *handlers) nodeDelete(w http.ResponseWriter, r *http.Request) {
	h.wrap(idAction(func(s *core.Service, r *http.Request, op core.Operator, id int64) error {
		return s.AdminDeleteNode(r.Context(), op, id)
	}))(w, r)
}

func (h *handlers) nodeTemplates(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, _ core.Operator) (any, error) {
		id, err := idParam(r)
		if err != nil {
			return nil, err
		}
		return s.AdminNodeTemplates(r.Context(), id, qInt64(r.URL.Query(), "industryId"))
	})(w, r)
}

type nodeTemplatesBody struct {
	IndustryID  int64   `json:"industryId"`
	TemplateIDs []int64 `json:"templateIds"`
}

func (h *handlers) nodeTemplateAdd(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error) {
		id, err := idParam(r)
		if err != nil {
			return nil, err
		}
		var b nodeTemplatesBody
		if err := decode(r, &b); err != nil {
			return nil, err
		}
		added, err := s.AdminAddNodeTemplates(r.Context(), op, id, b.IndustryID, b.TemplateIDs)
		if err != nil {
			return nil, err
		}
		return map[string]any{"added": added}, nil
	})(w, r)
}

func (h *handlers) nodeTemplateOrder(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error) {
		id, err := idParam(r)
		if err != nil {
			return nil, err
		}
		var b nodeTemplatesBody
		if err := decode(r, &b); err != nil {
			return nil, err
		}
		return nil, s.AdminSortNodeTemplates(r.Context(), op, id, b.IndustryID, b.TemplateIDs)
	})(w, r)
}

func (h *handlers) nodeTemplateRemove(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error) {
		id, err := idParam(r)
		if err != nil {
			return nil, err
		}
		tid, err := strconv.ParseInt(pathvar.Vars(r)["templateId"], 10, 64)
		if err != nil || tid <= 0 {
			return nil, xerr.Param("模板 id 不合法")
		}
		return nil, s.AdminRemoveNodeTemplate(r.Context(), op, id, tid)
	})(w, r)
}

// templateReprocessMedia POST /templates/{id}/reprocess-media：重新处理视频（抽封面 / 转码）。把 FAILED（或想重做的 READY）视频模板重新排队为 PROCESSING，
// 后台任务接手；正在处理中的拒绝（1003）。返回 {id, updatedAt, revision}；审计 TEMPLATE_MEDIA_REPROCESS。
func (h *handlers) templateReprocessMedia(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error) {
		id, err := idParam(r)
		if err != nil {
			return nil, err
		}
		return s.AdminReprocessTemplateMedia(r.Context(), op, id)
	})(w, r)
}

// channelDelete DELETE /channels/{id}：逻辑删除渠道。被模板引用（含回收站）时拒绝（4002 · RESOURCE_CONFLICT，文案带引用数与示例模板）；
// 未引用才可删，删除后 code 不可复用。渠道列表每行带 templateCount / trashCount 供前端提前置灰。
func (h *handlers) channelDelete(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error) {
		id, err := idParam(r)
		if err != nil {
			return nil, err
		}
		return map[string]any{"id": id}, s.AdminDeleteChannel(r.Context(), op, id)
	})(w, r)
}

// industrySwitchGet 行业功能总开关（数据库 > 配置文件）。
func (h *handlers) industrySwitchGet(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, _ core.Operator) (any, error) {
		return s.IndustrySwitchView(r.Context())
	})(w, r)
}

// industrySwitchSet PUT {enabled, reason}：写库，之后以库为准。
func (h *handlers) industrySwitchSet(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error) {
		var body struct {
			Enabled *bool  `json:"enabled"`
			Reason  string `json:"reason"`
		}
		if err := decode(r, &body); err != nil {
			return nil, err
		}
		if body.Enabled == nil {
			return nil, xerr.New(xerr.ParamMissing, "缺少 enabled")
		}
		return s.SetIndustrySwitch(r.Context(), op, *body.Enabled, body.Reason)
	})(w, r)
}

// industrySwitchReset DELETE {reason}：恢复跟随配置文件。
func (h *handlers) industrySwitchReset(w http.ResponseWriter, r *http.Request) {
	h.wrap(func(w http.ResponseWriter, r *http.Request, s *core.Service, op core.Operator) (any, error) {
		var body struct {
			Reason string `json:"reason"`
		}
		if err := decode(r, &body); err != nil {
			return nil, err
		}
		return s.ResetIndustrySwitch(r.Context(), op, body.Reason)
	})(w, r)
}
