package media

import (
	"context"
	"io"
	"os"
	"time"
)

// 字体入口（ingest / ingest/upload）：字体不是媒体，不走 ffmpeg 解码，只按文件头（魔数）识别。
// 识别到字体即按字体落桶（mime 固定为 font/*），体积仍受 MaxInputBytes 约束；其余内容走媒体的完整解码校验。

// fontSignature 文件头魔数到 mime 的映射。
var fontSignatures = []struct {
	magic string
	mime  string
}{
	{"\x00\x01\x00\x00", "font/ttf"}, // TrueType
	{"true", "font/ttf"},             // TrueType（Apple 变体）
	{"OTTO", "font/otf"},             // OpenType（CFF）
	{"ttcf", "font/collection"},      // TrueType / OpenType 集合
	{"wOFF", "font/woff"},            // WOFF
	{"wOF2", "font/woff2"},           // WOFF2
}

// fontMime 按文件头判断是不是字体；不是则返回 false。文件读不了也返回 false（交给媒体路径报错）。
func fontMime(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	head := make([]byte, 4)
	if _, err := io.ReadFull(f, head); err != nil {
		return "", false
	}
	for _, sig := range fontSignatures {
		if string(head) == sig.magic {
			return sig.mime, true
		}
	}
	return "", false
}

// storeFontFile 字体落桶：体积校验后直接落，不做媒体属性探测。
func (s *Server) storeFontFile(ctx context.Context, purpose, path, mime string) (MediaResult, error) {
	st, err := os.Stat(path)
	if err != nil {
		return MediaResult{}, internal(err)
	}
	if st.Size() > s.svc.cfg.MaxInputBytes {
		return MediaResult{}, newAPIError(codeSourceRejected, "source.size_exceeded")
	}
	return s.storeWithMeta(ctx, purpose, path, MediaMeta{Mime: mime, Size: st.Size()})
}

// ingestLocal 本地文件的统一入口（ingest 与字节上传共用）：字体按文件头分流，其余做完整解码校验后落桶。
// purpose 已由调用方校验。
func (s *Server) ingestLocal(ctx context.Context, purpose, path string) (MediaResult, error) {
	if mime, ok := fontMime(path); ok {
		return s.storeFontFile(ctx, purpose, path, mime)
	}
	validateStart := time.Now()
	if err := s.svc.Validate(ctx, path); err != nil {
		return MediaResult{}, err
	}
	logStage(ctx, "validate", validateStart)
	return s.storeFile(ctx, purpose, path)
}
