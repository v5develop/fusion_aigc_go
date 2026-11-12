package logic

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/colorfixsolver"
	"aigc-local-life/pkg/xerr"
)

// fakeSolver 记录请求，按 resp / err 返回。
type fakeSolver struct {
	enabled bool
	got     []colorfixsolver.Request
	resp    *colorfixsolver.Response
	err     error
}

func (f *fakeSolver) Enabled() bool { return f.enabled }
func (f *fakeSolver) Solve(_ context.Context, r colorfixsolver.Request) (*colorfixsolver.Response, error) {
	f.got = append(f.got, r)
	return f.resp, f.err
}

const afPublic = "https://cdn.example.com/lib/result.png"

func afPayload(items string, garments string) json.RawMessage {
	return json.RawMessage(`{"items":` + items + `,"garment_images":` + garments + `}`)
}

// afDeps 一个本人成功的商拍任务：上衣 + 下装各有参考图；蒙版调用计数在 *calls。
func afDeps(sv *fakeSolver, calls *atomic.Int32) *autoFixDeps {
	task := &model.Task{UserID: 7, Kind: model.KindTryon, Status: model.TaskSucceeded, BatchID: 1}
	payload := afPayload(`["top","bottom"]`, `[
		{"url":"https://cdn.example.com/g/top-back.png","role":"上衣","angle":"back"},
		{"url":"https://cdn.example.com/g/top-front.png","role":"上衣","angle":"front"},
		{"url":"https://cdn.example.com/g/bottom-flat.png","role":"下装","angle":"flat"}]`)
	return &autoFixDeps{
		solver: sv,
		loadTask: func(_ context.Context, userID int64, taskID string) (*model.Task, json.RawMessage, error) {
			if userID != 7 || taskID != "tyo1" {
				return nil, nil, nil
			}
			return task, payload, nil
		},
		source: func(context.Context, int64, string) (string, string, string, bool, error) {
			return "", "", afPublic, true, nil
		},
		pieceMask: func(_ context.Context, _ int64, _, _, _ string, parts []string) (string, error) {
			calls.Add(1)
			return "https://cdn.example.com/mask/" + strings.Join(parts, "+") + ".png", nil
		},
		owns: func(u string) bool { return strings.HasPrefix(u, "https://cdn.example.com/") },
	}
}

func solved(key string, hue, sat, val, bri, con, score float64) colorfixsolver.Piece {
	return colorfixsolver.Piece{Key: key, Score: score, Params: colorfixsolver.Params{Hue: hue, Saturation: sat, Value: val, Brightness: bri, Contrast: con}}
}

func wantReason(t *testing.T, err error, code int, reason string) {
	t.Helper()
	if c, _, ok := xerr.Parse(err); !ok || c != code || xerr.ReasonOf(err) != reason {
		t.Fatalf("want code=%d reason=%s, got err=%v reason=%s", code, reason, err, xerr.ReasonOf(err))
	}
}

func TestAutoColorFixDisabled(t *testing.T) {
	var calls atomic.Int32
	for name, sv := range map[string]autoFixSolver{"nil": nil, "off": &fakeSolver{enabled: false}} {
		d := afDeps(&fakeSolver{enabled: true}, &calls)
		d.solver = sv
		loaded := false
		d.loadTask = func(context.Context, int64, string) (*model.Task, json.RawMessage, error) {
			loaded = true
			return nil, nil, nil
		}
		_, err := autoColorFix(context.Background(), d, &studio.AutoColorFixReq{UserId: 7, TaskId: "tyo1"})
		wantReason(t, err, xerr.NotReady, xerr.ReasonColorFixAutoDisabled)
		if loaded || calls.Load() != 0 {
			t.Fatalf("%s: 功能关闭时不该读库 / 识别", name)
		}
	}
}

func TestAutoColorFixOwnershipAndState(t *testing.T) {
	var calls atomic.Int32
	sv := &fakeSolver{enabled: true}
	d := afDeps(sv, &calls)
	// 别人的任务 / 不存在：4001，不碰成品、蒙版、求解
	_, err := autoColorFix(context.Background(), d, &studio.AutoColorFixReq{UserId: 8, TaskId: "tyo1"})
	wantReason(t, err, xerr.NotFound, "RESOURCE_NOT_FOUND")
	_, err = autoColorFix(context.Background(), d, &studio.AutoColorFixReq{UserId: 7, TaskId: "nope"})
	wantReason(t, err, xerr.NotFound, "RESOURCE_NOT_FOUND")
	// 缺用户 / 缺任务号
	if _, err = autoColorFix(context.Background(), d, &studio.AutoColorFixReq{TaskId: "tyo1"}); err == nil {
		t.Fatal("缺用户应报错")
	}
	if _, err = autoColorFix(context.Background(), d, &studio.AutoColorFixReq{UserId: 7}); err == nil {
		t.Fatal("缺任务号应报错")
	}
	// 未成功 / 非商拍
	task, payload, _ := d.loadTask(context.Background(), 7, "tyo1")
	task.Status = model.TaskRunning
	_, err = autoColorFix(context.Background(), d, &studio.AutoColorFixReq{UserId: 7, TaskId: "tyo1"})
	wantReason(t, err, xerr.Conflict, xerr.ReasonColorFixAutoNotReady)
	task.Status, task.Kind = model.TaskSucceeded, model.KindPose
	_, err = autoColorFix(context.Background(), d, &studio.AutoColorFixReq{UserId: 7, TaskId: "tyo1"})
	wantReason(t, err, xerr.ParamError, xerr.ReasonColorFixAutoNoReference)
	_ = payload
	if len(sv.got) != 0 || calls.Load() != 0 {
		t.Fatalf("校验失败不该调求解 / 识别: solver=%d mask=%d", len(sv.got), calls.Load())
	}
}

func TestAutoColorFixMappingClampingAndMatching(t *testing.T) {
	var calls atomic.Int32
	sv := &fakeSolver{enabled: true, resp: &colorfixsolver.Response{Version: "colorfix-solver/test", Pieces: []colorfixsolver.Piece{
		// 越界值夹到 UpdateTaskColorFix 的范围，小数四舍五入；分数夹到 0–100
		solved("top", 99, -99, 41.4, -90.6, 12.5, 123.456),
		solved("BOTTOM", -9, 2, 0, -7, 3, 58.04),
	}}}
	resp, err := autoColorFix(context.Background(), afDeps(sv, &calls), &studio.AutoColorFixReq{UserId: 7, TaskId: " tyo1 "})
	if err != nil {
		t.Fatal(err)
	}
	if len(sv.got) != 1 {
		t.Fatalf("应只调一次求解: %d", len(sv.got))
	}
	req := sv.got[0]
	if req.ImageURL != afPublic {
		t.Fatalf("成品地址 %s", req.ImageURL)
	}
	// 参考图：上衣取正面（不是背面），下装取平铺；蒙版按单品部位取（tops / pants），顺序 = 载荷里的单品顺序
	if len(req.Reference) != 2 || req.Reference[0] != (colorfixsolver.Ref{Key: "top", URL: "https://cdn.example.com/g/top-front.png"}) ||
		req.Reference[1] != (colorfixsolver.Ref{Key: "bottom", URL: "https://cdn.example.com/g/bottom-flat.png"}) {
		t.Fatalf("参考图匹配不对: %+v", req.Reference)
	}
	if len(req.Masks) != 2 || req.Masks[0].URL != "https://cdn.example.com/mask/tops.png" || req.Masks[1].URL != "https://cdn.example.com/mask/pants.png" {
		t.Fatalf("蒙版匹配不对: %+v", req.Masks)
	}
	if resp.TaskId != "tyo1" || resp.Version != "colorfix-solver/test" || len(resp.Pieces) != 2 || len(resp.Skipped) != 0 {
		t.Fatalf("resp=%+v", resp)
	}
	top := resp.Pieces[0]
	if top.Key != "top" || top.Params.Hue != 30 || top.Params.Saturation != -60 || top.Params.Value != 40 || top.Params.Brightness != -80 ||
		top.Params.Contrast != 13 || top.Score != 100 {
		t.Fatalf("夹取 / 取整不对: %+v %+v", top, top.Params)
	}
	bot := resp.Pieces[1]
	if bot.Key != "bottom" || bot.Params.Hue != -9 || bot.Params.Saturation != 2 || bot.Params.Brightness != -7 || bot.Score != 58 {
		t.Fatalf("bottom %+v %+v", bot, bot.Params)
	}
	for _, p := range resp.Pieces {
		if err := ValidateColorFix(p.Params); err != nil {
			t.Fatalf("结果必须能直接交给 UpdateTaskColorFix: %v", err)
		}
	}
}

func TestAutoColorFixPiecesFilterAndLookFallback(t *testing.T) {
	var calls atomic.Int32
	sv := &fakeSolver{enabled: true, resp: &colorfixsolver.Response{Pieces: []colorfixsolver.Piece{solved("bottom", 1, 2, 3, 4, 5, 90)}}}
	d := afDeps(sv, &calls)
	// 只要下装；上衣不参与
	resp, err := autoColorFix(context.Background(), d, &studio.AutoColorFixReq{UserId: 7, TaskId: "tyo1", Pieces: []string{"bottom", "下装"}})
	if err != nil || len(resp.Pieces) != 1 || len(sv.got[0].Reference) != 1 || sv.got[0].Reference[0].Key != "bottom" || calls.Load() != 1 {
		t.Fatalf("filter resp=%+v err=%v got=%+v", resp, err, sv.got)
	}
	// 不认识的单品 / 整套不能当单品：1003
	for _, bad := range []string{"hat", "整套"} {
		_, err = autoColorFix(context.Background(), d, &studio.AutoColorFixReq{UserId: 7, TaskId: "tyo1", Pieces: []string{bad}})
		wantReason(t, err, xerr.ParamError, "PARAM_INVALID")
	}
	// 请求的单品不在批次里
	_, err = autoColorFix(context.Background(), d, &studio.AutoColorFixReq{UserId: 7, TaskId: "tyo1", Pieces: []string{"shoes"}})
	wantReason(t, err, xerr.ParamError, xerr.ReasonColorFixAutoNoReference)

	// 单品没有自己的图 → 退回「整套」图
	d2 := afDeps(sv, &calls)
	d2.loadTask = func(context.Context, int64, string) (*model.Task, json.RawMessage, error) {
		return &model.Task{UserID: 7, Kind: model.KindTryon, Status: model.TaskSucceeded}, afPayload(`["top"]`,
			`[{"url":"https://cdn.example.com/g/look.png","role":"整套","angle":"front"}]`), nil
	}
	sv.resp = &colorfixsolver.Response{Pieces: []colorfixsolver.Piece{solved("top", 0, 0, 0, 0, 0, 80)}}
	sv.got = nil
	if _, err = autoColorFix(context.Background(), d2, &studio.AutoColorFixReq{UserId: 7, TaskId: "tyo1"}); err != nil ||
		sv.got[0].Reference[0].URL != "https://cdn.example.com/g/look.png" {
		t.Fatalf("整套回退 err=%v got=%+v", err, sv.got)
	}
}

func TestAutoColorFixSkippedPieces(t *testing.T) {
	var calls atomic.Int32
	sv := &fakeSolver{enabled: true, resp: &colorfixsolver.Response{Pieces: []colorfixsolver.Piece{solved("top", 1, 1, 1, 1, 1, 70)}}}
	d := afDeps(sv, &calls)
	d.pieceMask = func(_ context.Context, _ int64, _, _, _ string, parts []string) (string, error) {
		if parts[0] == "pants" {
			return "", errMaskNoRegion
		}
		return "https://cdn.example.com/mask/tops.png", nil
	}
	resp, err := autoColorFix(context.Background(), d, &studio.AutoColorFixReq{UserId: 7, TaskId: "tyo1"})
	if err != nil || len(resp.Pieces) != 1 || len(resp.Skipped) != 1 || resp.Skipped[0].Key != "bottom" || resp.Skipped[0].Reason != skipNoMask {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if len(sv.got[0].Reference) != 1 || sv.got[0].Reference[0].Key != "top" {
		t.Fatalf("没蒙版的单品不该送求解: %+v", sv.got[0])
	}

	// 求解端漏了一件 / 给了 NaN → unsolved，不当作成功
	sv.resp = &colorfixsolver.Response{Pieces: []colorfixsolver.Piece{solved("top", math.NaN(), 0, 0, 0, 0, 70)}}
	d.pieceMask = func(context.Context, int64, string, string, string, []string) (string, error) {
		return "https://cdn.example.com/m.png", nil
	}
	_, err = autoColorFix(context.Background(), d, &studio.AutoColorFixReq{UserId: 7, TaskId: "tyo1"})
	wantReason(t, err, xerr.NotFound, xerr.ReasonColorFixAutoNoPiece)

	// 参考图地址不是本存储 / 白名单：不送求解，按没参考图
	d.owns = func(string) bool { return false }
	_, err = autoColorFix(context.Background(), d, &studio.AutoColorFixReq{UserId: 7, TaskId: "tyo1"})
	wantReason(t, err, xerr.ParamError, xerr.ReasonColorFixAutoNoReference)
}

func TestAutoColorFixNoMaskAnywhere(t *testing.T) {
	var calls atomic.Int32
	sv := &fakeSolver{enabled: true}
	d := afDeps(sv, &calls)
	d.pieceMask = func(context.Context, int64, string, string, string, []string) (string, error) {
		return "", errMaskNoRegion
	}
	_, err := autoColorFix(context.Background(), d, &studio.AutoColorFixReq{UserId: 7, TaskId: "tyo1"})
	wantReason(t, err, xerr.NotFound, xerr.ReasonColorFixAutoNoPiece)
	// 识别服务整体不可用：可重试的 6001，不是「没识别到」
	d.pieceMask = func(context.Context, int64, string, string, string, []string) (string, error) {
		return "", errMaskUnavailable
	}
	_, err = autoColorFix(context.Background(), d, &studio.AutoColorFixReq{UserId: 7, TaskId: "tyo1"})
	wantReason(t, err, xerr.ThirdPartyError, xerr.ReasonColorFixAutoUnavailable)
	if len(sv.got) != 0 {
		t.Fatal("没有蒙版不该调求解")
	}
}

// 子任务已存的逐件蒙版优先：单部位的单品不再走区域识别。
func TestAutoColorFixUsesStoredPartMask(t *testing.T) {
	var calls atomic.Int32
	sv := &fakeSolver{enabled: true, resp: &colorfixsolver.Response{Pieces: []colorfixsolver.Piece{solved("top", 1, 1, 1, 1, 1, 70), solved("bottom", 1, 1, 1, 1, 1, 70)}}}
	d := afDeps(sv, &calls)
	stored, _ := json.Marshal([]maskPart{{Part: "tops", URL: "https://cdn.example.com/stored/tops.png"}})
	base := d.loadTask
	d.loadTask = func(ctx context.Context, u int64, id string) (*model.Task, json.RawMessage, error) {
		tk, p, e := base(ctx, u, id)
		if tk != nil {
			tk.MaskURL, tk.MaskParts = sql.NullString{String: "https://cdn.example.com/stored/all.png", Valid: true}, stored
		}
		return tk, p, e
	}
	if _, err := autoColorFix(context.Background(), d, &studio.AutoColorFixReq{UserId: 7, TaskId: "tyo1"}); err != nil {
		t.Fatal(err)
	}
	if sv.got[0].Masks[0].URL != "https://cdn.example.com/stored/tops.png" {
		t.Fatalf("应取已存的上衣蒙版: %+v", sv.got[0].Masks)
	}
	if calls.Load() != 1 { // 只有下装（没存）走了区域识别
		t.Fatalf("区域识别次数 %d", calls.Load())
	}
}

// 私有成品：交给求解端的是现签的干净地址，不是任何客户端传的地址。
func TestAutoColorFixPrivateSourceUsesSignedOriginal(t *testing.T) {
	var calls atomic.Int32
	sv := &fakeSolver{enabled: true, resp: &colorfixsolver.Response{Pieces: []colorfixsolver.Piece{solved("top", 1, 1, 1, 1, 1, 70)}}}
	d := afDeps(sv, &calls)
	d.source = func(context.Context, int64, string) (string, string, string, bool, error) {
		return "rid-1", "https://priv.example.com/v1/work/x.png?Signature=s", "", true, nil
	}
	var gotRID string
	d.pieceMask = func(_ context.Context, _ int64, rid, src, _ string, _ []string) (string, error) {
		gotRID = rid
		return "https://cdn.example.com/m.png", nil
	}
	if _, err := autoColorFix(context.Background(), d, &studio.AutoColorFixReq{UserId: 7, TaskId: "tyo1", Pieces: []string{"top"}}); err != nil {
		t.Fatal(err)
	}
	if sv.got[0].ImageURL != "https://priv.example.com/v1/work/x.png?Signature=s" || gotRID != "rid-1" {
		t.Fatalf("image=%s rid=%s", sv.got[0].ImageURL, gotRID)
	}
	// 没有成品
	d.source = func(context.Context, int64, string) (string, string, string, bool, error) {
		return "", "", "", false, nil
	}
	_, err := autoColorFix(context.Background(), d, &studio.AutoColorFixReq{UserId: 7, TaskId: "tyo1"})
	wantReason(t, err, xerr.NotFound, "RESOURCE_NOT_FOUND")
}

func TestAutoColorFixSolverErrorClassification(t *testing.T) {
	cases := []struct {
		err    error
		code   int
		reason string
	}{
		{&colorfixsolver.Error{Kind: colorfixsolver.KindBusy, Status: 429}, xerr.ThirdPartyError, xerr.ReasonColorFixAutoBusy},
		{&colorfixsolver.Error{Kind: colorfixsolver.KindTimeout, Status: 504}, xerr.ThirdPartyError, xerr.ReasonColorFixAutoTimeout},
		{&colorfixsolver.Error{Kind: colorfixsolver.KindUnusable, Status: 422}, xerr.NotFound, xerr.ReasonColorFixAutoNoPiece},
		{&colorfixsolver.Error{Kind: colorfixsolver.KindUnavailable, Status: 502}, xerr.ThirdPartyError, xerr.ReasonColorFixAutoUnavailable},
		{&colorfixsolver.Error{Kind: colorfixsolver.KindUnavailable, Status: 401}, xerr.ThirdPartyError, xerr.ReasonColorFixAutoUnavailable},
		{errors.New("boom"), xerr.ThirdPartyError, xerr.ReasonColorFixAutoUnavailable},
	}
	var calls atomic.Int32
	for _, c := range cases {
		_, err := autoColorFix(context.Background(), afDeps(&fakeSolver{enabled: true, err: c.err}, &calls), &studio.AutoColorFixReq{UserId: 7, TaskId: "tyo1"})
		wantReason(t, err, c.code, c.reason)
		if _, msg, _ := xerr.Parse(err); strings.Contains(msg, "boom") || strings.Contains(msg, "429") {
			t.Fatalf("求解端原文不能给用户: %s", msg)
		}
	}
}

// 真实客户端 + httptest 假求解服务：端到端走一遍线上协议（Bearer、请求体、429 / 504 映射）。
func TestAutoColorFixWithRealClientAgainstFakeSolver(t *testing.T) {
	status := http.StatusOK
	var auth string
	var body colorfixsolver.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(status)
		if status != http.StatusOK {
			_, _ = w.Write([]byte(`{"error":{"code":"x","message":"y"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"pieces":[{"key":"top","params":{"hue":-9,"saturation":2,"value":0,"brightness":-7,"contrast":3},"score":97.1}],"version":"v"}`))
	}))
	t.Cleanup(srv.Close)
	var calls atomic.Int32
	d := afDeps(&fakeSolver{}, &calls)
	d.solver = colorfixsolver.New(colorfixsolver.Conf{Endpoint: srv.URL, Token: "tok-tok-tok-tok-tok"})
	resp, err := autoColorFix(context.Background(), d, &studio.AutoColorFixReq{UserId: 7, TaskId: "tyo1", Pieces: []string{"top"}})
	if err != nil || len(resp.Pieces) != 1 || resp.Pieces[0].Params.Brightness != -7 || resp.Pieces[0].Score != 97.1 {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if auth != "Bearer tok-tok-tok-tok-tok" || body.ImageURL != afPublic || body.Masks[0].Key != "top" {
		t.Fatalf("auth=%q body=%+v", auth, body)
	}
	status = http.StatusTooManyRequests
	_, err = autoColorFix(context.Background(), d, &studio.AutoColorFixReq{UserId: 7, TaskId: "tyo1"})
	wantReason(t, err, xerr.ThirdPartyError, xerr.ReasonColorFixAutoBusy)
	status = http.StatusGatewayTimeout
	_, err = autoColorFix(context.Background(), d, &studio.AutoColorFixReq{UserId: 7, TaskId: "tyo1"})
	wantReason(t, err, xerr.ThirdPartyError, xerr.ReasonColorFixAutoTimeout)
}
