package media

import (
	"errors"
	"net/http"
	"time"
)

// 资源 URL 签发（设计文档 §3）。URL 不入库，调用方每次需要时现签。
const (
	urlModeSigned = "signed"
	urlModePublic = "public"

	defaultURLTTLSeconds = 3600
	maxURLTTLSeconds     = 24 * 3600
)

// resourceURLReq 请求：rid 指定资源；class 决定桶内前缀；store 决定桶；mode 决定地址形式。
type resourceURLReq struct {
	RID        string `json:"rid"`
	Class      string `json:"class"`
	Store      string `json:"store"`
	Mode       string `json:"mode"`
	TTLSeconds int64  `json:"ttl_seconds"`
	// Process 可选的 OSS 图片处理样式（named style）。只允许 mode=signed；为空表示不加工。
	Process string `json:"process"`
}

// resourceURLResp 响应：只返回地址与有效期，不返回字节。
type resourceURLResp struct {
	RID       string `json:"rid"`
	Store     string `json:"store"`
	Mode      string `json:"mode"`
	Size      int64  `json:"size"`
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// checkClassStore 类别与存储必须配对：素材库只能进 asset_library，用户输入与中间产物只能进私有桶，
// 交付产物可以进私有桶或第三方公有桶（按服务商配置选择）。
func checkClassStore(class, store string) error {
	switch class {
	case classUser, classWork:
		if store != storePrivate {
			return invalid("request.class_store")
		}
	case classAsset:
		if store != storeAsset {
			return invalid("request.class_store")
		}
	case classDeliver:
		if store != storePrivate && store != storeThirdParty {
			return invalid("request.class_store")
		}
	default:
		return invalid("request.class")
	}
	return nil
}

// validProcessStyle 处理串只允许 [A-Za-z0-9_./,-]，且不能为空。
// OSS 命名样式（style/wm-view）与免费档参数水印（image/watermark,text_<base64url>,g_se,...；文字与 logo 路径都是
// URL 安全 Base64，不含 + / =）都只用这些字符：逗号是 RFC 3986 的子分隔符，在查询串里原样合法，
// 因此签名用的值与地址里的值逐字相同，也避免把任意字符带进签名或查询串。
func validProcessStyle(style string) bool {
	if style == "" {
		return false
	}
	for i := 0; i < len(style); i++ {
		c := style[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '_', c == '.', c == '/', c == '-', c == ',':
		default:
			return false
		}
	}
	return true
}

// resourceURL 签发资源的访问地址。
func (s *Server) resourceURL(w http.ResponseWriter, r *http.Request) {
	var req resourceURLReq
	if !readJSON(w, r, &req) {
		return
	}
	_, ridClass, err := ParseRID(req.RID)
	if err != nil {
		writeAPIError(w, invalid("request.rid"))
		return
	}
	// 类别以 rid 为准：请求里如果带了类别，必须一致
	if req.Class != "" && req.Class != ridClass {
		writeAPIError(w, invalid("request.class_mismatch"))
		return
	}
	req.Class = ridClass
	if !validStore(req.Store) {
		writeAPIError(w, invalid("request.store"))
		return
	}
	if err := checkClassStore(req.Class, req.Store); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.Process != "" {
		if req.Mode != urlModeSigned {
			writeAPIError(w, invalid("request.process_mode"))
			return
		}
		if !validProcessStyle(req.Process) {
			writeAPIError(w, invalid("request.process_style"))
			return
		}
	}
	if req.Mode != urlModeSigned && req.Mode != urlModePublic {
		writeAPIError(w, invalid("request.mode"))
		return
	}
	ttl := req.TTLSeconds
	if ttl == 0 {
		ttl = defaultURLTTLSeconds
	}
	if ttl <= 0 || ttl > maxURLTTLSeconds {
		writeAPIError(w, invalid("request.ttl"))
		return
	}
	ctx := r.Context()
	_, path, err := s.objectPath(ctx, req.RID)
	if err != nil {
		writeAPIError(w, ridNotFound(err))
		return
	}

	if err := s.checkReadable(ctx, req.Store, path); err != nil {
		writeAPIError(w, err)
		return
	}
	size, err := s.objects.Head(ctx, req.Store, path)
	if errors.Is(err, errObjectNotFound) {
		writeAPIError(w, newAPIError(codeNotFound, "resource.object_missing"))
		return
	}
	if err != nil {
		writeAPIError(w, err)
		return
	}

	var u StoredURL
	if req.Mode == urlModePublic {
		u, err = s.objects.PublicURL(ctx, req.Store, path)
	} else if req.Process != "" {
		u, err = s.objects.SignedProcessURL(ctx, req.Store, path, req.Process, time.Duration(ttl)*time.Second)
	} else {
		u, err = s.objects.SignedURL(ctx, req.Store, path, time.Duration(ttl)*time.Second)
	}
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if u.Kind == "none" {
		writeAPIError(w, newAPIError(codeNoPublicLocation, "resource.no_readable_location"))
		return
	}
	writeJSON(w, http.StatusOK, resourceURLResp{
		RID: req.RID, Store: req.Store, Mode: req.Mode, Size: size,
		URL: u.Value, ExpiresAt: u.ExpiresAt,
	})
}
