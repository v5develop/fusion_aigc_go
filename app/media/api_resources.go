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
