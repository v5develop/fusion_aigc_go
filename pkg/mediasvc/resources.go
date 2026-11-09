package mediasvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 本文件：资源（rid）相关的调用——按 URL / 字节拉入、探测、现签地址、复制到素材库、下载字节。
// 契约以 app/media 的代码为准（docs/api/media-service.md 与代码的出入见 image.go 头部注释）。

// Resource 一个已落桶的对象（服务端 MediaResult 的调用方视图）。业务侧只保存 RID。
type Resource struct {
	RID    string `json:"rid"`
	Class  string `json:"class"` // user / work / deliver / asset
	Store  string `json:"store"` // private / asset_library / third_party
	SHA256 string `json:"sha256"`
	Mime   string `json:"mime"`
	Bytes  int64  `json:"bytes"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	// URL 服务端随结果给出的地址（kind=none 时为空）。私有桶对象请用 ResourceURLs 现签，不要持久化。
	URL string `json:"url,omitempty"`
	// Parents 溯源（如 CopyToLibrary 的源 rid）。
	Parents []string `json:"parents,omitempty"`
	// Label 仅 Crop 填：对应 PixelBox.Label。
	Label string `json:"label,omitempty"`
}

// ProbeInfo 探测结果（不落桶）。
type ProbeInfo struct {
	Mime      string  `json:"mime"`
	Bytes     int64   `json:"size"`
	DurationS float64 `json:"duration_s"`
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	Fps       float64 `json:"fps"`
	HasAudio  bool    `json:"has_audio"`
}

// 服务端 purpose 取值。
const (
	PurposeUserInput    = "user_input"    // 用户输入，落 user 类别私有桶
	PurposeIntermediate = "intermediate"  // 中间产物，落 work 类别私有桶
	PurposeAssetLibrary = "asset_library" // 素材库桶（公有可读）
	PurposeThirdParty   = "third_party"   // 第三方交付桶
)

// mediaResult 服务端 MediaResult。
type mediaResult struct {
	Object struct {
		RID    string `json:"rid"`
		Class  string `json:"class"`
		Store  string `json:"store"`
		Sha256 string `json:"sha256"`
		Size   int64  `json:"size"`
		Mime   string `json:"mime"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
	} `json:"object"`
	URL struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
	} `json:"url"`
	Parents []string `json:"parents"`
}

func (m mediaResult) resource() Resource {
	r := Resource{
		RID: m.Object.RID, Class: m.Object.Class, Store: m.Object.Store, SHA256: m.Object.Sha256,
		Mime: m.Object.Mime, Bytes: m.Object.Size, Width: m.Object.Width, Height: m.Object.Height,
		Parents: m.Parents,
	}
	if m.URL.Kind != "" && m.URL.Kind != "none" {
		r.URL = m.URL.Value
	}
	return r
}

func parseResource(raw []byte) (Resource, error) {
	var m mediaResult
	if err := json.Unmarshal(raw, &m); err != nil {
		return Resource{}, fmt.Errorf("媒体服务返回无法解析: %w", err)
	}
	if m.Object.RID == "" {
		return Resource{}, fmt.Errorf("媒体服务返回缺少 rid")
	}
	return m.resource(), nil
}

// autoIdem 幂等键的默认值：sha256(方法 + 入参的 JSON)。
//
// 重放语义：入参完全相同的调用得到同一个键，服务端按键派生 rid，所以重试、重复提交得到同一个 rid
// 与同一个对象（覆盖写，内容一致），不会产生重复产物；入参（含源 rid、尺寸、purpose）任一变化则键变化，得到新 rid。
// 代价：调用方若想「对同样输入强制重新生成」，必须自己传 idem。
// 同键异体服务端返回 400（request.idempotency_key_reused），自动键由入参决定，不会触发。
func autoIdem(method string, args ...any) string {
	b, _ := json.Marshal(args)
	sum := sha256.Sum256(append([]byte(method+"|"), b...))
	return hex.EncodeToString(sum[:])
}

func pick(idem, method string, args ...any) string {
	if strings.TrimSpace(idem) != "" {
		return idem
	}
	return autoIdem(method, args...)
}

// IngestURL 按 URL 拉入并落桶（POST /v1/mediasvc/ingest）。路由 key 是 URL（同一 URL 命中同一实例的输入缓存）。
func (c *Client) IngestURL(ctx context.Context, srcURL, purpose, idem string) (Resource, error) {
	idem = pick(idem, "ingest", srcURL, purpose)
	body, _ := json.Marshal(map[string]any{"source": map[string]string{"url": srcURL}, "purpose": purpose})
	raw, err := c.post(ctx, srcURL, "/v1/mediasvc/ingest", body, "application/json", idem)
	if err != nil {
		return Resource{}, err
	}
	return parseResource(raw)
}

// IngestBytes 上传字节并落桶（POST /v1/mediasvc/ingest/upload?purpose=，请求体是原始字节）。
// name 只用来推断 Content-Type 提示（服务端按内容判断类型）。
func (c *Client) IngestBytes(ctx context.Context, name string, data []byte, purpose, idem string) (Resource, error) {
	sum := sha256.Sum256(data)
	idem = pick(idem, "ingest/upload", purpose, hex.EncodeToString(sum[:]))
	ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
	if ct == "" {
		ct = "application/octet-stream"
	}
	raw, err := c.post(ctx, idem, "/v1/mediasvc/ingest/upload?purpose="+url.QueryEscape(purpose), data, ct, idem)
	if err != nil {
		return Resource{}, err
	}
	return parseResource(raw)
}

// Probe 探测 rid 的媒体属性（POST /v1/mediasvc/probe，不落桶，只读所以不带幂等键）。路由到 rid 所属实例。
func (c *Client) Probe(ctx context.Context, rid string) (ProbeInfo, error) {
	body, _ := json.Marshal(map[string]any{"source": map[string]string{"rid": rid}})
	raw, err := c.post(ctx, rid, "/v1/mediasvc/probe", body, "application/json", "")
	if err != nil {
		return ProbeInfo{}, err
	}
	var p ProbeInfo
	if err := json.Unmarshal(raw, &p); err != nil {
		return ProbeInfo{}, fmt.Errorf("媒体服务返回无法解析: %w", err)
	}
	return p, nil
}

// CopyToLibrary 把 rid 复制成新 rid（POST /v1/mediasvc/resources/copy）。purpose 只接受 asset_library / third_party
// （user 类别源不能复制到公有桶，403）。返回的 Resource.Parents 为源 rid。
func (c *Client) CopyToLibrary(ctx context.Context, rid, purpose, idem string) (Resource, error) {
	idem = pick(idem, "resources/copy", rid, purpose)
	body, _ := json.Marshal(map[string]any{"rid": rid, "purpose": purpose})
	raw, err := c.post(ctx, rid, "/v1/mediasvc/resources/copy", body, "application/json", idem)
	if err != nil {
		return Resource{}, err
	}
	return parseResource(raw)
}

const (
	defaultURLTTL = time.Hour
	maxURLTTL     = 24 * time.Hour
)

// storeForRID 现签地址需要的 store：asset 在素材库桶，其余在私有桶（deliver 在第三方桶的对象不在此覆盖）。
func storeForRID(rid string) (string, error) {
	m := ridPattern.FindStringSubmatch(rid)
	if m == nil {
		return "", &Error{Status: http.StatusBadRequest, Code: "invalid_request", Message: "rid 格式不合法: " + rid}
	}
	if m[2] == "asset" {
		return "asset_library", nil
	}
	return "private", nil
}

// ResourceURLs 为每个 rid 现签公网读地址（POST /v1/mediasvc/resources/urls，mode=signed）。
// 服务端一次只签一个 rid，这里并发调用（上限 4）后合并；任一失败则整体返回第一个错误。
// ttl<=0 取 1 小时，超过 24 小时截到 24 小时。URL 不要入库，每次需要时现签。
func (c *Client) ResourceURLs(ctx context.Context, rids []string, ttl time.Duration) (map[string]string, error) {
	return c.resourceURLs(ctx, rids, ttl, "")
}

// ResourceURLsWithProcess 同 ResourceURLs，但每个地址都带 OSS 图片处理参数 process（x-oss-process），
// 且该参数由媒体服务纳入签名：客户端去掉或改动它，签名即失效。用于免费档水印视图（mrw://）的读时参数水印。
// process 为空时返回错误（调用方必须明确给出处理串，不能意外签出干净地址）。
func (c *Client) ResourceURLsWithProcess(ctx context.Context, rids []string, ttl time.Duration, process string) (map[string]string, error) {
	if process == "" {
		return nil, &Error{Status: http.StatusBadRequest, Code: "invalid_request", Message: "process 不能为空"}
	}
	return c.resourceURLs(ctx, rids, ttl, process)
}

func (c *Client) resourceURLs(ctx context.Context, rids []string, ttl time.Duration, process string) (map[string]string, error) {
	if ttl <= 0 {
		ttl = defaultURLTTL
	}
	ttl = min(ttl, maxURLTTL)
	out := make(map[string]string, len(rids))
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
		sem      = make(chan struct{}, 4)
	)
	for _, rid := range rids {
		store, err := storeForRID(rid)
		if err != nil {
			return nil, err
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			req := map[string]any{
				"rid": rid, "store": store, "mode": "signed", "ttl_seconds": int64(ttl / time.Second),
			}
			if process != "" {
				req["process"] = process
			}
			body, _ := json.Marshal(req)
			raw, err := c.post(ctx, rid, "/v1/mediasvc/resources/urls", body, "application/json", "")
			var res struct {
				URL string `json:"url"`
			}
			if err == nil {
				if jerr := json.Unmarshal(raw, &res); jerr != nil {
					err = fmt.Errorf("媒体服务返回无法解析: %w", jerr)
				} else if res.URL == "" {
					err = ErrNoReadableURL
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			out[rid] = res.URL
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// maxFetchBytes FetchBytes 的体积上限（用于反推 sidecar 的 data URL，图片足够）。
const maxFetchBytes = 64 << 20

// FetchBytes 下载 rid 的内容。服务端没有直接下载接口，所以先 ResourceURLs 现签再 GET
// （GET 不带媒体服务令牌，地址本身已签名）。返回 mime 取自响应头，缺失时按内容嗅探。
func (c *Client) FetchBytes(ctx context.Context, rid string) ([]byte, string, error) {
	urls, err := c.ResourceURLs(ctx, []string{rid}, 10*time.Minute)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urls[rid], nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, "", &Error{Status: http.StatusServiceUnavailable, Code: "unreachable", Message: "下载资源失败"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", &Error{Status: resp.StatusCode, Code: "fetch_failed", Message: fmt.Sprintf("下载资源返回 %d", resp.StatusCode)}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxFetchBytes {
		return nil, "", &Error{Status: http.StatusRequestEntityTooLarge, Code: "too_large", Message: "资源超过下载上限"}
	}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mt == "" || mt == "application/octet-stream" {
		mt = http.DetectContentType(data)
	}
	return data, mt, nil
}

// Delete 删除资源（POST /v1/mediasvc/delete，软删除 + 保留期后清理）。对象本就不存在也算成功（幂等）。
func (c *Client) Delete(ctx context.Context, rid, idem string) error {
	if !ValidRID(rid) {
		return &Error{Status: http.StatusBadRequest, Code: "invalid_request", Message: "rid 格式不合法: " + rid}
	}
	idem = pick(idem, "delete", rid)
	body, _ := json.Marshal(map[string]any{"rid": rid})
	_, err := c.post(ctx, rid, "/v1/mediasvc/delete", body, "application/json", idem)
	return err
}
