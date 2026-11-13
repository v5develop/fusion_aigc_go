package logic

import (
	"context"
	"encoding/json"
	"strings"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/config"
)

// adminImage 后台批次详情里的一张图：ref 是库里存的原始引用（mr:// 等，给运营复制），
// url 是现签的短期预览地址（有效期同 FreeWatermark.SignedTTLSeconds，默认 10 分钟）；签不出 / 不是可直接打开的引用时为 null。
type adminImage struct {
	Ref  string  `json:"ref"`
	URL  *string `json:"url"`
	ID   string  `json:"id,omitempty"`
	Role string  `json:"role,omitempty"`
}

type adminSelectedGroup struct {
	ModelIndex int          `json:"modelIndex"`
	Images     []adminImage `json:"images"`
}

type adminMaskPart struct {
	Part  string     `json:"part"`
	Image adminImage `json:"image"`
}

// adminTaskMedia 一张子任务涉及的全部图：送生图的图序 / 成品 / 服装蒙版 / 逐件蒙版。
// traceTaskId 是统一链路（/creation/tasks/{id}/spans）用的任务号（AiCreation_LF_…），不是 taskId。
type adminTaskMedia struct {
	TaskID      string          `json:"taskId"`
	TraceTaskID string          `json:"traceTaskId"`
	Inputs      []adminImage    `json:"inputs"`
	Output      *adminImage     `json:"output"`
	Mask        *adminImage     `json:"mask"`
	MaskParts   []adminMaskPart `json:"maskParts"`
}

type adminMedia struct {
	SelectedImages []adminSelectedGroup `json:"selectedImages"`
	Tasks          []adminTaskMedia     `json:"tasks"`
}

// buildAdminMedia 收集批次详情里所有图片引用并统一解析成可预览地址。
// 解析走与用户侧同一套签名（signPlainRefs / signWatermarkViews，带进程内缓存、整批失败时逐个重签）：
// mr:// 签普通地址，mrw:// 只签水印视图（永不签干净原件），http(s) 原样作为预览地址，
// 其它内部引用（aig:// 等，媒体服务签不了）与签失败的一律 url=null，前端显示占位 + 复制按钮。
func buildAdminMedia(ctx context.Context, signer processSigner, c config.FreeWatermarkConf, b *model.Batch, tasks []*model.Task, traceID func(*model.Task) string) adminMedia {
	m := adminMedia{SelectedImages: []adminSelectedGroup{}, Tasks: make([]adminTaskMedia, 0, len(tasks))}
	var all []*adminImage

	collect := func(imgs []adminImage) []adminImage {
		for i := range imgs {
			all = append(all, &imgs[i])
		}
		return imgs
	}

	var sel []model.SelectedImages
	if len(b.SelectedImages) > 0 {
		_ = json.Unmarshal(b.SelectedImages, &sel)
	}
	for _, g := range sel {
		m.SelectedImages = append(m.SelectedImages, adminSelectedGroup{ModelIndex: g.ModelIndex, Images: collect(toAdminImages(g.Images))})
	}

	for _, t := range tasks {
		tm := adminTaskMedia{TaskID: t.TaskID.String, Inputs: []adminImage{}, MaskParts: []adminMaskPart{}}
		if traceID != nil {
			tm.TraceTaskID = traceID(t)
		}
		var in []model.InputImage
		if len(t.InputImages) > 0 {
			_ = json.Unmarshal(t.InputImages, &in)
		}
		tm.Inputs = collect(toAdminImages(in))
		if t.ResultURL.Valid && strings.TrimSpace(t.ResultURL.String) != "" {
			tm.Output = &adminImage{Ref: t.ResultURL.String}
			all = append(all, tm.Output)
		}
		if t.MaskURL.Valid && strings.TrimSpace(t.MaskURL.String) != "" {
			tm.Mask = &adminImage{Ref: t.MaskURL.String}
			all = append(all, tm.Mask)
		}
		if len(t.MaskParts) > 0 {
			var parts []struct {
				Part    string `json:"part"`
				MaskURL string `json:"mask_url"`
			}
			if json.Unmarshal(t.MaskParts, &parts) == nil {
				for _, p := range parts {
					tm.MaskParts = append(tm.MaskParts, adminMaskPart{Part: p.Part, Image: adminImage{Ref: p.MaskURL}})
				}
				for i := range tm.MaskParts {
					all = append(all, &tm.MaskParts[i].Image)
				}
			}
		}
		m.Tasks = append(m.Tasks, tm)
	}
	// tm 按值追加进切片：Output/Mask 是共享指针，Inputs/MaskParts 共享底层数组，收集到的指针仍然有效。
	resolveAdminImages(ctx, signer, c, all)
	return m
}

func toAdminImages(in []model.InputImage) []adminImage {
	out := make([]adminImage, 0, len(in))
	for _, i := range in {
		out = append(out, adminImage{Ref: i.URL, ID: i.ID, Role: i.Role})
	}
	return out
}

// resolveAdminImages 就地填充 URL。空 ref → null。
func resolveAdminImages(ctx context.Context, signer processSigner, c config.FreeWatermarkConf, imgs []*adminImage) {
	vals := make([]string, len(imgs))
	ptrs := make([]*string, 0, len(imgs))
	for i, im := range imgs {
		vals[i] = strings.TrimSpace(im.Ref)
		if isPendingRef(vals[i]) || isWatermarkRef(vals[i]) {
			ptrs = append(ptrs, &vals[i])
		}
	}
	if len(ptrs) > 0 {
		resolveWatermarkViews(ctx, signer, c, ptrs) // 签不出 = 空串
	}
	for i, im := range imgs {
		v := vals[i]
		if v != "" && (strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://")) {
			u := v
			im.URL = &u
		}
	}
}
