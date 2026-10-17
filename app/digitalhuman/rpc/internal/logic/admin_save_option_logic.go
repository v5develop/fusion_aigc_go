package logic

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"aigc-local-life/app/digitalhuman/model"
	"aigc-local-life/app/digitalhuman/rpc/internal/svc"
	"aigc-local-life/app/digitalhuman/rpc/pb"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminSaveOptionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminSaveOptionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminSaveOptionLogic {
	return &AdminSaveOptionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *AdminSaveOptionLogic) AdminSaveOption(in *pb.OptionInfo) (*pb.OptionInfo, error) {
	if !validOptionTypes[in.OptionType] {
		return nil, xerr.Newf(xerr.ParamError, "不支持的选项类型 %s", in.OptionType)
	}
	code, label := strings.TrimSpace(in.Code), strings.TrimSpace(in.Label)
	if code == "" || label == "" {
		return nil, xerr.Param("编码和名称不能为空")
	}
	row := &model.AfvDigitalHumanOption{
		OptionType: in.OptionType, Code: code, Label: label, Value: nullStr(in.Value),
		SortOrder: int64(in.SortOrder), Status: int64(in.Status),
	}
	var err error
	if in.Id > 0 {
		old, ferr := l.svcCtx.Option.FindOne(l.ctx, in.Id)
		if errors.Is(ferr, model.ErrNotFound) || (ferr == nil && old.Deleted == 1) {
			return nil, xerr.NotFoundf("选项")
		}
		if ferr != nil {
			return nil, ferr
		}
		row.Id, row.DeletedId, row.CreateTime = old.Id, old.DeletedId, old.CreateTime
		err = l.svcCtx.Option.Update(l.ctx, row)
	} else {
		var res sql.Result
		if res, err = l.svcCtx.Option.Insert(l.ctx, row); err == nil {
			row.Id, err = res.LastInsertId()
		}
	}
	if isDuplicate(err) {
		return nil, xerr.New(xerr.Conflict, "同类型下编码已存在")
	}
	if err != nil {
		return nil, err
	}
	return toOption(row), nil
}
