package media

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// copyReq 显式复制请求（契约 CopyReq）：源 rid 与目标用途；幂等键走 Idempotency-Key 头。
type copyReq struct {
	Rid     string `json:"rid"`
	Purpose string `json:"purpose"`
}

// copyResource 把源资源复制到目标类别，返回新 rid 的 MediaResult，parents 指向源 rid（设计文档 §3）。
// 复制用 readObject（取源：本机缓存命中则不下载，否则 Fetch）与 Put（写目标，经 storeFile）；新 rid 由幂等键派生，同键重放得到同一个 rid。
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
	// 用户输入不能复制到公有桶：复制会让私有素材公开可读（与 ingest 共用同一判据）
	if err := checkUserInputNotPublic(SourceRef{Rid: req.Rid}, req.Purpose); err != nil {
		writeAPIError(w, err)
		return
	}
	_, srcPath, err := s.objectPath(r.Context(), req.Rid)
	if err != nil {
		writeAPIError(w, ridNotFound(err))
		return
	}

	s.withJob(w, r, func(ctx context.Context, dir string) (any, error) {
		headStart := time.Now()
		srcStore, srcVer, err := s.resolveSourceStore(ctx, srcClass, srcPath)
		if err != nil {
			logStage(ctx, "copy_src_head", headStart)
			return nil, err
		}
		// 待删除的对象不能再复制（与读取一样返回 410）
		err = s.checkReadable(ctx, srcStore, srcPath)
		logStage(ctx, "copy_src_head", headStart) // 定位源桶 + 待删除检查
		if err != nil {
			return nil, err
		}
		// 源字节在本机对象缓存里（同机刚写入或刚读过）且版本与存储一致时直接用本地文件，不再下载。
		// 版本用上面探测存在性时同一次 HEAD 拿到的，不再 HEAD 第二次
		readStart := time.Now()
		local, facts, err := s.readObjectAt(ctx, req.Rid, srcStore, srcPath, dir, srcVer)
		logStage(ctx, "copy_read", readStart) // 取源字节（本机缓存命中则近 0，否则从存储下载）
		if err != nil {
			return nil, err
		}
		// 以请求上下文里的幂等键派生目标 rid，目标类别与存储由 purpose 决定
		var res MediaResult
		if facts != nil {
			// 命中写穿透条目：这份字节落桶时已 probe 过、hash 过（版本一致即字节一致），直接沿用，不再 probe 与 hash
			res, err = s.storeMeta(withKnownDigest(ctx, local, facts.sum, facts.meta.Size), req.Purpose, local, facts.meta, true)
		} else {
			res, err = s.storeFile(ctx, req.Purpose, local)
		}
		if err != nil {
			return nil, err
		}
		res.Parents = []string{req.Rid}
		return res, nil
	})
}

// resolveSourceStore 找到源对象所在的桶。asset 只在素材库桶；user/work 只在私有桶；
// deliver 先查私有桶，找不到再查第三方桶（第三方交付的产物可以再次复制）。两处都没有返回 404。
// 只做存在性探测（HEAD），不读内容。存储能给出版本（objectVersioner）时用同一次 HEAD 取版本一并返回，
// 供对象缓存核对；否则版本为 nil，由 readObjectAt 自己查（不支持版本的存储本来也不启用缓存）。
func (s *Server) resolveSourceStore(ctx context.Context, class, path string) (string, *sourceVersion, error) {
	candidates := []string{storeForClass(class, "")}
	if class == classDeliver {
		candidates = []string{storePrivate, storeThirdParty}
	}
	v, versioned := s.objects.(objectVersioner)
	for _, st := range candidates {
		var ver *sourceVersion
		var err error
		if versioned {
			var version string
			_, version, err = v.Version(ctx, st, path)
			ver = &sourceVersion{version: version}
		} else {
			_, err = s.objects.Head(ctx, st, path)
		}
		if err == nil {
			return st, ver, nil
		}
		if !errors.Is(err, errObjectNotFound) {
			return "", nil, err
		}
	}
	return "", nil, newAPIError(codeNotFound, "resource.object_missing")
}
