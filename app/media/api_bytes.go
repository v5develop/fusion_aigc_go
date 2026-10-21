package media

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

// 字节流上传：调用方把媒体字节放进请求体，服务端流式落临时文件、完整解码校验、按用途落桶。
// 适用于没有 URL 的素材：用户提交的字节、网关返回的字节产物、Java 本地生成的文件。
//
// 请求：POST /v1/mediasvc/ingest/upload?purpose=<用途>
//   - Idempotency-Key（必填）：同一个键 + 同一份字节 = 同一个结果；同一个键但字节不同 = 400。
//   - X-Deadline-Ms（可选）：本次请求最多处理的毫秒数，超过服务端上限时截到上限。
//   - Content-Type 只是提示，服务端按内容判断媒体类型。
//
// 体积上限按用途区分：
//   - user_input（用户输入）：MaxInputBytes，超过返回 422 source_rejected（check=source.size_exceeded）；
//   - 其它用途（我方产物，如 Java 本地加工的文件）：StoreLocalMaxBytes（默认 2 GiB，与 Java 的 GATEWAY_RESULT_MAX_BYTES 对齐），
//     超过返回 400 invalid_request（check=request.body_too_large），不把我方产物判成素材被拒。
//
// 注意：不走 withDeadline / idempotent 这两个中间件，它们会把整个请求体读进内存，对视频不可用。
// 这个路由的契约是 HTTP 字节流，go-zero 的 .api 描述不了原始请求体，因此不登记在 media.api 里，
// 完整说明见 docs/api/media-service.md。
func (s *Server) uploadBytes(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get(idempotencyHeader)
	if err := checkKey(key); err != nil {
		writeAPIError(w, err)
		return
	}
	purpose := r.URL.Query().Get("purpose")
	if err := checkPurpose(purpose); err != nil {
		writeAPIError(w, err)
		return
	}
	ctx := r.Context()
	if d := bytesDeadline(r, s.opCeiling); d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	limit, oversize := s.uploadLimit(purpose)
	r.Body = http.MaxBytesReader(w, r.Body, limit)

	jobCtx, dir, done, err := s.svc.begin(ctx)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	defer done()

	path := workFile(dir, "upload.bin")
	var digest [32]byte
	var size int64
	// 读请求体是 I/O，占输入名额，不占计算名额
	err = s.svc.withIO(jobCtx, func() error {
		var err error
		digest, size, err = copyWithDigest(path, r.Body)
		return err
	})
	if err != nil {
		var oe *opError
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &oe): // 排队超时等服务侧的瞬时错误
			writeAPIError(w, err)
		case errors.As(err, &tooLarge):
			writeAPIError(w, oversize)
		default:
			writeAPIError(w, invalid("request.body_read"))
		}
		return
	}
	if size == 0 {
		writeAPIError(w, invalid("request.body_empty"))
		return
	}

	// 摘要算出后再做幂等检查：重放直接返回已存的结果，同键不同体拒绝
	idemKey := "ingest/upload|" + key
	entry, state := s.idem.begin(idemKey, digest)
	switch state {
	case idemReplay:
		writeReplay(w, entry)
		return
	case idemConflict:
		writeAPIError(w, invalid("request.idempotency_key_reused"))
		return
	case idemBusy:
		w.Header().Set("Retry-After", "5")
		writeAPIError(w, newAPIError(codeTransient, "request.idempotency_in_flight"))
		return
	}
	// 没有成功就清掉"处理中"标记（非 2xx 不缓存），让同一个键可以重试；panic 按 500 结束（见 settleIdem）
	succeeded := false
	defer settleIdem(w, s.idem, idemKey, digest, &succeeded)

	jobCtx = withIdemKey(jobCtx, key)
	out, err := s.ingestLocal(jobCtx, purpose, path)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	body, err := json.Marshal(out)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	s.idem.finish(idemKey, digest, http.StatusOK, "application/json", body)
	succeeded = true
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// defaultStoreLocalMaxBytes 我方产物字节上传的默认上限（2 GiB），与 Java 侧 GATEWAY_RESULT_MAX_BYTES 一致。
const defaultStoreLocalMaxBytes = 2 << 30

// uploadLimit 按用途取字节上传的体积上限，并给出超限时的错误。
func (s *Server) uploadLimit(purpose string) (int64, *apiError) {
	if purpose == "user_input" {
		return s.svc.cfg.MaxInputBytes, newAPIError(codeSourceRejected, "source.size_exceeded")
	}
	limit := s.svc.cfg.StoreLocalMaxBytes
	if limit <= 0 {
		limit = defaultStoreLocalMaxBytes
	}
	return limit, newAPIError(codeInvalidRequest, "request.body_too_large")
}

// bytesDeadline 读 X-Deadline-Ms；没有或无效返回 0（不加额外期限）；超过服务端上限时截到上限。
func bytesDeadline(r *http.Request, ceiling time.Duration) time.Duration {
	ms, err := strconv.ParseInt(r.Header.Get("X-Deadline-Ms"), 10, 64)
	if err != nil || ms <= 0 {
		return 0
	}
	d := time.Duration(ms) * time.Millisecond
	if ceiling > 0 && d > ceiling {
		d = ceiling
	}
	return d
}

// copyWithDigest 把请求体流式写入 path，同时算 sha256 与字节数（不把整个请求体放进内存）。
func copyWithDigest(path string, body io.Reader) ([32]byte, int64, error) {
	var sum [32]byte
	f, err := os.Create(path)
	if err != nil {
		return sum, 0, err
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), body)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(path)
		return sum, n, copyErr
	}
	if closeErr != nil {
		return sum, n, closeErr
	}
	copy(sum[:], h.Sum(nil))
	return sum, n, nil
}
