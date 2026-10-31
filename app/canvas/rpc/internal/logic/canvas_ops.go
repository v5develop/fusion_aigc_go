package logic

import (
	"context"
	"database/sql"
	"strings"
	"unicode/utf8"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/config"
	"aigc-local-life/app/canvas/rpc/internal/repo"
	"aigc-local-life/pkg/workspace"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

const defaultTitle = "未命名画布"

func (c *core) cleanTitle(t string) (string, error) {
	t = strings.TrimSpace(t)
	if t == "" {
		return defaultTitle, nil
	}
	if utf8.RuneCountInString(t) > c.s.Config.Limits.MaxTitleLen {
		return "", xerr.Param("标题最多 100 字")
	}
	return t, nil
}

// imageLimit 某个模型节点的参考图上限：节点 data.maxImageRefs 与 catalog（配置 ∩ gateway）取小；没有 data 值按 catalog。
func (c *core) imageLimit(n *Node) int {
	cat := 0
	if n != nil && n.Type == "model" {
		if m, ok := c.s.Config.FindModel(strData(n.Data, "model"), strData(n.Data, "modelId"), ""); ok {
			cat = c.catalogImageRefs(m)
		}
	}
	v := DefaultImageLimit(n)
	if n != nil && intData(n.Data, "maxImageRefs") <= 0 && cat > 0 {
		return cat
	}
	if cat > 0 && v > cat {
		return cat
	}
	return v
}

func toSummary(x *model.Canvas) *canvas.CanvasSummary {
	return &canvas.CanvasSummary{
		Id: x.ID, Title: x.Title, Revision: x.Revision, NodeCount: x.NodeCount, CoverUrl: x.CoverURL.String,
		CreatedAt: ms(x.CreatedAt), UpdatedAt: ms(x.UpdatedAt),
	}
}

func toDoc(x *model.Canvas, snapshot []byte) *canvas.CanvasDoc {
	return &canvas.CanvasDoc{
		Id: x.ID, Title: x.Title, Revision: x.Revision, Snapshot: string(snapshot), NodeCount: x.NodeCount,
		CoverUrl: x.CoverURL.String, CreatedAt: ms(x.CreatedAt), UpdatedAt: ms(x.UpdatedAt),
	}
}

func (c *core) ListCanvases(ctx context.Context, in *canvas.ListCanvasesReq) (*canvas.ListCanvasesResp, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	off, lim := repo.Page(int64(in.Page), int64(in.PageSize), 20, 50)
	list, total, err := c.s.Repo.Canvas.ListByUser(ctx, in.UserId, off, lim)
	if err != nil {
		return nil, err
	}
	out := &canvas.ListCanvasesResp{Total: total, List: make([]*canvas.CanvasSummary, 0, len(list))}
	for _, x := range list {
		out.List = append(out.List, toSummary(x))
	}
	return out, nil
}

// insertCanvas 落一张新画布（快照已校验、已剥离）。
func (c *core) insertCanvas(ctx context.Context, userID, ws int64, title string, d *Doc) (*model.Canvas, []byte, error) {
	if ws == 0 {
		if id, err := workspace.PersonalID(ctx, c.s.Conn, userID); err == nil {
			ws = id
		} else {
			logx.WithContext(ctx).Errorf("canvas: 查用户 %d 个人空间失败: %v", userID, err)
		}
	}
	snap := d.Marshal()
	now := c.now()
	x := &model.Canvas{
		ID: NewCanvasID(), UserID: userID, WorkspaceID: ws, Title: title, Revision: NewRevision(now),
		NodeCount: d.NodeCount(), Snapshot: snap,
	}
	if cv := d.Cover(); cv != "" {
		x.CoverURL = sql.NullString{String: cv, Valid: true}
	}
	if err := c.s.Repo.Canvas.Insert(ctx, x); err != nil {
		return nil, nil, err
	}
	x.CreatedAt, x.UpdatedAt = now, now
	return x, snap, nil
}

func (c *core) CreateCanvas(ctx context.Context, in *canvas.CreateCanvasReq) (*canvas.CanvasDoc, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	title, err := c.cleanTitle(in.Title)
	if err != nil {
		return nil, err
	}
	d, _ := ParseSnapshot([]byte(EmptySnapshot), 0)
	x, snap, err := c.insertCanvas(ctx, in.UserId, in.WorkspaceId, title, d)
	if err != nil {
		return nil, err
	}
	return toDoc(x, snap), nil
}

func (c *core) GetCanvas(ctx context.Context, in *canvas.CanvasIdReq) (*canvas.CanvasDoc, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	c.rememberBearer(ctx, in.UserId)
	x, err := c.s.Repo.Canvas.GetOwned(ctx, in.UserId, in.Id)
	if err != nil {
		return nil, err
	}
	if x == nil {
		return nil, xerr.NotFoundf("画布")
	}
	d, err := ParseSnapshot(x.Snapshot, 0)
	if err != nil {
		return nil, err
	}
	latest, err := c.s.Repo.Run.LatestByCanvas(ctx, x.ID)
	if err != nil {
		return nil, err
	}
	succeeded, err := c.s.Repo.Run.LatestSucceededByNode(ctx, x.ID)
	if err != nil {
		return nil, err
	}
	d.Backfill(latest, succeeded, c.progressOf)
	return toDoc(x, d.Marshal()), nil
}

// prepareSnapshot 解析 + 校验 + 剥离运行态。canvasID 非空时按该画布已有 run 的节点剥成品字段。
func (c *core) prepareSnapshot(ctx context.Context, canvasID string, raw []byte) (*Doc, error) {
	d, err := ParseSnapshot(raw, c.s.Config.Limits.MaxSnapshotBytes)
	if err != nil {
		return nil, err
	}
	if err := ValidateDoc(d, c.s.Config.Limits.MaxNodes, c.imageLimit); err != nil {
		return nil, err
	}
	hasRun := map[string]bool{}
	if canvasID != "" {
		if hasRun, err = c.s.Repo.Run.NodesWithRuns(ctx, canvasID); err != nil {
			return nil, err
		}
	}
	d.StripRuntime(hasRun)
	return d, nil
}

func (c *core) SaveCanvas(ctx context.Context, in *canvas.SaveCanvasReq) (*canvas.SaveCanvasResp, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Snapshot) == "" {
		return nil, xerr.New(xerr.ParamMissing, "缺少 snapshot")
	}
	cur, err := c.s.Repo.Canvas.GetOwned(ctx, in.UserId, in.Id)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, xerr.NotFoundf("画布")
	}
	conflict := func(x *model.Canvas) *canvas.SaveCanvasResp {
		return &canvas.SaveCanvasResp{Conflict: true, Revision: x.Revision, UpdatedAt: ms(x.UpdatedAt), NodeCount: x.NodeCount, CoverUrl: x.CoverURL.String}
	}
	// 先比一次 revision：冲突时不必做整段校验
	ifMatch := strings.TrimSpace(in.IfMatchRevision)
	if in.Force {
		ifMatch = ""
	} else if ifMatch == "" {
		// 不带版本号又不 force 的保存会静默覆盖别的窗口的修改：拒绝，要求显式 force
		return nil, xerr.New(xerr.ParamMissing, "缺少 if_match_revision；确需覆盖保存请传 force=true")
	}
	if ifMatch != "" && ifMatch != cur.Revision {
		return conflict(cur), nil
	}
	d, err := c.prepareSnapshot(ctx, cur.ID, []byte(in.Snapshot))
	if err != nil {
		return nil, err
	}
	args := repo.SaveArgs{
		ID: cur.ID, UserID: in.UserId, NewRevision: NewRevision(c.now()), Snapshot: d.Marshal(),
		NodeCount: d.NodeCount(), CoverURL: d.Cover(), IfMatch: ifMatch,
	}
	if in.HasTitle {
		t, err := c.cleanTitle(in.Title)
		if err != nil {
			return nil, err
		}
		args.Title = &t
	}
	ok, err := c.s.Repo.Canvas.Save(ctx, args)
	if err != nil {
		return nil, err
	}
	if !ok {
		// 校验期间被别的窗口抢先保存（或刚被删除）
		now, err := c.s.Repo.Canvas.GetOwned(ctx, in.UserId, in.Id)
		if err != nil {
			return nil, err
		}
		if now == nil {
			return nil, xerr.NotFoundf("画布")
		}
		return conflict(now), nil
	}
	return &canvas.SaveCanvasResp{Revision: args.NewRevision, UpdatedAt: c.now().UnixMilli(), NodeCount: args.NodeCount, CoverUrl: args.CoverURL}, nil
}

func (c *core) DeleteCanvas(ctx context.Context, in *canvas.CanvasIdReq) (*canvas.Empty, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	ok, err := c.s.Repo.Canvas.SoftDelete(ctx, in.UserId, in.Id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, xerr.NotFoundf("画布")
	}
	return &canvas.Empty{}, nil
}

// SaveAsCanvas 复制已保存的快照成新画布（运行态与成品全部剥离，节点都是 idle）。
func (c *core) SaveAsCanvas(ctx context.Context, in *canvas.SaveAsCanvasReq) (*canvas.CanvasDoc, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	src, err := c.s.Repo.Canvas.GetOwned(ctx, in.UserId, in.SrcId)
	if err != nil {
		return nil, err
	}
	if src == nil {
		return nil, xerr.NotFoundf("画布")
	}
	title := in.Title
	if strings.TrimSpace(title) == "" {
		title = src.Title + " - 副本"
		if utf8.RuneCountInString(title) > c.s.Config.Limits.MaxTitleLen {
			title = string([]rune(title)[:c.s.Config.Limits.MaxTitleLen])
		}
	}
	if title, err = c.cleanTitle(title); err != nil {
		return nil, err
	}
	d, err := ParseSnapshot(src.Snapshot, 0)
	if err != nil {
		return nil, err
	}
	d.StripAllResults()
	x, snap, err := c.insertCanvas(ctx, in.UserId, src.WorkspaceID, title, d)
	if err != nil {
		return nil, err
	}
	return toDoc(x, snap), nil
}

// catalogImageRefs 模型档的参考图上限：配置值与 gateway feature_limits.max_reference_images 取小（gateway 有值时以它为准）。
func (c *core) catalogImageRefs(m config.ModelSpec) int {
	v := m.MaxImageRefs
	if g, ok := gatewayGroups.get(m.ModelGroup); ok && g.FeatureLimits.MaxReferenceImages != nil {
		gv := *g.FeatureLimits.MaxReferenceImages
		if v <= 0 || m.Kind == "video" || gv < v {
			v = gv
		}
	}
	return v
}
