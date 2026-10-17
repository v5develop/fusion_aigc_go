package logic

import (
	"context"
	"time"

	"aigc-local-life/app/digitalhuman/model"
	"aigc-local-life/app/digitalhuman/rpc/internal/svc"
	"aigc-local-life/app/digitalhuman/rpc/pb"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type AcceptAgreementLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAcceptAgreementLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AcceptAgreementLogic {
	return &AcceptAgreementLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *AcceptAgreementLogic) AcceptAgreement(in *pb.AgreementReq) (*pb.AgreementInfo, error) {
	if in.Code != "" && in.Code != agreementAigc {
		return nil, xerr.Newf(xerr.ParamError, "未知的协议 %s", in.Code)
	}
	ver := int64(l.svcCtx.Config.Agreement.AigcVersion)
	_, err := l.svcCtx.Agreement.Insert(l.ctx, &model.AfvUserAgreement{
		UserId: in.UserId, AgreementCode: agreementAigc, Version: ver, AcceptTime: time.Now(),
	})
	if err != nil && !isDuplicate(err) { // 重复同意视为成功
		return nil, err
	}
	return &pb.AgreementInfo{Code: agreementAigc, Version: int32(ver), Accepted: true}, nil
}
