// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package common

import (
	"context"
	"errors"
	"strings"
	"time"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/storage"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// uploadExts 允许的文件类型 → 扩展名（对象 key 由后端生成，扩展名与类型一致，成品链接能正常预览/播放）
var uploadExts = map[string]string{
	"image/jpeg": "jpg", "image/png": "png", "image/webp": "webp", "image/gif": "gif", "image/heic": "heic",
	// 视频格式与「爆款视频复刻」options 下发的 video_formats 保持一致，否则前端选得到却传不上
	"video/mp4": "mp4", "video/quicktime": "mov", "video/x-msvideo": "avi",
	"video/x-matroska": "mkv", "video/x-flv": "flv", "video/x-ms-wmv": "wmv",
}

type PresignUploadLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 预签名上传地址（推荐）：后端签好 PUT 地址，前端直接 PUT 文件，无需密钥与签名
func NewPresignUploadLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PresignUploadLogic {
	return &PresignUploadLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// resolvePresignContentType 取请求的文件类型：content_type 优先，其次 contentType（与 Java 统一），都没有返回空串。
func resolvePresignContentType(snake, camel string) string {
	if v := strings.TrimSpace(snake); v != "" {
		return v
	}
	return strings.TrimSpace(camel)
}

func (l *PresignUploadLogic) PresignUpload(req *types.PresignUploadReq) (resp *types.PresignUploadResp, err error) {
	raw := resolvePresignContentType(req.ContentType, req.ContentTypeCamel)
	if raw == "" {
		return nil, xerr.New(xerr.ParamError, "缺少文件类型：请传 content_type（或 contentType）")
	}
	ct := strings.ToLower(raw)
	ext, ok := uploadExts[ct]
	if !ok || !strings.HasPrefix(ct, req.Kind+"/") {
		return nil, xerr.Newf(xerr.ParamError, "不支持的文件类型 %s（图片 jpeg/png/webp/gif/heic，视频 mp4/mov/avi/mkv/flv/wmv）", raw)
	}
	key := storage.ObjectKey(l.svcCtx.Config.Storage.Prefix, req.Kind, ctxdata.UserID(l.ctx), ext)
	ttl := time.Duration(l.svcCtx.Config.Upload.CredentialTTLMinutes) * time.Minute
	p, err := l.svcCtx.Storage.PresignPut(l.ctx, key, ct, ttl)
	if errors.Is(err, storage.ErrDirectUploadUnsupported) {
		return nil, xerr.New(xerr.NotReady, "当前环境未开启直传（本地存储模式请用 POST /upload）")
	}
	if err != nil {
		l.Errorf("presign upload: %v", err)
		return nil, xerr.New(xerr.ServerError, "获取上传地址失败，请重试")
	}
	return &types.PresignUploadResp{PutUrl: p.PutURL, Url: p.URL, ContentType: ct, ExpiresAt: p.ExpiresAt.UnixMilli()}, nil
}
