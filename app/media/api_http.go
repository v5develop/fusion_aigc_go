package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

// 契约路由的 HTTP 层：解析、校验、调用 Service、落桶。
// 不在这里写 ffmpeg 调用；能力都在 Service 里（av.go / ops.go）。

const (
	idempotencyHeader = "Idempotency-Key"
	maxRequestBody    = 1 << 20 // 请求体只有 JSON 指令，1M 足够
)

// readJSON 解析 JSON 请求体；失败是 invalid_request。
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeAPIError(w, invalid("request.body_invalid"))
		return false
	}
	return true
}

// withJob 占并发名额、建工作目录，执行 fn，成功写 200 JSON。适用于只需要本地文件的操作。
func (s *Server) withJob(w http.ResponseWriter, r *http.Request, fn func(ctx context.Context, dir string) (any, error)) {
	ctx, dir, done, err := s.svc.begin(r.Context())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	defer done()
	out, err := fn(ctx, dir)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// writeOK 写 200 JSON（用于自己占名额的 Service 方法）。
func (s *Server) writeOK(w http.ResponseWriter, r *http.Request, fn func() (any, error)) {
	out, err := fn()
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) ingest(w http.ResponseWriter, r *http.Request) {
	if err := checkKey(r.Header.Get(idempotencyHeader)); err != nil {
		writeAPIError(w, err)
		return
	}
	var req ingestReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := firstErr(checkIngestSource(req.Source), checkPurpose(req.Purpose)); err != nil {
		writeAPIError(w, err)
		return
	}
	if err := s.checkHeaderHost(req.Source); err != nil {
		writeAPIError(w, err)
		return
	}
	s.withJob(w, r, func(ctx context.Context, dir string) (any, error) {
		path, err := s.sourceFn(req.Source)(ctx, dir)
		if err != nil {
			return nil, err
		}
		// 字体按文件头通过；媒体做完整解码校验（见 ingestLocal）
		return s.ingestLocal(ctx, req.Purpose, path)
	})
}

func (s *Server) probe(w http.ResponseWriter, r *http.Request) {
	var req probeReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := checkSource(req.Source); err != nil {
		writeAPIError(w, err)
		return
	}
	s.withJob(w, r, func(ctx context.Context, dir string) (any, error) {
		path, err := s.sourceFn(req.Source)(ctx, dir)
		if err != nil {
			return nil, err
		}
		return s.probeMeta(ctx, path)
	})
}

func (s *Server) frames(w http.ResponseWriter, r *http.Request) {
	if err := checkKey(r.Header.Get(idempotencyHeader)); err != nil {
		writeAPIError(w, err)
		return
	}
	var req framesReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.Count < 1 || req.Count > maxFramesPerCall {
		writeAPIError(w, invalid("request.count_range"))
		return
	}
	if err := checkSource(req.Source); err != nil {
		writeAPIError(w, err)
		return
	}
	s.withJob(w, r, func(ctx context.Context, dir string) (any, error) {
		path, err := s.sourceFn(req.Source)(ctx, dir)
		if err != nil {
			return nil, err
		}
		frames, err := s.svc.Frames(ctx, path, req.Count)
		if err != nil {
			return nil, err
		}
		items := make([]FrameItem, 0, len(frames))
		for i, f := range frames {
			p := workFile(dir, fmt.Sprintf("frame-%d.jpg", i))
			if err := os.WriteFile(p, f.JPEG, 0o600); err != nil {
				return nil, err
			}
			// TODO(purpose): 抽帧契约没有 purpose，帧固定按 intermediate 落桶
			res, err := s.storeFile(ctx, "intermediate", p)
			if err != nil {
				return nil, err
			}
			items = append(items, FrameItem{TimestampS: f.Timestamp, Object: res.Object})
		}
		return FramesResp{Frames: items}, nil
	})
}

func (s *Server) cover(w http.ResponseWriter, r *http.Request) {
	if err := checkKey(r.Header.Get(idempotencyHeader)); err != nil {
		writeAPIError(w, err)
		return
	}
	var req coverReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := firstErr(checkSource(req.Source), checkPurpose(req.Purpose)); err != nil {
		writeAPIError(w, err)
		return
	}
	// Cover 自己占名额，这里不能再占，否则并发为 1 时死锁
	s.writeOK(w, r, func() (any, error) {
		jpeg, err := s.svc.Cover(r.Context(), s.sourceFn(req.Source))
		if err != nil {
			return nil, err
		}
		dir, cleanup, err := newTempDir(s.svc.cfg.TempDir)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		p := workFile(dir, "cover.jpg")
		if err := os.WriteFile(p, jpeg, 0o600); err != nil {
			return nil, err
		}
		return s.storeFile(r.Context(), req.Purpose, p)
	})
}

func (s *Server) watermark(w http.ResponseWriter, r *http.Request) {
	if err := checkKey(r.Header.Get(idempotencyHeader)); err != nil {
		writeAPIError(w, err)
		return
	}
	var req watermarkReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := firstErr(checkSource(req.Source), checkPurpose(req.Purpose)); err != nil {
		writeAPIError(w, err)
		return
	}
	s.writeOK(w, r, func() (any, error) {
		out, err := s.svc.Watermark(r.Context(), s.sourceFn(req.Source))
		if err != nil {
			return nil, err
		}
		defer out.Cleanup()
		return s.storeFile(r.Context(), req.Purpose, out.Path)
	})
}

func (s *Server) trim(w http.ResponseWriter, r *http.Request) {
	if err := checkKey(r.Header.Get(idempotencyHeader)); err != nil {
		writeAPIError(w, err)
		return
	}
	var req trimReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := firstErr(checkSource(req.Source), checkPurpose(req.Purpose),
		checkSeconds(req.Seconds, 0, maxSecondsParam, true)); err != nil {
		writeAPIError(w, err)
		return
	}
	s.withJob(w, r, func(ctx context.Context, dir string) (any, error) {
		path, err := s.sourceFn(req.Source)(ctx, dir)
		if err != nil {
			return nil, err
		}
		if err := s.svc.Validate(ctx, path); err != nil {
			return nil, err
		}
		// 比 seconds 短的输入不拒收，截出来就是原长度（拒收只在 conform 里做）
		out := workFile(dir, "trim.mp4")
		if err := s.svc.Trim(ctx, path, req.Seconds, out); err != nil {
			return nil, ffmpegFailure(ctx, err, "media.trim_failed")
		}
		return s.storeFile(ctx, req.Purpose, out)
	})
}

func (s *Server) conform(w http.ResponseWriter, r *http.Request) {
	if err := checkKey(r.Header.Get(idempotencyHeader)); err != nil {
		writeAPIError(w, err)
		return
	}
	var req conformReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := firstErr(checkSource(req.Source), checkPurpose(req.Purpose),
		checkSeconds(req.Seconds, 0, maxSecondsParam, false), checkRule(req.Rule)); err != nil {
		writeAPIError(w, err)
		return
	}
	s.withJob(w, r, func(ctx context.Context, dir string) (any, error) {
		path, err := s.sourceFn(req.Source)(ctx, dir)
		if err != nil {
			return nil, err
		}
		out, err := s.conformVideo(ctx, dir, path, req)
		if err != nil {
			return nil, err
		}
		return s.storeFile(ctx, req.Purpose, out)
	})
}

// imageConform 图片规整（契约 /v1/mediasvc/image/conform）。
func (s *Server) imageConform(w http.ResponseWriter, r *http.Request) {
	if err := checkKey(r.Header.Get(idempotencyHeader)); err != nil {
		writeAPIError(w, err)
		return
	}
	var req imageConformReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := firstErr(checkSource(req.Source), checkPurpose(req.Purpose), checkImageRule(req.Rule)); err != nil {
		writeAPIError(w, err)
		return
	}
	s.withJob(w, r, func(ctx context.Context, dir string) (any, error) {
		path, err := s.sourceFn(req.Source)(ctx, dir)
		if err != nil {
			return nil, err
		}
		out, err := s.conformImage(ctx, dir, path, req)
		if err != nil {
			return nil, err
		}
		return s.storeFile(ctx, req.Purpose, out)
	})
}

func (s *Server) compose(w http.ResponseWriter, r *http.Request) {
	if err := checkKey(r.Header.Get(idempotencyHeader)); err != nil {
		writeAPIError(w, err)
		return
	}
	var req composeReq
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.Sources) < 1 || len(req.Sources) > s.svc.cfg.MaxComposeInputs {
		writeAPIError(w, invalid("request.sources_count"))
		return
	}
	if err := checkPurpose(req.Purpose); err != nil {
		writeAPIError(w, err)
		return
	}
	srcs := make([]Source, 0, len(req.Sources))
	for _, src := range req.Sources {
		if err := checkSource(src); err != nil {
			writeAPIError(w, err)
			return
		}
		srcs = append(srcs, s.sourceFn(src))
	}
	s.writeOK(w, r, func() (any, error) {
		out, err := s.svc.ComposeSources(r.Context(), srcs)
		if err != nil {
			return nil, err
		}
		defer out.Cleanup()
		return s.storeFile(r.Context(), req.Purpose, out.Path)
	})
}

func (s *Server) extractAudio(w http.ResponseWriter, r *http.Request) {
	if err := checkKey(r.Header.Get(idempotencyHeader)); err != nil {
		writeAPIError(w, err)
		return
	}
	var req extractAudioReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := firstErr(checkSource(req.Source), checkPurpose(req.Purpose)); err != nil {
		writeAPIError(w, err)
		return
	}
	s.withJob(w, r, func(ctx context.Context, dir string) (any, error) {
		path, err := s.sourceFn(req.Source)(ctx, dir)
		if err != nil {
			return nil, err
		}
		out := workFile(dir, "audio.mp3")
		if err := s.svc.ExtractAudio(ctx, path, out); err != nil {
			return nil, ffmpegFailure(ctx, err, "media.extract_audio_failed")
		}
		return s.storeFile(ctx, req.Purpose, out)
	})
}

func (s *Server) adjustDuration(w http.ResponseWriter, r *http.Request) {
	if err := checkKey(r.Header.Get(idempotencyHeader)); err != nil {
		writeAPIError(w, err)
		return
	}
	var req adjustDurationReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := firstErr(checkSource(req.Source), checkPurpose(req.Purpose), checkFactor(req.Factor)); err != nil {
		writeAPIError(w, err)
		return
	}
	s.withJob(w, r, func(ctx context.Context, dir string) (any, error) {
		path, err := s.sourceFn(req.Source)(ctx, dir)
		if err != nil {
			return nil, err
		}
		out := workFile(dir, "adjusted.mp4")
		if err := s.svc.AdjustDuration(ctx, path, req.Factor, out); err != nil {
			return nil, ffmpegFailure(ctx, err, "media.adjust_duration_failed")
		}
		return s.storeFile(ctx, req.Purpose, out)
	})
}

func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	if err := checkKey(r.Header.Get(idempotencyHeader)); err != nil {
		writeAPIError(w, err)
		return
	}
	var req deleteReq
	if !readJSON(w, r, &req) {
		return
	}
	// 两种写法：{"rid": "..."}（系统内首选）或 {"object": {"store", "key"}}（历史兼容）
	store, path := req.Object.Store, req.Object.Key
	if req.Rid != "" {
		st, p, err := s.objectPath(r.Context(), req.Rid)
		if errors.Is(err, errObjectNotFound) {
			writeJSON(w, http.StatusOK, deleteResp{Deleted: false, State: deleteStateAbsent})
			return
		}
		if err != nil {
			writeAPIError(w, err)
			return
		}
		store, path = st, p
	}
	if !validStore(store) {
		writeAPIError(w, invalid("request.object_store"))
		return
	}
	res, err := s.softDelete(r.Context(), store, path)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	// 本机缓存里的副本不再需要（正确性不靠这里：读取时总会对照待删除标记与存储版本，别的实例删除同样生效）
	if req.Rid != "" {
		s.objCache.forget(req.Rid)
	}
	out := deleteResp{Deleted: res.state != deleteStateAbsent, State: res.state}
	if !res.deleteAfter.IsZero() {
		out.DeleteAfter = res.deleteAfter.Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, out)
}

// 删除结果的状态（deleteResp.state）。
const (
	deleteStatePending = "pending_delete"
	deleteStateAbsent  = "absent"
)

type softDeleteResult struct {
	state       string
	deleteAfter time.Time
}

// softDelete 软删除（设计文档 §8.5）：只登记待删除标记，不删字节。宽限期过后由清理任务物理删除。
// 用户输入与历史路径永不删（403）。对象不存在返回 absent。已有标记时幂等，沿用原到期时间。
func (s *Server) softDelete(ctx context.Context, store, path string) (softDeleteResult, error) {
	if !deletable(path) {
		return softDeleteResult{}, errUserInputProtected
	}
	_, err := s.objects.Head(ctx, store, path)
	if errors.Is(err, errObjectNotFound) {
		return softDeleteResult{state: deleteStateAbsent}, nil
	}
	if err != nil {
		return softDeleteResult{}, err
	}
	deadline := s.now().Add(s.grace).Truncate(time.Second)
	at, _, err := s.objects.CreatePending(ctx, store, path, deadline)
	if err != nil {
		return softDeleteResult{}, err
	}
	return softDeleteResult{state: deleteStatePending, deleteAfter: at}, nil
}

// restore 在宽限期内撤销软删除（设计文档 §8.5）。宽限期已过返回 409 grace_expired，清理任务随后会删除它。
func (s *Server) restore(w http.ResponseWriter, r *http.Request) {
	var req restoreReq
	if !readJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	store, path, err := s.objectPath(ctx, req.Rid)
	if err != nil {
		writeAPIError(w, ridNotFound(err))
		return
	}
	deadline, pending, err := s.objects.PendingDeadline(ctx, store, path)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if !pending {
		// 不在待删除状态：对象存在则幂等返回 restored=false，不存在返回 404
		if _, err := s.objects.Head(ctx, store, path); err != nil {
			if errors.Is(err, errObjectNotFound) {
				writeAPIError(w, newAPIError(codeNotFound, "resource.object_missing"))
				return
			}
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, restoreResp{Rid: req.Rid, Restored: false})
		return
	}
	if !s.now().Before(deadline) {
		writeAPIError(w, newAPIError(codeGraceExpired, "resource.grace_expired"))
		return
	}
	if err := s.objects.ClearPending(ctx, store, path); err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, restoreResp{Rid: req.Rid, Restored: true})
}

// checkReadable 待删除标记中的对象不可读（410 resource_deleted）。未标记的照常。
func (s *Server) checkReadable(ctx context.Context, store, path string) error {
	_, pending, err := s.objects.PendingDeadline(ctx, store, path)
	if err != nil {
		return err
	}
	if pending {
		return newAPIError(codeResourceDeleted, "resource.pending_delete")
	}
	return nil
}

// capabilities 告诉调用方服务端现在能做什么（水印依赖 logo 是否已配置）。
func (s *Server) capabilities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{
		"ingest": true, "probe": true, "frames": true, "cover": true, "watermark": s.svc.logoAvailable(),
		"trim": true, "conform": true, "compose": true, "extract_audio": true, "adjust_duration": true, "delete": true, "restore": true,
		"image_resize": true, "font": true,
		"image_watermark": true, "image_grid": true, "image_crop": true, "image_mark": true,
	})
}

// firstErr 返回第一个非 nil 错误。
func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func checkFactor(f float64) error {
	if f <= 0 || f > 100 {
		return invalid("request.factor_range")
	}
	return nil
}

func newTempDir(root string) (string, func(), error) {
	dir, err := os.MkdirTemp(root, "media-")
	if err != nil {
		return "", nil, err
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}
