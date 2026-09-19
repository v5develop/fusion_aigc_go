package logic

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"aigc-life/app/digitalhuman/model"
	"aigc-life/app/digitalhuman/rpc/internal/svc"
	"aigc-life/app/digitalhuman/rpc/pb"
	"aigc-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminSaveVoiceLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminSaveVoiceLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminSaveVoiceLogic {
	return &AdminSaveVoiceLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *AdminSaveVoiceLogic) AdminSaveVoice(in *pb.VoiceInfo) (*pb.VoiceInfo, error) {
	name, voice := strings.TrimSpace(in.Name), strings.TrimSpace(in.Voice)
	if name == "" || voice == "" {
		return nil, xerr.Param("名称和音色参数不能为空")
	}
	if in.Gender != genderFemale && in.Gender != genderMale {
		return nil, xerr.Param("性别只能是 female 或 male")
	}
	cats := in.Categories
	if cats == nil {
		cats = []string{}
	}
	catJSON, _ := json.Marshal(cats)
	row := &model.AfvDigitalHumanVoice{
		Name: name, Voice: voice, Gender: in.Gender, Style: strings.TrimSpace(in.Style), Categories: string(catJSON),
		PreviewUrl: nullStr(in.PreviewUrl), SortOrder: int64(in.SortOrder), Status: int64(in.Status),
	}
	var err error
	if in.Id > 0 {
		old, ferr := l.svcCtx.Voice.FindAlive(l.ctx, in.Id)
		if errors.Is(ferr, model.ErrNotFound) {
			return nil, xerr.NotFoundf("音色")
		}
		if ferr != nil {
			return nil, ferr
		}
		row.Id, row.DeletedId, row.CreateTime = old.Id, old.DeletedId, old.CreateTime
		if old.Voice != voice && in.PreviewUrl == old.PreviewUrl.String {
			row.PreviewUrl = sql.NullString{} // 换了音色参数，旧试听作废
		}
		err = l.svcCtx.Voice.Update(l.ctx, row)
	} else {
		var res sql.Result
		if res, err = l.svcCtx.Voice.Insert(l.ctx, row); err == nil {
			row.Id, err = res.LastInsertId()
		}
	}
	if isDuplicate(err) {
		return nil, xerr.New(xerr.Conflict, "该音色参数已存在")
	}
	if err != nil {
		return nil, err
	}
	return toVoice(row, true), nil
}
