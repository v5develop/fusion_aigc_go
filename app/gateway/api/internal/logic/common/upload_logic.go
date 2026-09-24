// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package common

import (
	"context"
	"io"
	"net/http"

	"aigc-life/app/gateway/api/internal/svc"
	"aigc-life/app/gateway/api/internal/types"
	"aigc-life/pkg/ctxdata"
	"aigc-life/pkg/storage"
	"aigc-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type UploadLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 上传照片（multipart 字段 file），返回公网地址
func NewUploadLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UploadLogic {
	return &UploadLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 允许的类型：按文件内容判断，不信任扩展名和客户端声明的类型。
// 这个代传接口只在本地开发用（线上前端直传 OSS），所以这里只要够联调即可。
var (
	imageExt = map[string]string{"image/jpeg": "jpg", "image/png": "png", "image/webp": "webp"}
	videoExt = map[string]string{"video/mp4": "mp4", "video/quicktime": "mov", "video/avi": "avi",
		"video/x-msvideo": "avi", "video/x-matroska": "mkv", "video/x-flv": "flv", "video/x-ms-wmv": "wmv"}
)

func (l *UploadLogic) Upload(r *http.Request) (resp *types.UploadResp, err error) {
	limit := l.svcCtx.Config.Upload.MaxVideoBytes
	if limit <= 0 {
		limit = l.svcCtx.Config.Upload.MaxImageBytes
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		return nil, xerr.Param("请选择要上传的文件（字段 file）")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, xerr.Param("读取文件失败")
	}
	if int64(len(data)) > limit {
		return nil, xerr.Newf(xerr.ParamError, "文件不能超过 %dMB", limit>>20)
	}
	ct := http.DetectContentType(data)
	kind := "image"
	ext, ok := imageExt[ct]
	if !ok {
		if ext, ok = videoExt[ct]; !ok {
			return nil, xerr.Param("只支持 JPG、PNG、WebP 照片或 MP4、MOV、AVI 视频")
		}
		kind = "video"
	}
	if kind == "image" && int64(len(data)) > l.svcCtx.Config.Upload.MaxImageBytes {
		return nil, xerr.Newf(xerr.ParamError, "照片不能超过 %dMB", l.svcCtx.Config.Upload.MaxImageBytes>>20)
	}
	key := storage.ObjectKey(l.svcCtx.Config.Storage.Prefix, kind, ctxdata.UserID(l.ctx), ext)
	url, err := l.svcCtx.Storage.Put(l.ctx, key, ct, data)
	if err != nil {
		l.Errorf("upload: %v", err)
		return nil, xerr.New(xerr.ServerError, "上传失败，请重试")
	}
	return &types.UploadResp{Url: url, Size: int64(len(data)), ContentType: ct}, nil
}
