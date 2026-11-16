package replicate

// 示例素材的上传与解析（运营不再手抄裸 rid）：
//   POST /samples/media/upload   multipart 字段 file → 落媒体服务（asset_library 桶）→ 返回 rid + 预览
//   POST /samples/media/resolve  {rid 或 mr://rid} → 校验存在 → 返回同样的预览（粘贴已有素材时用）
// 两个接口都只读业务表、不计费，写示例仍由保存接口（replicate:sample）完成。

import (
	"io"
	"net/http"
	"strings"
	"time"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/rest"
)

// 上传上限：媒体服务入库目前整块读入内存，超过即拒。
const sampleUploadMaxBytes = 200 << 20

// MediaPreview 运营看到的素材信息（不含原始 url 之外的内部字段）。
type MediaPreview struct {
	RID        string  `json:"rid"`
	Kind       string  `json:"kind"` // video / image / other
	Mime       string  `json:"mime"`
	Bytes      int64   `json:"bytes"`
	DurationS  float64 `json:"durationS"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	PreviewURL string  `json:"previewUrl"` // 短时效签名地址，仅用于页面预览，不要存库
}

func registerSampleMedia(server *rest.Server, c *svc.ServiceContext, h *handlers) {
	m := newSampleMedia(c.Config.ReplicateAdmin.Media.BaseURL, c.Config.ReplicateAdmin.Media.Token)
	server.AddRoutes(rest.WithMiddlewares([]rest.Middleware{c.AdminAuth},
		rest.Route{Method: http.MethodPost, Path: prefix + "/samples/media/upload", Handler: h.wrap(needSample(m.upload))},
		rest.Route{Method: http.MethodPost, Path: prefix + "/samples/media/resolve", Handler: h.wrap(m.resolve)},
	))
}

type sampleMedia struct{ c *mediasvc.Client }

func newSampleMedia(baseURL, token string) *sampleMedia {
	if strings.TrimSpace(baseURL) == "" {
		return &sampleMedia{}
	}
	return &sampleMedia{c: mediasvc.New([]string{baseURL}, token)}
}

var errMediaOff = xerr.New(xerr.NotReady, "媒体服务未配置，暂不能上传示例素材")

func (m *sampleMedia) upload(r *http.Request) (any, error) {
	if m.c == nil {
		return nil, errMediaOff
	}
	r.Body = http.MaxBytesReader(nil, r.Body, sampleUploadMaxBytes)
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		return nil, xerr.Param("上传失败：文件超过 200MB 或格式不对")
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		return nil, xerr.Param("请选择要上传的视频或图片")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, sampleUploadMaxBytes+1))
	if err != nil {
		return nil, xerr.Param("读取文件失败，请重试")
	}
	if len(data) > sampleUploadMaxBytes {
		return nil, xerr.Param("文件超过 200MB，请先压缩后再上传")
	}
	res, err := m.c.IngestBytes(r.Context(), hdr.Filename, data, mediasvc.PurposeAssetLibrary, "")
	if err != nil {
		return nil, xerr.Param("上传到媒体服务失败，请稍后重试")
	}
	return m.preview(r, res.RID, res.Mime, res.Bytes, res.Width, res.Height)
}

func (m *sampleMedia) resolve(r *http.Request) (any, error) {
	if m.c == nil {
		return nil, errMediaOff
	}
	var body struct {
		RID string `json:"rid"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	rid := strings.TrimPrefix(strings.TrimSpace(body.RID), "mr://")
	if rid == "" || len(rid) > 128 || strings.ContainsAny(rid, " /?#") {
		return nil, xerr.Param("素材编号格式不对，应为一串字母数字")
	}
	return m.preview(r, rid, "", 0, 0, 0)
}

// preview 探测属性并签一个短时效预览地址。rid 不存在 / 探测失败都按「找不到」处理。
func (m *sampleMedia) preview(r *http.Request, rid, mime string, bytes int64, w, h int) (any, error) {
	ctx := r.Context()
	info, err := m.c.Probe(ctx, rid)
	if err != nil {
		return nil, xerr.Param("找不到这个素材，请确认编号是否正确")
	}
	urls, err := m.c.ResourceURLs(ctx, []string{rid}, 10*time.Minute)
	if err != nil {
		return nil, xerr.New(xerr.ServerError, "素材预览暂时不可用，请稍后再试")
	}
	if mime == "" {
		mime = info.Mime
	}
	if bytes == 0 {
		bytes = info.Bytes
	}
	if w == 0 {
		w, h = info.Width, info.Height
	}
	kind := "other"
	switch {
	case strings.HasPrefix(mime, "video/"):
		kind = "video"
	case strings.HasPrefix(mime, "image/"):
		kind = "image"
	}
	if kind == "other" {
		return nil, xerr.Param("只支持视频或图片素材")
	}
	return &MediaPreview{RID: rid, Kind: kind, Mime: mime, Bytes: bytes, DurationS: info.DurationS,
		Width: w, Height: h, PreviewURL: urls[rid]}, nil
}
