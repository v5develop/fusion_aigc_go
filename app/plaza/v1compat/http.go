package v1compat

import (
	"net/http"
	"time"

	"aigc-local-life/pkg/v1wire"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"github.com/zeromicro/go-zero/rest"
)

// 三条 v1 路径（精确匹配；nginx 用 location = 逐条切，DESIGN 9.3）。
const (
	PathList      = "/api/v1/plaza-templates/list"
	PathTags      = "/api/v1/plaza-templates/tags"
	PathHeadlines = "/api/v1/plaza-headlines"
)

// Module 三个接口共用的数据源与复刻统计（Java 里列表与头条共用同一个 PlazaTemplateUsageStatsService 单例）。
type Module struct {
	src   Source
	usage *usageStats
	now   func() time.Time
}

// New now 为 nil 时用 time.Now。
func New(src Source, now func() time.Time) *Module {
	if now == nil {
		now = time.Now
	}
	return &Module{src: src, usage: newUsageStats(src, now), now: now}
}

// MustBuild 用网关的 DataSource 建模块（go-zero 按 DSN 复用连接池，不额外开池）。dataSource 为空返回 nil。
func MustBuild(dataSource string) *Module {
	if dataSource == "" {
		logx.Info("plaza v1compat: 未配置 DataSource，v1 兼容三接口不注册")
		return nil
	}
	db, err := sqlx.NewMysql(dataSource).RawDB()
	if err != nil {
		logx.Must(err)
	}
	return New(SQLSource{DB: db}, nil)
}

// Register 注册三条 v1 路径（公开，不挂 Auth：与 Java 的 permitAll 一致，带不带 Authorization 都一样）。
// m 为 nil 时不注册（路径继续由 nginx 指向 Java，网关上 404）。
func Register(server *rest.Server, m *Module) {
	if m == nil {
		return
	}
	server.AddRoutes([]rest.Route{
		{Method: http.MethodGet, Path: PathList, Handler: m.HandleList},
		{Method: http.MethodGet, Path: PathTags, Handler: m.HandleTags},
		{Method: http.MethodGet, Path: PathHeadlines, Handler: m.HandleHeadlines},
	})
}

func (m *Module) HandleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// Spring 按参数声明顺序解析：page → size → tags，第一个失败就回 400
	page, err := v1wire.SpringLong(q["page"], 1)
	if err != nil {
		v1wire.WriteJavaFail(w, r, errTypeMismatch("page"))
		return
	}
	size, err := v1wire.SpringLong(q["size"], 10)
	if err != nil {
		v1wire.WriteJavaFail(w, r, errTypeMismatch("size"))
		return
	}
	tags, _ := v1wire.SpringStringList(q["tags"])
	reply(w, r)(m.List(r.Context(), page, size, tags))
}

func (m *Module) HandleTags(w http.ResponseWriter, r *http.Request) {
	reply(w, r)(m.Tags(r.Context()))
}

func (m *Module) HandleHeadlines(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tag, hasTag := v1wire.SpringString(q["tag"])
	kw, hasKw := v1wire.SpringString(q["keyword"])
	reply(w, r)(m.Headlines(r.Context(), tag, kw, hasTag, hasKw))
}

func reply(w http.ResponseWriter, r *http.Request) func([]byte, *v1wire.JavaFailure) {
	return func(data []byte, f *v1wire.JavaFailure) {
		if f != nil {
			v1wire.WriteJavaFail(w, r, f)
			return
		}
		v1wire.WriteJavaOK(w, r, data)
	}
}
