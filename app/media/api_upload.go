package media

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

// 直传（设计文档 §11.4）：Java 不选对象路径。先申请上传目标（rid + 签名 PUT 地址），客户端直传，再凭 rid 注册。

const uploadTTL = 15 * time.Minute

type uploadTargetReq struct {
	Class   string `json:"class"`
	Store   string `json:"store"`
	IdemKey string `json:"idem_key"`
	Mime    string `json:"mime"`
	Size    int64  `json:"size"`
}

type uploadTargetResp struct {
	RID       string `json:"rid"`
	UploadURL string `json:"upload_url"`
	ExpiresAt string `json:"expires_at"`
}

type registerReq struct {
	RID string `json:"rid"`
}

// uploadTarget 申请直传目标。只允许 user 类别进私有桶。
func (s *Server) uploadTarget(w http.ResponseWriter, r *http.Request) {
	var req uploadTargetReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.Class != classUser || req.Store != storePrivate {
		writeAPIError(w, invalid("request.class_store"))
		return
	}
	if req.IdemKey == "" || req.Size <= 0 {
		writeAPIError(w, invalid("request.upload_target"))
		return
	}
	rid, err := s.id.RID(s.owners.ownerFor(r.Context(), req.IdemKey), classUser, req.IdemKey)
	if err != nil {
		writeAPIError(w, invalid("request.idem_key"))
		return
	}
	ctx := r.Context()
	ext := extForMime(req.Mime)
	path, err := s.uploadPath(ctx, rid, ext, time.Now())
	if errors.Is(err, errUploadTypeChanged) {
		writeAPIError(w, newAPIError(codeConflict, "upload.mime_changed"))
		return
	}
	if err != nil {
		writeAPIError(w, err)
		return
	}
	// 目标已经存在（用户已直传过）：不再签发上传地址，避免覆盖已登记的用户对象
	switch _, err := s.objects.Head(ctx, storePrivate, path); {
	case err == nil:
		writeAPIError(w, newAPIError(codeConflict, "upload.object_exists"))
		return
	case !errors.Is(err, errObjectNotFound):
		writeAPIError(w, err)
		return
	}
	u, err := s.objects.SignedPutURL(ctx, storePrivate, path, uploadTTL)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if u.Kind == "none" {
		writeAPIError(w, newAPIError(codeNoPublicLocation, "upload.no_upload_location"))
		return
	}
	writeJSON(w, http.StatusOK, uploadTargetResp{RID: rid, UploadURL: u.Value, ExpiresAt: u.ExpiresAt})
}

// errUploadTypeChanged 同一幂等键的直传目标改了声明类型：路径已固定，拒绝（409）。
var errUploadTypeChanged = errors.New("media: upload declared type changed")

// uploadPath 直传目标的路径（用户对象）：v1/user/<yyyy>/<mm>/<对象键>[.<后缀>]，后缀由客户端声明的类型决定。
// 声明类型只是承诺：注册时会校验实际内容的类型，不一致则拒绝（422）。路径与索引在签发地址前写入，重试沿用同一路径；
// 同一幂等键改了声明类型返回 errUploadTypeChanged。
func (s *Server) uploadPath(ctx context.Context, rid, ext string, now time.Time) (string, error) {
	p, found, err := s.readIndex(ctx, rid, classUser)
	if err != nil {
		return "", err
	}
	if found {
		if pathExt(p) != ext {
			return "", errUploadTypeChanged
		}
		return p, nil
	}
	p = datedPath(classUser, now, s.id.ObjectKey(rid), ext)
	if err := s.writeIndex(ctx, rid, p); err != nil {
		return "", unavailable("对象索引写入失败，请稍后重试")
	}
	s.paths.put(rid, p)
	return p, nil
}

// pathExt 路径的后缀（不含点）；没有后缀返回空串。
func pathExt(path string) string {
	base := path[strings.LastIndex(path, "/")+1:]
	if i := strings.IndexByte(base, '.'); i >= 0 {
		return base[i+1:]
	}
	return ""
}

// register 确认直传完成：取回对象，完整解码校验（与字节流上传同一标准），返回对象引用与媒体属性。
// 不合法的字节返回 422 source_rejected，对象本身保留，由调用方决定是否删除（这里不自动删用户对象）。
// 对象不存在返回 404 not_found。url 为空（kind none）：公有地址由 resources/urls 或 copy 另行获取。
func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var req registerReq
	if !readJSON(w, r, &req) {
		return
	}
	_, class, err := ParseRID(req.RID)
	if err != nil || class != classUser {
		writeAPIError(w, invalid("request.rid"))
		return
	}
	jobCtx, dir, done, err := s.svc.begin(r.Context())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	defer done()

	local, err := s.sourceFn(SourceRef{Rid: req.RID})(jobCtx, dir)
	if errors.Is(err, errObjectNotFound) {
		writeAPIError(w, newAPIError(codeNotFound, "register.object_missing"))
		return
	}
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if err := s.svc.Validate(jobCtx, local); err != nil {
		writeAPIError(w, err)
		return
	}
	meta, err := s.probeMeta(jobCtx, local)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	// 声明类型与实际内容不一致：路径（含后缀）已固定，拒绝登记；对象保留，由调用方决定是否删除
	if declared := pathExt(s.uploadedPathOf(jobCtx, req.RID)); declared != "" && extForMime(meta.Mime) != declared {
		writeAPIError(w, newAPIError(codeSourceRejected, "upload.type_mismatch"))
		return
	}
	sum, _, err := hashFile(local)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, MediaResult{
		Object: ObjectRef{
			RID: req.RID, Class: classUser, Store: storePrivate,
			Sha256: sum, Size: meta.Size, Mime: meta.Mime,
			DurationS: meta.DurationS, Width: meta.Width, Height: meta.Height, Fps: meta.Fps, HasAudio: meta.HasAudio,
		},
		Url: URLRef{Kind: "none"},
	})
}

// uploadedPathOf 直传对象的索引路径（没有索引返回空串）。
func (s *Server) uploadedPathOf(ctx context.Context, rid string) string {
	p, found, err := s.readIndex(ctx, rid, classUser)
	if err != nil || !found {
		return ""
	}
	return p
}
