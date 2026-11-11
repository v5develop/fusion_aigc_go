package logic

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/xerr"

	"github.com/DATA-DOG/go-sqlmock"
)

var latestRunCols = []string{"id", "canvas_id", "node_id", "user_id", "kind", "backend", "status", "candidates", "video_url", "created_at"}

// expectLatestRuns LatestSucceededByNode 只按本画布 cv_1 查。
func expectLatestRuns(mock sqlmock.Sqlmock, rows *sqlmock.Rows) {
	mock.ExpectQuery(regexp.QuoteMeta("canvas_id = 'cv_1'")).WillReturnRows(rows)
}

func videoReq(refs ...*canvas.RunReference) *canvas.RunNodeRequest {
	return &canvas.RunNodeRequest{Kind: "video", Model: "Seedance 2.5", Prompt: "走秀", Ratio: "16:9", Size: "720p", Duration: "6s", References: refs}
}

const staleSigned = "https://bucket.oss-cn-hangzhou.aliyuncs.com/v1/work/2026/11/old.mp4?Expires=1&OSSAccessKeyId=a&Signature=b"

// 视频口按 node_id 取上游最近一次成功 run 的长期引用现签，不用请求里（已过期）的签名地址。
func TestVideoRefByNodeResolvesDurableRef(t *testing.T) {
	c, mock := mockCore(t)
	media, lm := newLibMedia(t)
	c.s.Media = media
	expectLatestRuns(mock, sqlmock.NewRows(latestRunCols).
		AddRow(11, "cv_1", "v", 7, "video", "gateway", 2, `[{"rid":"`+srcRID0+`","url":"mr://`+srcRID0+`.jpg"}]`, "mr://"+srcVideo+".mp4", time.Now()))
	sp, err := c.buildSpec(videoReq(&canvas.RunReference{Handle: "video", NodeId: "v", Video: staleSigned}), c.refResolver(context.Background(), "cv_1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sp.Videos) != 1 || sp.Videos[0] != "https://signed/"+srcVideo+"?Expires=600" {
		t.Fatalf("应按上游 run 的 mr:// 现签: %v", sp.Videos)
	}
	if lm.signs != 1 {
		t.Fatalf("只签上游视频一次: %d", lm.signs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	// 上游视频已转存素材库（公开长期地址）：原样用，不签
	l := &refLookup{c: c, ctx: context.Background(), out: &frozenRefs{}, loaded: true, runs: map[string]*model.NodeRun{
		"v": {NodeID: "v", VideoURL: sql.NullString{String: "https://cdn/lib-v.mp4", Valid: true}},
	}}
	sp, err = c.buildSpec(videoReq(&canvas.RunReference{Handle: "video", NodeId: "v", Video: staleSigned}), l)
	if err != nil || sp.Videos[0] != "https://cdn/lib-v.mp4" {
		t.Fatalf("素材库地址原样: %v %v", sp, err)
	}
}

// 请求里是本画布产物的旧签名地址（没带 candidate_index，或候选被拖成了独立图片节点）：换回长期引用现签；
// 视频口指向的上游没有成功 run 时同样按签名地址映射。
func TestStaleSignedURLMapsBackToOwnOutput(t *testing.T) {
	c, mock := mockCore(t)
	media, _ := newLibMedia(t)
	c.s.Media = media
	expectLatestRuns(mock, sqlmock.NewRows(latestRunCols).
		AddRow(11, "cv_1", "m", 7, "image", "gateway", 2, refCands(srcRID0, srcRID1), nil, time.Now()).
		AddRow(12, "cv_1", "v", 7, "video", "gateway", 2, nil, "mr://"+srcVideo+".mp4", time.Now()))
	req := &canvas.RunNodeRequest{Kind: "image", Model: "Banana", ModelId: "banana-v1", Prompt: "白底", Ratio: "1:1", Size: "2K",
		References: []*canvas.RunReference{
			{Handle: "image", Index: 1, NodeId: "img1", Image: "https://signed/" + srcRID1 + "?Expires=1&Signature=old"},
		}}
	l := c.refResolver(context.Background(), "cv_1")
	sp, err := c.buildSpec(req, l)
	if err != nil {
		t.Fatal(err)
	}
	if sp.Images[0].URL != "https://signed/"+srcRID1+"?Expires=600" {
		t.Fatalf("旧签名应换成现签: %+v", sp.Images)
	}
	// 冻结的是长期引用 + 上游 run + 下标，不是签名地址
	if fr := l.frozenOut().Refs; fr["references.0"] != (frozenRef{Ref: mrRef(srcRID1, "png"), RunID: 11, Index: 1}) {
		t.Fatalf("冻结值不对: %+v", fr)
	}
	// 视频口：节点 u（用户视频节点、没有 run）里存的是 v 产物的旧签名地址
	sp, err = c.buildSpec(videoReq(&canvas.RunReference{Handle: "video", NodeId: "u", Video: "https://signed/" + srcVideo + "?Expires=1&Signature=old"}), l)
	if err != nil {
		t.Fatal(err)
	}
	if sp.Videos[0] != "https://signed/"+srcVideo+"?Expires=600" {
		t.Fatalf("视频旧签名应换成现签: %v", sp.Videos)
	}
	// 工具参数里的输入图同样换新
	p := map[string]any{"image_url": "https://signed/" + srcRID0 + "?Expires=1&Signature=old"}
	if err := resolveParamURLs(p, l); err != nil {
		t.Fatal(err)
	}
	if p["image_url"] != "https://signed/"+srcRID0+"?Expires=600" {
		t.Fatalf("params.image_url 应换新: %v", p)
	}
	fr := l.frozenOut().Refs
	if fr["references.0"] != (frozenRef{Ref: "mr://" + srcVideo + ".mp4", RunID: 12, Index: -1}) ||
		fr["params.image_url"] != (frozenRef{Ref: mrRef(srcRID0, "png"), RunID: 11, Index: 0}) {
		t.Fatalf("冻结值不对: %+v", fr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 别的画布的节点：查询只按本画布，查不到它的 run；它产物的签名地址也不会被重新签出来；执行前连线校验直接拒绝。
func TestForeignCanvasNodeNotResolved(t *testing.T) {
	c, mock := mockCore(t)
	media, lm := newLibMedia(t)
	c.s.Media = media
	expectLatestRuns(mock, sqlmock.NewRows(latestRunCols).
		AddRow(11, "cv_1", "m", 7, "image", "gateway", 2, refCands(srcRID0), nil, time.Now()))
	l := c.refResolver(context.Background(), "cv_1")
	foreign := "https://signed/inst01-work-zzzzzzzzzzzzzzzzzzzzzzzzzz?Expires=1&Signature=x"
	if _, ok := l.latestVideo("x"); ok {
		t.Fatal("别的画布的节点不应解析出产物")
	}
	if _, ok := l.latestCandidate("x", 0); ok {
		t.Fatal("别的画布的节点不应解析出产物")
	}
	if got, err := l.image("references.0", "x", nil, foreign); err != nil || got != foreign {
		t.Fatalf("别人产物的签名地址不能映射重签: %q %v", got, err)
	}
	if len(l.frozenOut().Refs) != 0 {
		t.Fatalf("没解析出东西就不冻结: %+v", l.frozenOut())
	}
	if lm.signs != 1 { // 只比对过本画布自己的那 1 个产物
		t.Fatalf("signs=%d", lm.signs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	doc, err := ParseSnapshot([]byte(`{"nodes":[{"id":"m","type":"model","data":{"model":"Seedance 2.5"}},{"id":"d","type":"model","data":{}}],"edges":[{"id":"e","source":"m","target":"d","sourceHandle":"video","targetHandle":"video"}]}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	err = ValidateRun(doc, RunCheck{NodeID: "d", Kind: "video", ImageLimit: 3, VideoLimit: 1,
		Refs: []RefInput{{Handle: "video", NodeID: "x", URL: foreign}}})
	if err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("快照外（别的画布）的节点应拒绝: %v", err)
	}
}

// 外部 / 用户上传地址原样：不签、不改。
func TestExternalVideoURLUnchanged(t *testing.T) {
	c, mock := mockCore(t)
	media, lm := newLibMedia(t)
	c.s.Media = media
	expectLatestRuns(mock, sqlmock.NewRows(latestRunCols).
		AddRow(11, "cv_1", "m", 7, "image", "gateway", 2, refCands(srcRID0), nil, time.Now()))
	l := c.refResolver(context.Background(), "cv_1")
	ext := "https://cdn.example.com/u/a.mp4"
	sp, err := c.buildSpec(videoReq(&canvas.RunReference{Handle: "video", NodeId: "u", Video: ext}), l)
	if err != nil {
		t.Fatal(err)
	}
	if sp.Videos[0] != ext {
		t.Fatalf("外部地址应原样: %v", sp.Videos)
	}
	// 上游是图片产物（没有视频）：视频口仍用请求地址
	sp, err = c.buildSpec(videoReq(&canvas.RunReference{Handle: "video", NodeId: "m", Video: ext}), l)
	if err != nil || sp.Videos[0] != ext {
		t.Fatalf("上游没有视频产物时用请求地址: %v %v", sp, err)
	}
	if lm.signs != 0 {
		t.Fatalf("不带签名参数的地址不应触发现签: %d", lm.signs)
	}
	// 带签名参数但不是本画布产物（如第三方签名地址）：比对不上，原样
	thirdParty := "https://other.example.com/x.mp4?X-Amz-Signature=abc&X-Amz-Expires=60"
	sp, err = c.buildSpec(videoReq(&canvas.RunReference{Handle: "video", NodeId: "u", Video: thirdParty}), l)
	if err != nil || sp.Videos[0] != thirdParty {
		t.Fatalf("第三方签名地址原样: %v %v", sp, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	// 没有 lookup（nil）照旧只用请求地址
	if sp, err = c.buildSpec(videoReq(&canvas.RunReference{Handle: "video", NodeId: "u", Video: ext}), nil); err != nil || sp.Videos[0] != ext {
		t.Fatalf("nil lookup: %v %v", sp, err)
	}
}

func TestLooksSigned(t *testing.T) {
	for u, want := range map[string]bool{
		staleSigned: true,
		"https://b.oss-cn-shanghai.aliyuncs.com/k.png?x-oss-signature=1&x-oss-date=2": true,
		"https://s3/k.png?X-Amz-Signature=1":                                          true,
		"https://cdn/lib.png":                                                         false,
		"https://cdn/lib.png?x-oss-process=image/resize":                              false,
		"mr://inst01-work-aaaaaaaaaaaaaaaaaaaaaaaaaa.png":                             false,
	} {
		if looksSigned(u) != want {
			t.Fatalf("%s → %v", u, !want)
		}
	}
	if objectPath("https://A.com/v1/work/y.png?Expires=1") != objectPath("http://cdn.b.com/v1/work/y.png?Expires=2") {
		t.Fatal("路径比对应忽略协议、主机（CDN / 自定义域名）与查询串")
	}
	if objectPath("https://a.com/x?Expires=1") != "" {
		t.Fatal("过短的路径不参与比对")
	}
}

// contentMedia 媒体服务替身（贴近真实语义）：每次现签签名串都不同；ingest 的 rid 由源地址派生（真实服务按幂等键 =
// 源地址派生 rid，重签的地址拉入会得到新 rid），sha256 由内容决定（这里用地址里的源 rid 代表内容；素材库拷贝内容相同）。
// signStatus 非 0 时现签返回该状态码。
type contentMedia struct {
	mu         sync.Mutex
	signs      int
	signStatus int
}

func newContentMedia(t *testing.T) (*mediasvc.Client, *contentMedia) {
	m := &contentMedia{}
	ridIn := regexp.MustCompile(`inst01-work-[a-z]{26}`)
	srv, _ := fakeServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		m.mu.Lock()
		defer m.mu.Unlock()
		switch {
		case r.URL.Path == "/v1/mediasvc/resources/urls":
			if m.signStatus != 0 {
				w.WriteHeader(m.signStatus)
				_, _ = w.Write([]byte(`{"error":{"code":"x","message":"sign failed"}}`))
				return
			}
			m.signs++
			var req struct{ Rid string }
			_ = json.Unmarshal(body, &req)
			_ = json.NewEncoder(w).Encode(map[string]any{"url": fmt.Sprintf("https://oss.example.com/v1/work/%s.png?Expires=600&OSSAccessKeyId=k&Signature=s%d", req.Rid, m.signs)})
		case strings.HasPrefix(r.URL.Path, "/v1/mediasvc/ingest"):
			var req struct {
				Source struct{ URL string } `json:"source"`
			}
			_ = json.Unmarshal(body, &req)
			byURL := sha256.Sum256([]byte(req.Source.URL))
			content := sha256.Sum256([]byte(ridIn.FindString(req.Source.URL)))
			rid := "inst01-user-" + strings.Repeat(string(rune('a'+byURL[0]%26)), 26)
			_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]any{
				"rid": rid, "class": "user", "store": "private", "sha256": hex.EncodeToString(content[:]), "mime": "image/png"}})
		default:
			w.WriteHeader(404)
		}
	})
	return mediasvc.New([]string{srv.URL}, "tok"), m
}

// hashView 网关 request_hash 的视角：MediaRef 带 sha256 时只取 sha256（ai-gateway contract/media.py），其余字段原样。
func hashView(t *testing.T, reqs []aigateway.CallRequest) string {
	t.Helper()
	var out []any
	for _, r := range reqs {
		b, _ := json.Marshal(map[string]any{"op": r.Operation, "group": r.ModelGroup, "input": r.Input, "limits": r.Limits})
		var v any
		_ = json.Unmarshal(b, &v)
		var walk func(x any) any
		walk = func(x any) any {
			switch y := x.(type) {
			case map[string]any:
				if sha, ok := y["sha256"].(string); ok && sha != "" {
					if _, isRef := y["uri"]; isRef {
						return "sha:" + sha
					}
				}
				for k, e := range y {
					y[k] = walk(e)
				}
			case []any:
				for i, e := range y {
					y[i] = walk(e)
				}
			}
			return x
		}
		out = append(out, walk(v))
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func uris(reqs []aigateway.CallRequest) string {
	b, _ := json.Marshal(reqs[0].Input)
	return string(b)
}

const runByID11 = `WHERE \(r.id = 11\) LIMIT 1`
const runByID12 = `WHERE \(r.id = 12\) LIMIT 1`

// 受理时冻结上游引用：排队期间上游重跑成功，重新派发仍用受理时的那批产物（不再查「最近一次成功」），
// 网关输入在 request_hash 视角下逐字节一致（同一幂等键不会 409）；上游转存素材库后用素材库拷贝，哈希仍一致。
func TestFrozenRefsSurviveUpstreamRerun(t *testing.T) {
	c, mock := mockCore(t)
	media, cm := newContentMedia(t)
	c.s.Media = media
	ctx := context.Background()
	expectLatestRuns(mock, sqlmock.NewRows(latestRunCols).
		AddRow(11, "cv_1", "m", 7, "image", "gateway", 2, refCands(srcRID0, srcRID1), nil, time.Now()).
		AddRow(12, "cv_1", "v", 7, "video", "gateway", 2, nil, "mr://"+srcVideo+".mp4", time.Now()))
	req := videoReq(
		&canvas.RunReference{Handle: "image", Index: 1, NodeId: "m", Image: "https://stale/first.png", CandidateIndex: idx(1)},
		&canvas.RunReference{Handle: "video", NodeId: "v", Video: staleSigned},
	)
	// 受理
	l := c.refResolver(ctx, "cv_1")
	sp1, err := c.buildSpec(req, l)
	if err != nil {
		t.Fatal(err)
	}
	st := toStored(req)
	st.Frozen = l.frozenOut()
	reqJSON, _ := json.Marshal(st)
	if !strings.Contains(string(reqJSON), `"frozen":{"refs":{"references.0":{"ref":"mr://`+srcRID1+`.png","run_id":11,"index":1}`) ||
		strings.Contains(string(reqJSON), "Signature=s") {
		t.Fatalf("落库请求应冻结长期引用、不含签名地址: %s", reqJSON)
	}
	run := &model.NodeRun{ID: 20, CanvasID: "cv_1", NodeID: "d", UserID: 7, Kind: model.KindVideo, Backend: model.BackendGateway,
		Status: model.RunQueued, Request: reqJSON, ModelGroup: sp1.ModelGroup, TaskID: sql.NullString{String: "cv_t", Valid: true}}
	reqs1, err := c.gatewayRequests(ctx, run, sp1)
	if err != nil {
		t.Fatal(err)
	}

	// 排队期间上游 m / v 都重跑成功了（最近一次成功已是新 run）——重新派发不查最近一次成功（sqlmock 对未声明的查询报错），
	// 只按冻结的 run 主键读：两个 run 仍是成功态、产物未变
	mock.ExpectQuery(runByID11).WillReturnRows(sqlmock.NewRows(latestRunCols).
		AddRow(11, "cv_1", "m", 7, "image", "gateway", 2, refCands(srcRID0, srcRID1), nil, time.Now()))
	mock.ExpectQuery(runByID12).WillReturnRows(sqlmock.NewRows(latestRunCols).
		AddRow(12, "cv_1", "v", 7, "video", "gateway", 2, nil, "mr://"+srcVideo+".mp4", time.Now()))
	sp2, err := c.specFromRun(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sp2.Images[0].URL, srcRID1) || len(sp2.Videos) != 1 || !strings.Contains(sp2.Videos[0], srcVideo) {
		t.Fatalf("重新派发应沿用受理时的产物: first=%s videos=%v", sp2.Images[0].URL, sp2.Videos)
	}
	if sp2.Images[0].URL == sp1.Images[0].URL {
		t.Fatal("每次派发应重新现签（替身每次签名串不同）")
	}
	reqs2, err := c.gatewayRequests(ctx, run, sp2)
	if err != nil {
		t.Fatal(err)
	}
	if uris(reqs1) == uris(reqs2) {
		t.Fatal("替身按源地址派生 rid：重签后 uri 应不同（证明比对的是哈希视角而非巧合）")
	}
	if hashView(t, reqs1) != hashView(t, reqs2) {
		t.Fatalf("同一幂等键的网关输入（哈希视角）必须一致:\n%s\n%s", hashView(t, reqs1), hashView(t, reqs2))
	}

	// 上游 m 转存素材库完成（候选换成公开地址，内容同一份拷贝）：用素材库地址，哈希仍一致
	lib := `[{"rid":"x","url":"https://cdn.example.com/lib/` + srcRID0 + `.png"},{"rid":"y","url":"https://cdn.example.com/lib/` + srcRID1 + `.png"}]`
	mock.ExpectQuery(runByID11).WillReturnRows(sqlmock.NewRows(latestRunCols).
		AddRow(11, "cv_1", "m", 7, "image", "gateway", 2, lib, nil, time.Now()))
	mock.ExpectQuery(runByID12).WillReturnRows(sqlmock.NewRows(latestRunCols).
		AddRow(12, "cv_1", "v", 7, "video", "gateway", 2, nil, "mr://"+srcVideo+".mp4", time.Now()))
	sp3, err := c.specFromRun(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	if sp3.Images[0].URL != "https://cdn.example.com/lib/"+srcRID1+".png" {
		t.Fatalf("转存后应用同一 run 同一下标的素材库地址: %s", sp3.Images[0].URL)
	}
	reqs3, err := c.gatewayRequests(ctx, run, sp3)
	if err != nil {
		t.Fatal(err)
	}
	if hashView(t, reqs1) != hashView(t, reqs3) {
		t.Fatal("转存后重新派发的网关输入（哈希视角）也必须一致")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	_ = cm
}

// 冻结的上游产物签不出：媒体服务 4xx（对象不存在）→ 参数错误（run 失败、退款）；5xx → 暂时性错误（留在排队重试）。
// 冻结的上游 run 已不在 / 不属本画布 → 用冻结的引用本身，绝不改用别的产物。
func TestFrozenRefMissingObject(t *testing.T) {
	c, mock := mockCore(t)
	media, cm := newContentMedia(t)
	c.s.Media = media
	ctx := context.Background()
	st := toStored(videoReq(&canvas.RunReference{Handle: "video", NodeId: "v", Video: staleSigned}))
	st.Frozen = &frozenRefs{Refs: map[string]frozenRef{"references.0": {Ref: "mr://" + srcVideo + ".mp4", RunID: 12, Index: -1}}}
	reqJSON, _ := json.Marshal(st)
	run := &model.NodeRun{ID: 20, CanvasID: "cv_1", NodeID: "d", UserID: 7, Kind: model.KindVideo, Backend: model.BackendGateway, Request: reqJSON}

	// 上游 run 被换到了别的画布（不该发生，兜底）：不信它的当前值，用冻结引用
	mock.ExpectQuery(runByID12).WillReturnRows(sqlmock.NewRows(latestRunCols).
		AddRow(12, "cv_other", "v", 8, "video", "gateway", 2, nil, "mr://inst01-work-zzzzzzzzzzzzzzzzzzzzzzzzzz.mp4", time.Now()))
	sp, err := c.specFromRun(ctx, run)
	if err != nil || !strings.Contains(sp.Videos[0], srcVideo) {
		t.Fatalf("应用冻结引用: %v %v", sp, err)
	}

	cm.signStatus = 404
	mock.ExpectQuery(runByID12).WillReturnRows(sqlmock.NewRows(latestRunCols))
	_, err = c.specFromRun(ctx, run)
	if code, msg, ok := xerr.Parse(err); !ok || code != xerr.ParamError || !strings.Contains(msg, "上游产物已不可用") {
		t.Fatalf("对象不存在应是参数错误: %v", err)
	}
	cm.signStatus = 503
	mock.ExpectQuery(runByID12).WillReturnRows(sqlmock.NewRows(latestRunCols))
	sctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond) // 客户端对 5xx 有退避重试，测试里不等满
	defer cancel()
	if _, err = c.specFromRun(sctx, run); !isTemporary(err) {
		t.Fatalf("媒体服务 5xx 应是暂时性错误: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 本改动之前受理的 run（落库请求没有 frozen）：重新派发保持原行为——视频用请求地址，图片按 candidate_index 取最近一次成功。
func TestLegacyRunWithoutFrozen(t *testing.T) {
	c, mock := mockCore(t)
	media, _ := newContentMedia(t)
	c.s.Media = media
	reqJSON, _ := json.Marshal(toStored(videoReq(
		&canvas.RunReference{Handle: "image", Index: 1, NodeId: "m", Image: "https://stale/first.png", CandidateIndex: idx(0)},
		&canvas.RunReference{Handle: "video", NodeId: "v", Video: "https://cdn.example.com/u/a.mp4"},
	)))
	run := &model.NodeRun{ID: 20, CanvasID: "cv_1", NodeID: "d", UserID: 7, Kind: model.KindVideo, Backend: model.BackendGateway, Request: reqJSON}
	expectLatestRuns(mock, sqlmock.NewRows(latestRunCols).
		AddRow(11, "cv_1", "m", 7, "image", "gateway", 2, refCands(srcRID0), nil, time.Now()).
		AddRow(12, "cv_1", "v", 7, "video", "gateway", 2, nil, "mr://"+srcVideo+".mp4", time.Now()))
	sp, err := c.specFromRun(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sp.Images[0].URL, srcRID0) || sp.Videos[0] != "https://cdn.example.com/u/a.mp4" {
		t.Fatalf("旧 run 应保持原行为: %s %v", sp.Images[0].URL, sp.Videos)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 幂等比对忽略服务端冻结的 frozen：同一请求重放（哪怕上游已重跑、解析结果不同）不判 4002。
func TestSameRequestIgnoresFrozen(t *testing.T) {
	st := toStored(videoReq(&canvas.RunReference{Handle: "video", NodeId: "v", Video: staleSigned}))
	plain, _ := json.Marshal(st)
	st.Frozen = &frozenRefs{Refs: map[string]frozenRef{"references.0": {Ref: "mr://x", RunID: 1, Index: -1}}}
	withFrozen, _ := json.Marshal(st)
	if !sameRequest(withFrozen, plain) {
		t.Fatal("frozen 不参与幂等比对")
	}
	st.Prompt = "别的"
	changed, _ := json.Marshal(st)
	if sameRequest(withFrozen, changed) {
		t.Fatal("客户端字段不同仍应判不同")
	}
}
