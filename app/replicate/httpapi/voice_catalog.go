package httpapi

import (
	"context"
	"errors"

	"aigc-local-life/app/digitalhuman/model"
	"aigc-local-life/app/replicate/core"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// voiceCatalog 换声音的音色目录：只读复用数字人音色表（afv_digital_human_voice），只认启用且未删除的音色；不改 digitalhuman。
type voiceCatalog struct {
	m model.AfvDigitalHumanVoiceModel
}

func newVoiceCatalog(conn sqlx.SqlConn) core.VoiceCatalog {
	return voiceCatalog{m: model.NewAfvDigitalHumanVoiceModel(conn)}
}

func toVoiceInfo(v *model.AfvDigitalHumanVoice) core.VoiceInfo {
	return core.VoiceInfo{ID: v.Id, Name: v.Name, Voice: v.Voice, Gender: v.Gender, Style: v.Style, PreviewURL: v.PreviewUrl.String}
}

func (c voiceCatalog) ListVoices(ctx context.Context) ([]core.VoiceInfo, error) {
	rows, err := c.m.List(ctx, "", true)
	if err != nil {
		return nil, err
	}
	out := make([]core.VoiceInfo, 0, len(rows))
	for _, v := range rows {
		out = append(out, toVoiceInfo(v))
	}
	return out, nil
}

func (c voiceCatalog) GetVoice(ctx context.Context, id int64) (*core.VoiceInfo, error) {
	v, err := c.m.FindAlive(ctx, id)
	if errors.Is(err, model.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if v.Status != 1 {
		return nil, nil
	}
	info := toVoiceInfo(v)
	return &info, nil
}
