package media

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
)

// 运维定位（设计 §11 的补充）：只给持有运维令牌的运维查询 rid 对应的类别、存储位置、桶名、对象键与路径。
// 业务接口的响应不含这些信息：Java 只保存 rid，桶与对象键只用于排障、对账与运维操作（如手工恢复）。
// 运维令牌为空时该接口不注册（返回 404），不做无鉴权的开放查询。

type locateReq struct {
	RID string `json:"rid"`
}

type locateResp struct {
	RID       string `json:"rid"`
	Class     string `json:"class"`
	Store     string `json:"store"`
	Bucket    string `json:"bucket,omitempty"`
	ObjectKey string `json:"object_key"`
	Path      string `json:"path"`
}

// opsAuth 用运维令牌鉴权（与业务令牌分开，常量时间比较）。
func (s *Server) opsAuth(next http.HandlerFunc) http.HandlerFunc {
	want := sha256.Sum256([]byte(s.opsToken))
	return func(w http.ResponseWriter, r *http.Request) {
		got := sha256.Sum256([]byte(bearerOrToken(r)))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			writeAPIError(w, newAPIError(codeUnauthorized, "auth.ops_token"))
			return
		}
		next(w, r)
	}
}

func (s *Server) locate(w http.ResponseWriter, r *http.Request) {
	var req locateReq
	if !readJSON(w, r, &req) {
		return
	}
	_, class, err := ParseRID(req.RID)
	if err != nil {
		writeAPIError(w, invalid("request.rid"))
		return
	}
	store := storeForClass(class, "")
	objectKey := s.id.ObjectKey(req.RID)
	resp := locateResp{RID: req.RID, Class: class, Store: store, ObjectKey: objectKey, Path: storePath(class, objectKey)}
	if oss, ok := s.objects.(*OSSStore); ok {
		if b, err := oss.bucket(store); err == nil {
			resp.Bucket = b.Bucket
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
