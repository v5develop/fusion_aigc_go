package media

import (
	"context"
	"errors"
	"net/http"
)

// copyReq 显式复制请求（契约 CopyReq）：源 rid 与目标用途；幂等键走 Idempotency-Key 头。
type copyReq struct {
	Rid     string `json:"rid"`
	Purpose string `json:"purpose"`
}

// copyResource 把源资源复制到目标类别，返回新 rid 的 MediaResult，parents 指向源 rid（设计文档 §3）。
// 复制用已有的 Fetch（取源）与 Put（写目标，经 storeFile）；新 rid 由幂等键派生，同键重放得到同一个 rid。
// 只允许 asset_library（asset 类别）与 third_party（deliver 类别）；不允许复制到 user 类别。
func (s *Server) copyResource(w http.ResponseWriter, r *http.Request) {
	if err := checkKey(r.Header.Get(idempotencyHeader)); err != nil {
		writeAPIError(w, err)
		return
	}
	var req copyReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.Purpose != "asset_library" && req.Purpose != "third_party" {
		writeAPIError(w, invalid("request.purpose"))
		return
	}
	_, srcClass, err := ParseRID(req.Rid)
	if err != nil {
		writeAPIError(w, invalid("request.rid"))
		return
	}
	// 用户输入不能复制到公有桶：复制会让私有素材公开可读（隐私默认值，不因调用方指定而放开）
	if srcClass == classUser {
		writeAPIError(w, newAPIError(codeForbiddenPurpose, "purpose.user_input_not_public"))
		return
	}
	srcPath := storePath(srcClass, s.id.ObjectKey(req.Rid))

	s.withJob(w, r, func(ctx context.Context, dir string) (any, error) {
		srcStore, err := s.resolveSourceStore(ctx, srcClass, srcPath)
		if err != nil {
			return nil, err
		}
		// 待删除的对象不能再复制（与读取一样返回 410）
		if err := s.checkReadable(ctx, srcStore, srcPath); err != nil {
			return nil, err
		}
		local, err := s.objects.Fetch(ctx, srcStore, srcPath, dir)
		if err != nil {
			return nil, err
		}
		// storeFile 以请求上下文里的幂等键派生目标 rid，目标类别与存储由 purpose 决定
		res, err := s.storeFile(ctx, req.Purpose, local)
		if err != nil {
			return nil, err
		}
		res.Parents = []string{req.Rid}
		return res, nil
	})
}

// resolveSourceStore 找到源对象所在的桶。asset 只在素材库桶；user/work 只在私有桶；
// deliver 先查私有桶，找不到再查第三方桶（第三方交付的产物可以再次复制）。两处都没有返回 404。
// 只做存在性探测（Head），不读内容。
func (s *Server) resolveSourceStore(ctx context.Context, class, path string) (string, error) {
	candidates := []string{storeForClass(class, "")}
	if class == classDeliver {
		candidates = []string{storePrivate, storeThirdParty}
	}
	for _, st := range candidates {
		_, err := s.objects.Head(ctx, st, path)
		if err == nil {
			return st, nil
		}
		if !errors.Is(err, errObjectNotFound) {
			return "", err
		}
	}
	return "", newAPIError(codeNotFound, "resource.object_missing")
}
