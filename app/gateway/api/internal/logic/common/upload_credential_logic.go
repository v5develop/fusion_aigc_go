// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package common

import (
	"context"
	"errors"
	"time"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	types0 "aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/storage"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type UploadCredentialLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 客户端直传 OSS 的临时凭证（限定只能写返回的 dir 目录）
func NewUploadCredentialLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UploadCredentialLogic {
	return &UploadCredentialLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *UploadCredentialLogic) UploadCredential(req *types.UploadCredentialReq) (resp *types0.UploadCredentialResp, err error) {
	ttl := time.Duration(l.svcCtx.Config.Upload.CredentialTTLMinutes) * time.Minute
	cred, err := l.svcCtx.Storage.Credentials(l.ctx, req.Kind, ctxdata.UserID(l.ctx), ttl)
	if errors.Is(err, storage.ErrDirectUploadUnsupported) {
		return nil, xerr.New(xerr.NotReady, "当前环境未开启直传（本地存储模式请用 POST /upload）")
	}
	if err != nil {
		l.Errorf("upload credentials: %v", err)
		return nil, xerr.New(xerr.ServerError, "获取上传凭证失败，请重试")
	}
	return &types0.UploadCredentialResp{
		AccessKeyId: cred.AccessKeyID, AccessKeySecret: cred.AccessKeySecret, SecurityToken: cred.SecurityToken,
		Expiration: cred.Expiration, Bucket: cred.Bucket, Host: cred.Host, Dir: cred.Dir, BaseUrl: cred.BaseURL,
	}, nil
}
