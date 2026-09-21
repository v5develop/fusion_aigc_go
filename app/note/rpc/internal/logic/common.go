package logic

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"aigc-life/app/note/model"
	"aigc-life/app/note/rpc/internal/svc"
	"aigc-life/app/note/rpc/pb"
	"aigc-life/pkg/aigc"
	"aigc-life/pkg/pagex"
	"aigc-life/pkg/xerr"

	"github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/logx"
)

// noteTypes 笔记类型。Label 同时作为 74 的 note_type 传入（文档示例为「种草」）。
// 上传区文案来自设计稿（知识/情感两种见「图文笔记-知识/情感」页，2026-09-21 新增）。
var noteTypes = []*pb.NoteType{
	{Code: "explore", Label: "探店", MaterialLabel: "上传门店素材", MaterialHint: "上传参考素材，建议上传门头、店内环境照片"},
	{Code: "seeding", Label: "种草", MaterialLabel: "上传商品素材", MaterialHint: "上传参考素材，建议上传商品图、使用场景图"},
	{Code: "emotion", Label: "情感", MaterialLabel: "上传素材", MaterialHint: "上传参考素材，支持知识配图/情感场景图，AI将参考风格"},
	{Code: "knowledge", Label: "知识", MaterialLabel: "上传素材", MaterialHint: "上传参考素材，支持知识配图/情感场景图，AI将参考风格"},
}

func noteTypeLabel(code string) string {
	for _, t := range noteTypes {
		if t.Code == code {
			return t.Label
		}
	}
	return ""
}

func nullStr(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

func runeLen(s string) int { return len([]rune(s)) }

func isDuplicate(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

func jsonList(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func parseList(s sql.NullString) []string {
	var v []string
	if s.Valid {
		_ = json.Unmarshal([]byte(s.String), &v)
	}
	return v
}

func toInfo(n *model.AfvImageTextNote) *pb.NoteInfo {
	return &pb.NoteInfo{
		Id: n.Id, UserId: n.UserId, NoteType: n.NoteType, NoteTypeLabel: noteTypeLabel(n.NoteType), Topic: n.Topic,
		MaterialUrls: parseList(n.MaterialUrls), DigitalHumanId: n.DigitalHumanId.Int64, Title: n.Title,
		Content: n.Content.String, Tags: parseList(n.Tags), ImageCount: int32(n.ImageCount),
		ImageUrls: parseList(n.ImageUrls), Status: int32(n.Status), FailReason: n.FailReason.String,
		CreateTime: n.CreateTime.UnixMilli(),
	}
}

func findOwned(ctx context.Context, svcCtx *svc.ServiceContext, id, userID int64) (*model.AfvImageTextNote, error) {
	n, err := svcCtx.Note.FindOwned(ctx, id, userID)
	if errors.Is(err, model.ErrNotFound) {
		return nil, xerr.NotFoundf("笔记")
	}
	return n, err
}

// requireAigc 执行面未配置时，构思与生成直接拒绝。
func requireAigc(svcCtx *svc.ServiceContext) error {
	if !svcCtx.Aigc.Enabled() {
		return xerr.New(xerr.NotReady, "AI 生成服务暂未开放，请稍后再试")
	}
	return nil
}

// checkText 内容审核。命中敏感词返回参数错误，读词库失败只记日志不拦截。
func checkText(ctx context.Context, svcCtx *svc.ServiceContext, scene, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	r, err := svcCtx.Moderation.Check(ctx, scene, text)
	if err != nil {
		logx.WithContext(ctx).Errorf("moderation %s: %v", scene, err)
		return nil
	}
	if !r.Passed {
		logx.WithContext(ctx).Infof("moderation blocked scene=%s label=%s", scene, r.Label)
		return xerr.New(xerr.ParamError, "内容包含敏感信息，请修改后重试")
	}
	return nil
}

// normalizeTags 话题统一带 #，去空去重，限制个数。
func normalizeTags(tags []string, max int) []string {
	out := make([]string, 0, len(tags))
	seen := map[string]bool{}
	for _, t := range tags {
		t = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(t), "#＃"))
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, "#"+t)
		if len(out) == max {
			break
		}
	}
	return out
}

// checkNoteText 校验并审核用户在规划页 / 编辑页提交的文字。
func checkNoteText(ctx context.Context, svcCtx *svc.ServiceContext, scene, title, content string, tags []string) error {
	lim := svcCtx.Config.Limits
	switch {
	case strings.TrimSpace(title) == "":
		return xerr.Param("请输入笔记标题")
	case runeLen(title) > lim.MaxTitleLen:
		return xerr.Newf(xerr.ParamError, "标题最多 %d 字", lim.MaxTitleLen)
	case strings.TrimSpace(content) == "":
		return xerr.Param("请输入笔记正文")
	case runeLen(content) > lim.MaxContentLen:
		return xerr.Newf(xerr.ParamError, "正文最多 %d 字", lim.MaxContentLen)
	}
	return checkText(ctx, svcCtx, scene, title+"\n"+content+"\n"+strings.Join(tags, " "))
}

// ---- 74 构思结果 ----

// planScript 74 返回的 script（结构以 AIGC 同事提供的真实样例为准）：
//
//	{"note": {"title", "content", "tags": ["#.."]},
//	 "images": [{"index", "image_prompt", "reference_materials": [1, 2]}],
//	 "images_url": ["参考图1（数字人形象）", "参考图2（素材）", ...]}
//
// reference_materials 是 images_url 的 1 起下标，所以 images 与 images_url 必须原样一起交给 75。
type planScript struct {
	Note struct {
		Title   string   `json:"title"`
		Content string   `json:"content"`
		Tags    []string `json:"tags"`
	} `json:"note"`
	Images []struct {
		Index              int    `json:"index"`
		ImagePrompt        string `json:"image_prompt"`
		ReferenceMaterials []int  `json:"reference_materials"`
	} `json:"images"`
	ImagesURL []string `json:"images_url"`
}

func parseScript(raw []byte) (*planScript, error) {
	var s planScript
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	if strings.TrimSpace(s.Note.Title) == "" && strings.TrimSpace(s.Note.Content) == "" {
		return nil, errors.New("script 缺少 note.title / note.content")
	}
	if len(s.Images) == 0 {
		return nil, errors.New("script 缺少 images")
	}
	sort.SliceStable(s.Images, func(i, j int) bool { return s.Images[i].Index < s.Images[j].Index })
	return &s, nil
}

// imagesTask 用构思结果组装 75 的入参：按 index 取前 count 条，index 与参考图列表原样沿用（同 74→75 真实交互）。
func imagesTask(svcCtx *svc.ServiceContext, s *planScript, count int) aigc.NoteImages {
	count = min(count, len(s.Images))
	images := make([]aigc.NoteImage, 0, count)
	for _, img := range s.Images[:count] {
		refs := img.ReferenceMaterials
		if refs == nil {
			refs = []int{} // 空数组 = 纯文生图；不能传 null
		}
		images = append(images, aigc.NoteImage{Index: img.Index, ImagePrompt: img.ImagePrompt, ReferenceMaterials: refs})
	}
	cfg := svcCtx.Config.Generate
	return aigc.NoteImages{Images: images, ImageURLs: s.ImagesURL, Model: cfg.Model, Size: cfg.Size}
}

func list(ctx context.Context, svcCtx *svc.ServiceContext, in *pb.ListReq) (*pb.NoteList, error) {
	limit, offset := pagex.Offset(in.Page, in.PageSize)
	rows, total, err := svcCtx.Note.List(ctx, model.ListFilter{UserID: in.UserId, Status: int64(in.Status), Limit: limit, Offset: offset})
	if err != nil {
		return nil, err
	}
	resp := &pb.NoteList{List: make([]*pb.NoteInfo, 0, len(rows)), Total: total}
	for _, n := range rows {
		resp.List = append(resp.List, toInfo(n))
	}
	return resp, nil
}
