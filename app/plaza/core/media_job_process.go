package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// MediaJobInput 一次视频处理的入参（取自被认领的模板行）。
type MediaJobInput struct {
	TemplateID int64
	VideoURL   string
	PosterURL  string // 已有封面（运营传的或上次抽的）；非空则不再抽帧、也不会被覆盖
}

// MediaJobResult 处理结果。PosterURL 非空 = 新抽出的封面；VideoURL 非空 = 转码后的新地址；Note 记录没做 / 做了什么。
type MediaJobResult struct {
	PosterURL string
	VideoURL  string
	Note      string
}

// permanentError 重试也不会好的失败（能力缺失、超限）：直接 FAILED，不白耗 3 次。
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

func permanentErr(err error) error { return &permanentError{err: err} }

func isPermanent(err error) bool {
	var pe *permanentError
	return errors.As(err, &pe)
}

// processMedia 对一个视频模板做后处理：缺封面则抽首帧；视频不是 H.264 MP4 则转码（有转码能力时）。
// 没有存储（Uploader 为空）= 没有任何处理能力：已有封面则放行并记备注，没有封面则永久失败。
func (s *Service) processMedia(ctx context.Context, in MediaJobInput) (*MediaJobResult, error) {
	if s.uploader == nil {
		if strings.TrimSpace(in.PosterURL) != "" {
			return &MediaJobResult{Note: "未处理：没有可用的媒体处理能力（未配置存储 / 媒体服务），沿用已有封面，视频未转码"}, nil
		}
		return nil, permanentErr(errors.New("没有可用的封面抽帧能力（未配置存储 / 媒体服务），请手动上传封面后重新处理"))
	}
	return s.uploader.processVideo(ctx, in)
}

func (u *Uploader) processVideo(ctx context.Context, in MediaJobInput) (*MediaJobResult, error) {
	res := &MediaJobResult{}
	if strings.TrimSpace(in.PosterURL) == "" {
		if u.cover == nil {
			return nil, permanentErr(errors.New("没有可用的封面抽帧能力，请手动上传封面后重新处理"))
		}
		cover, err := u.cover.Cover(ctx, in.VideoURL)
		switch {
		case errors.Is(err, ErrCoverUnavailable):
			return nil, permanentErr(errors.New("没有可用的封面抽帧能力（媒体服务 / ffmpeg 未配置），请手动上传封面后重新处理"))
		case err != nil:
			return nil, fmt.Errorf("封面抽帧失败: %w", err)
		case cover == "":
			return nil, errors.New("封面抽帧没有返回地址")
		}
		res.PosterURL = cover
	}
	newURL, note, err := u.transcodeVideo(ctx, in.VideoURL)
	if err != nil {
		return nil, err
	}
	res.VideoURL, res.Note = newURL, note
	return res, nil
}

// transcodeVideo 返回 (新地址, 备注)。新地址为空 = 没有转码，备注说明原因。
func (u *Uploader) transcodeVideo(ctx context.Context, videoURL string) (string, string, error) {
	if u.transcode == nil {
		return "", "未转码：没有转码能力（未配置 FFmpeg），视频沿用上传时的编码", nil
	}
	if !u.store.Owns(videoURL) {
		return "", "未转码：视频不在本站存储里（外部地址）", nil
	}
	probe := u.codec
	if probe == nil {
		probe = func(ctx context.Context, rawURL string) (*VideoCodec, error) {
			return ffprobeCodec(ctx, u.cfg.FFprobe, rawURL)
		}
	}
	c, err := probe(ctx, videoURL)
	switch {
	case errors.Is(err, errProbeUnavailable):
		return "", "未转码：ffprobe 不可用，无法判断视频编码", nil
	case err != nil:
		return "", "", fmt.Errorf("读取视频编码失败: %w", err)
	}
	if c.isPlainH264() && fileExt(strings.SplitN(videoURL, "?", 2)[0]) == "mp4" {
		return "", "已是 H.264 MP4，无需转码", nil
	}
	nu, err := u.transcode.Transcode(ctx, videoURL)
	if err != nil {
		return "", "", err
	}
	return nu, fmt.Sprintf("已由 %s 转码为 H.264 MP4", c.Codec), nil
}
