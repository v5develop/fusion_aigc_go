package logic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/internal/config"
	"aigc-local-life/pkg/mediasvc"
)

// thumbMedia 媒体服务替身：resources/urls 记下每次的 rid / process，地址里带 x-oss-process（模拟处理串在签名内）。
type thumbMedia struct {
	mu       sync.Mutex
	reqs     []map[string]any
	failProc bool   // 带 process 的签名一律 500
	failRID  string // 只有这个 rid 的签名（原图与缩略图）永久 500
}

func (m *thumbMedia) count(withProc bool) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, req := range m.reqs {
		if p, _ := req["process"].(string); (p != "") == withProc {
			n++
		}
	}
	return n
}

func newThumbMedia(t *testing.T) (*mediasvc.Client, *thumbMedia) {
	t.Helper()
	m := &thumbMedia{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/mediasvc/resources/urls" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		m.mu.Lock()
		m.reqs = append(m.reqs, req)
		fail := m.failProc
		failRID := m.failRID
		m.mu.Unlock()
		rid, _ := req["rid"].(string)
		if rid == failRID {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"gone"}}`))
			return
		}
		u := "https://xxlm-pp.oss-cn-shanghai.aliyuncs.com/" + rid + ".png?Expires=1&OSSAccessKeyId=AK&Signature=s"
		if p, _ := req["process"].(string); p != "" {
			if fail {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"code":"x","message":"boom"}}`))
				return
			}
			u += "&x-oss-process=" + p
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"url": u})
	}))
	t.Cleanup(srv.Close)
	return mediasvc.New([]string{srv.URL}, "tok"), m
}

const thumbRID2 = "tst001-work-bbbbbbbbbbbbbbbbbbbbbbbbbb"

func runWith(cands []model.Candidate) *model.NodeRun {
	raw, _ := json.Marshal(cands)
	return &model.NodeRun{Candidates: raw, Status: model.RunSucceeded, Kind: model.KindImage}
}

// 私有引用的图片候选：原图照旧现签（不带处理串），另出一个带缩放处理串的缩略图；公开地址 / GIF 不出。
func TestCandidateThumbsSignedWithProcess(t *testing.T) {
	c := testCore()
	media, m := newThumbMedia(t)
	c.s.Media = media
	r := runWith([]model.Candidate{
		{URL: "mr://" + testRID + ".png", W: 2048, H: 2048},
		{URL: "https://cdn.example.com/lib/x.png"},
		{URL: "mr://" + thumbRID2 + ".gif"},
	})
	res := c.toResult(context.Background(), r)
	if len(res.Candidates) != 3 {
		t.Fatalf("candidates = %+v", res.Candidates)
	}
	orig, err := url.Parse(res.Candidates[0].Url)
	if err != nil || orig.Query().Get("x-oss-process") != "" || orig.Query().Get("Signature") == "" {
		t.Fatalf("原图应是不带处理串的签名地址: %s", res.Candidates[0].Url)
	}
	th, err := url.Parse(res.Candidates[0].ThumbUrl)
	if err != nil || th.Query().Get("x-oss-process") != config.DefaultThumbProcess {
		t.Fatalf("缩略图应带默认处理串: %q", res.Candidates[0].ThumbUrl)
	}
	if res.Candidates[1].ThumbUrl != "" || res.Candidates[2].ThumbUrl != "" {
		t.Fatalf("公开地址 / GIF 不出缩略图: %+v", res.Candidates)
	}
	// 处理串由媒体服务纳入签名：请求里带 process，有效期与原图一致
	var withProc int
	for _, req := range m.reqs {
		if p, _ := req["process"].(string); p != "" {
			withProc++
			if p != config.DefaultThumbProcess || req["rid"] != testRID || req["ttl_seconds"] != float64(refURLTTL.Seconds()) {
				t.Fatalf("缩略图签名请求 = %v", req)
			}
		}
	}
	if withProc != 1 {
		t.Fatalf("带处理串的签名次数 = %d, reqs=%v", withProc, m.reqs)
	}
	// 原 run 不被改写
	if r.Thumbs != nil || !strings.HasPrefix(r.CandidateList()[0].URL, "mr://") {
		t.Fatalf("原 run 不应被改写")
	}
}

// 画布回填：candidateThumbs 与 candidates 同序；保存时作为运行态剥掉。
func TestBackfillCandidateThumbsStripped(t *testing.T) {
	c := testCore()
	media, _ := newThumbMedia(t)
	c.s.Media = media
	r := runWith([]model.Candidate{{URL: "mr://" + testRID + ".png"}, {URL: "https://cdn.example.com/a.png"}})
	r.NodeID = "n1"
	d := &Doc{Nodes: []*Node{{ID: "n1", Type: "model", Data: map[string]any{}}}}
	d.Backfill(nil, c.resolvedRuns(context.Background(), []*model.NodeRun{r}), c.progressOf)
	thumbs, ok := d.Nodes[0].Data["candidateThumbs"].([]string)
	if !ok || len(thumbs) != 2 || !strings.Contains(thumbs[0], "x-oss-process=image/resize") || thumbs[1] != "" {
		t.Fatalf("candidateThumbs = %#v", d.Nodes[0].Data["candidateThumbs"])
	}
	d.StripRuntime(map[string]bool{})
	if _, ok := d.Nodes[0].Data["candidateThumbs"]; ok {
		t.Fatalf("candidateThumbs 是运行态，保存时须剥掉")
	}
}

// 缩略图只是加速：签失败 / 配置 off 时不出，原图不受影响。
func TestCandidateThumbsFailOpen(t *testing.T) {
	c := testCore()
	media, m := newThumbMedia(t)
	c.s.Media = media
	m.failProc = true
	r := runWith([]model.Candidate{{URL: "mr://" + testRID + ".png"}, {URL: "mr://" + thumbRID2 + ".jpg"}})
	res := c.toResult(context.Background(), r)
	for _, x := range res.Candidates {
		if x.Url == "" || x.ThumbUrl != "" {
			t.Fatalf("签失败应只丢缩略图: %+v", res.Candidates)
		}
	}
	m.failProc = false
	c.s.Config.Thumb.Process = "off"
	res = c.toResult(context.Background(), r)
	if res.Candidates[0].ThumbUrl != "" {
		t.Fatalf("off 不出缩略图: %+v", res.Candidates)
	}
}

// 水印视图：缩放接在水印处理链后面（缩略图仍带水印）；水印串不是 image/ 链或已含缩放时不出。
func TestWatermarkThumbProcess(t *testing.T) {
	thumb := config.DefaultThumbProcess
	if got := watermarkThumbProcess("image/watermark,text_YWJj,g_se", thumb); got != "image/watermark,text_YWJj,g_se/resize,m_lfit,w_480/format,webp/quality,q_80" {
		t.Fatalf("got %q", got)
	}
	for _, wm := range []string{"style/wm-view", "image/watermark,text_x/resize,w_100", "image/format,jpg", ""} {
		if got := watermarkThumbProcess(wm, thumb); got != "" {
			t.Fatalf("%q → %q, want 空", wm, got)
		}
	}
	c := testCore()
	media, _ := newThumbMedia(t)
	c.s.Media = media
	c.s.Config.FreeWatermark.Process = "image/watermark,text_YWJj,g_se"
	res := c.toResult(context.Background(), runWith([]model.Candidate{{URL: "mrw://" + testRID + ".png"}}))
	th, _ := url.Parse(res.Candidates[0].ThumbUrl)
	if got := th.Query().Get("x-oss-process"); !strings.HasPrefix(got, "image/watermark,") || !strings.HasSuffix(got, "/format,webp/quality,q_80") {
		t.Fatalf("水印缩略图处理串 = %q", got)
	}
}

func TestThumbConfProcess(t *testing.T) {
	cases := map[string]string{
		"": config.DefaultThumbProcess, "off": "", "OFF": "",
		"image/resize,w_320": "image/resize,w_320", "style/thumb": "", "image/resize,w_320&x=1": "",
	}
	for in, want := range cases {
		if got := (config.ThumbConf{Process: in}).ThumbProcess(); got != want {
			t.Fatalf("%q → %q, want %q", in, got, want)
		}
	}
}

// 一张永久签不出：每个 rid 的缩略图只签一次（不整批 + 逐个重签），其余照常出缩略图。
func TestCandidateThumbsPermanentFailureBounded(t *testing.T) {
	c := testCore()
	media, m := newThumbMedia(t)
	c.s.Media = media
	const rid3 = "tst001-work-cccccccccccccccccccccccccc"
	m.failRID = thumbRID2
	r := runWith([]model.Candidate{{URL: "mr://" + testRID + ".png"}, {URL: "mr://" + thumbRID2 + ".png"}, {URL: "mr://" + rid3 + ".png"}})
	res := c.toResult(context.Background(), r)
	if got := m.count(true); got != 3 {
		t.Fatalf("缩略图签名次数 = %d, want 3（每个 rid 一次）", got)
	}
	if got := m.count(false); got != 3 {
		t.Fatalf("原图签名次数 = %d, want 3", got)
	}
	if res.Candidates[0].ThumbUrl == "" || res.Candidates[2].ThumbUrl == "" {
		t.Fatalf("其余候选应照常出缩略图: %+v", res.Candidates)
	}
	if res.Candidates[1].Url != "" || res.Candidates[1].ThumbUrl != "" {
		t.Fatalf("签不出的那张原图与缩略图都应为空: %+v", res.Candidates[1])
	}
}

// 智能体读取出口（agentExec 的 ctx）不签缩略图。
func TestCandidateThumbsSkippedForAgent(t *testing.T) {
	c := testCore()
	media, m := newThumbMedia(t)
	c.s.Media = media
	r := runWith([]model.Candidate{{URL: "mr://" + testRID + ".png"}, {URL: "mr://" + thumbRID2 + ".png"}})
	res := c.toResult(withoutThumbs(context.Background()), r)
	if m.count(true) != 0 {
		t.Fatalf("智能体出口不应签缩略图: %v", m.reqs)
	}
	for _, x := range res.Candidates {
		if x.Url == "" || x.ThumbUrl != "" {
			t.Fatalf("原图照常、缩略图为空: %+v", res.Candidates)
		}
	}
	if got := runView(res)["candidates"].([]map[string]any); len(got) != 2 {
		t.Fatalf("runView candidates = %v", got)
	}
}
