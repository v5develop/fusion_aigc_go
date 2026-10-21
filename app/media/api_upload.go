package media

import (
	"errors"
	"net/http"
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
	key := s.id.ObjectKey(rid)
	path := storePath(classUser, key)
	// 目标已经存在（用户已直传过）：不再签发上传地址，避免覆盖已登记的用户对象
	switch _, err := s.objects.Head(r.Context(), storePrivate, path); {
	case err == nil:
		writeAPIError(w, newAPIError(codeConflict, "upload.object_exists"))
		return
	case !errors.Is(err, errObjectNotFound):
		writeAPIError(w, err)
		return
	}
	u, err := s.objects.SignedPutURL(r.Context(), storePrivate, path, uploadTTL)
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
