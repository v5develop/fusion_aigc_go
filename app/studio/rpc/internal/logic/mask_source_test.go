package logic

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/xerr"

	"github.com/DATA-DOG/go-sqlmock"
)

// ---------------------------------------------------------------- fakes（不调真实媒体服务）

// fakeMaskSigner 按 rid 现签私有桶地址：路径固定（对象键由 rid 派生），签名参数每次不同。
type fakeMaskSigner struct {
	host  string
	paths map[string]string // rid → 桶内路径
	n     int
	asked []string
}

func (s *fakeMaskSigner) ResourceURLs(_ context.Context, rids []string, _ time.Duration) (map[string]string, error) {
	out := map[string]string{}
	for _, rid := range rids {
		s.asked = append(s.asked, rid)
		p, ok := s.paths[rid]
		if !ok {
			return nil, fmt.Errorf("rid 不存在: %s", rid)
		}
		s.n++
		out[rid] = fmt.Sprintf("https://%s/%s?Expires=%d&OSSAccessKeyId=k&Signature=srv%d", s.host, p, 1700000000+s.n, s.n)
	}
	return out, nil
}

// 各用例用不同的 rid / 路径：signedIndex 是包级的，避免用例间互相命中。
func privRID(tag string) string { return "abcdef-work-" + strings.Repeat(tag, 26)[:26] }

func privKey(tag string) string { return strings.Repeat(tag, 26)[:26] }

const privHost = "priv-bucket.oss-cn-hangzhou.aliyuncs.com"

// userURL 前端拿到的签名地址（读口签出，签名参数与服务端现签的不同）。
func userURL(host, objKey, sig string) string {
	return "https://" + host + "/v1/work/2026/11/" + objKey + ".png?Expires=1&OSSAccessKeyId=k&Signature=" + sig
}

const listPrivSQL = "SELECT `result_rid` FROM `afv_studio_task` WHERE \\(user_id = %d\\) AND \\(deleted = 0\\) AND " +
	"\\(\\(result_url LIKE 'mr://%%' OR result_url LIKE 'mrw://%%'\\)\\) AND \\(result_rid LIKE '%%-work-%%'\\)"

// resetSignedIndex 清空签名地址 → rid 反查表（包级）。
func resetSignedIndex() {
	signedIndex.Lock()
	signedIndex.m = map[string]indexEntry{}
	signedIndex.Unlock()
}

func newPrivMasker(t *testing.T, sg *fakeMaskSigner) (*masker, sqlmock.Sqlmock, *fakeMaskGW) {
	// 反查表是包级的：每个用例从空表开始（-count>1 / 换顺序时上一轮签过的地址不能让「别的实例签的」用例走成本实例命中）
	resetSignedIndex()
	t.Cleanup(resetSignedIndex)
	r, mock := newMockRepo(t)
	gw := &fakeMaskGW{resp: segCall("0.01")}
	m := &masker{repo: r.Mask, gw: gw, media: &fakeMaskMedia{cutout: cutoutPNG(t, 8, 8, 255)}, now: time.Now,
		sign: sg, owner: r.Task}
	return m, mock, gw
}

// maskCallImage 网关调用里交出去的图片地址。
func maskCallImage(c aigateway.CallRequest) string {
	in, _ := c.Input.(map[string]any)
	ref, _ := in["image"].(aigateway.MediaRef)
	return ref.URI
}

// maskSelectKey 指定缓存键的查询。
func maskSelectKey(key, target string) string {
	return "SELECT .* FROM `afv_studio_mask_cache` AS `mc` WHERE \\(mc.url_key = '" + key + "'\\) AND \\(mc.target = '" + target +
		"'\\) AND \\(mc.parts = ''\\) LIMIT 1"
}

// ---------------------------------------------------------------- 纯函数

func TestParseMaskPrivatePath(t *testing.T) {
	k := privKey("p")
	ok := []string{
		"https://" + privHost + "/v1/work/2026/11/" + k + ".png?Signature=x",
		"https://" + privHost + "/v1/user/2026/01/" + k + "?Signature=x",
		"https://minio.example.com/bucket/v1/work/2026/11/" + k + ".jpg", // 路径式桶地址
	}
	for _, u := range ok {
		if _, _, _, good := parseMaskPrivatePath(u); !good {
			t.Errorf("%q 应认", u)
		}
	}
	key, class, month, _ := parseMaskPrivatePath(ok[0])
	if key != privHost+"/v1/work/2026/11/"+k+".png" || class != "work" || !month.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, maskShanghai)) {
		t.Fatalf("key=%q class=%q month=%v", key, class, month)
	}
	bad := []string{
		"https://" + privHost + "/v1/asset/2026/11/" + k + ".png",        // 素材库桶走白名单
		"https://" + privHost + "/v1/deliver/2026/11/" + k + ".png",      // 第三方桶
		"https://" + privHost + "/v1/work/2026/13/" + k + ".png",         // 月份不合法
		"https://" + privHost + "/v1/work/2026/11/" + k[:20] + ".png",    // 对象键长度不对
		"https://" + privHost + "/v1/work/2026/11/" + k + ".png/x",       // 不以对象结尾
		"https://" + privHost + "/v1/work/2026/11/../" + k + ".png",      // .. 段
		"https://u@" + privHost + "/v1/work/2026/11/" + k + ".png",       // userinfo
		"ftp://" + privHost + "/v1/work/2026/11/" + k + ".png",           // 协议
		"mr://" + privRID("p") + ".png",                                  // 引用本身不收（前端拿不到）
		"https://" + privHost + "/v1/work/2026/11/" + k + ".png#frag",    // 带锚点
		"https://" + privHost + "/v1/work/2026/11/" + strings.ToUpper(k), // 对象键只有小写 base32
	}
	for _, u := range bad {
		if _, _, _, good := parseMaskPrivatePath(u); good {
			t.Errorf("%q 不应认", u)
		}
	}
}

// ---------------------------------------------------------------- 归属反查

// 自己成品的私有签名地址（别的实例签的，反查表没有）：按路径年月取候选逐个现签比对命中 → 放行；
// 网关拿的是服务端现签的干净地址（不是用户给的），缓存键 / 幂等键按 mr://{rid}。
func TestSegmentMaskOwnPrivateURLResolved(t *testing.T) {
	own, other := privRID("a"), privRID("b")
	sg := &fakeMaskSigner{host: privHost, paths: map[string]string{
		own:   "v1/work/2026/11/" + privKey("a") + ".png",
		other: "v1/work/2026/11/" + privKey("b") + ".png",
	}}
	m, mock, gw := newPrivMasker(t, sg)
	refKey := maskRefKey("mr://" + own)

	mock.ExpectQuery(fmt.Sprintf(listPrivSQL, 7) + ".*ORDER BY id DESC LIMIT 100").
		WillReturnRows(sqlmock.NewRows([]string{"result_rid"}).AddRow(other).AddRow(own))
	mock.ExpectQuery(maskSelectKey(refKey, "garment")).WillReturnRows(sqlmock.NewRows(maskCols))
	mock.ExpectExec("INSERT INTO `afv_studio_mask_cache` .*'" + regexp.QuoteMeta("mr://"+own) + "'").
		WillReturnResult(sqlmock.NewResult(1, 1))

	in := &studio.SegmentMaskReq{UserId: 7, ImageUrl: userURL(privHost, privKey("a"), "user1"), Target: "garment"}
	resp, err := segmentMask(context.Background(), m, func(string) bool { return false }, in)
	if err != nil || resp.MaskUrl != "https://cdn.example.com/mask.png" || resp.Cached {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if len(gw.calls) != 1 {
		t.Fatalf("网关调用 %d 次", len(gw.calls))
	}
	src := maskCallImage(gw.calls[0])
	if strings.Contains(src, "user1") || !strings.HasPrefix(src, "https://"+privHost+"/v1/work/2026/11/"+privKey("a")+".png?") {
		t.Fatalf("网关应拿服务端现签的干净地址: %s", src)
	}
	if gw.keys[0] != "studio-mask:garment:"+refKey {
		t.Fatalf("幂等键应按 mr://{rid}: %s", gw.keys[0])
	}
	met(t, mock)
}

// 同一 rid 换了签名参数：第二次走本实例反查表 + 归属校验，缓存键不变 → 命中缓存，不再调网关。
func TestSegmentMaskPrivateCacheKeyStableAcrossSignatures(t *testing.T) {
	own := privRID("c")
	sg := &fakeMaskSigner{host: privHost, paths: map[string]string{own: "v1/work/2026/11/" + privKey("c") + ".png"}}
	m, mock, gw := newPrivMasker(t, sg)
	refKey := maskRefKey("mr://" + own)

	mock.ExpectQuery(fmt.Sprintf(listPrivSQL, 7)).WillReturnRows(sqlmock.NewRows([]string{"result_rid"}).AddRow(own))
	mock.ExpectQuery(maskSelectKey(refKey, "garment")).WillReturnRows(sqlmock.NewRows(maskCols))
	mock.ExpectExec("INSERT INTO `afv_studio_mask_cache`").WillReturnResult(sqlmock.NewResult(1, 1))
	in := &studio.SegmentMaskReq{UserId: 7, ImageUrl: userURL(privHost, privKey("c"), "sigA"), Target: "garment"}
	if _, err := segmentMask(context.Background(), m, nil, in); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta("FROM `afv_studio_task` WHERE (user_id = 7) AND (result_rid = '" + own + "') AND (deleted = 0)")).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(maskSelectKey(refKey, "garment")).WillReturnRows(sqlmock.NewRows(maskCols).
		AddRow(1, refKey, "garment", "", "mr://"+own, model.MaskOK, "aaaaaa-asset-mask", "https://cdn.example.com/mask.png", nil,
			"0.5", 8, 8, "imgseg.aliyun-cloth", "call_1", "0.01", 7, 0, now, now))
	mock.ExpectExec("UPDATE `afv_studio_mask_cache` SET hits").WillReturnResult(sqlmock.NewResult(0, 1))
	in.ImageUrl = userURL(privHost, privKey("c"), "sigB")
	resp, err := segmentMask(context.Background(), m, nil, in)
	if err != nil || !resp.Cached || resp.MaskUrl != "https://cdn.example.com/mask.png" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if len(gw.calls) != 1 {
		t.Fatalf("换签名不应再调网关: %d", len(gw.calls))
	}
	met(t, mock)
}

// 别人的成品：反查表命中但归属不符 → 拒；不在自己的候选里 → 拒。都按来源不合法（1003），不调网关。
func TestSegmentMaskOtherUsersPrivateURLRejected(t *testing.T) {
	rid := privRID("d")
	sg := &fakeMaskSigner{host: privHost, paths: map[string]string{rid: "v1/work/2026/11/" + privKey("d") + ".png"}}
	m, mock, gw := newPrivMasker(t, sg)
	u := userURL(privHost, privKey("d"), "s1")

	// 反查表里有（本实例给用户 8 签过），用户 9 拿来用
	indexSigned(rid, u, 10*time.Minute, time.Now())
	mock.ExpectQuery(regexp.QuoteMeta("FROM `afv_studio_task` WHERE (user_id = 9) AND (result_rid = '" + rid + "')")).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	_, err := segmentMask(context.Background(), m, nil, &studio.SegmentMaskReq{UserId: 9, ImageUrl: u, Target: "garment"})
	if code, _, _ := xerr.Parse(err); code != xerr.ParamError {
		t.Fatalf("别人的成品应拒: %v", err)
	}

	// 反查表没有（别的实例签的）：用户 9 自己的候选里没有这个对象
	other := privRID("e")
	sg.paths[other] = "v1/work/2026/11/" + privKey("e") + ".png"
	u2 := userURL(privHost, privKey("f"), "s2")
	mock.ExpectQuery(fmt.Sprintf(listPrivSQL, 9)).WillReturnRows(sqlmock.NewRows([]string{"result_rid"}).AddRow(other))
	_, err = segmentMask(context.Background(), m, nil, &studio.SegmentMaskReq{UserId: 9, ImageUrl: u2, Target: "garment"})
	if code, _, _ := xerr.Parse(err); code != xerr.ParamError {
		t.Fatalf("不在自己候选里应拒: %v", err)
	}
	if len(gw.calls) != 0 {
		t.Fatal("拒收不应调网关")
	}
	met(t, mock)
}

// 未知对象：用户没有任何私有成品 → 拒，不现签。
func TestSegmentMaskUnknownPrivateURLRejected(t *testing.T) {
	sg := &fakeMaskSigner{host: privHost, paths: map[string]string{}}
	m, mock, _ := newPrivMasker(t, sg)
	mock.ExpectQuery(fmt.Sprintf(listPrivSQL, 7)).WillReturnRows(sqlmock.NewRows([]string{"result_rid"}))
	_, err := segmentMask(context.Background(), m, nil,
		&studio.SegmentMaskReq{UserId: 7, ImageUrl: userURL(privHost, privKey("g"), "s"), Target: "product"})
	if code, _, _ := xerr.Parse(err); code != xerr.ParamError {
		t.Fatalf("未知对象应拒: %v", err)
	}
	if len(sg.asked) != 0 {
		t.Fatalf("没有候选不应现签: %v", sg.asked)
	}
	met(t, mock)
}

// 外部域名照抄本平台私有桶的路径（甚至是自己成品的对象键）：host 不等 → 拒；反查表按 host + path 也不会命中。
func TestSegmentMaskForeignHostSimilarPathRejected(t *testing.T) {
	own := privRID("h")
	sg := &fakeMaskSigner{host: privHost, paths: map[string]string{own: "v1/work/2026/11/" + privKey("h") + ".png"}}
	m, mock, gw := newPrivMasker(t, sg)
	indexSigned(own, userURL(privHost, privKey("h"), "real"), 10*time.Minute, time.Now())
	for _, host := range []string{"evil.example.com", privHost + ".evil.io"} {
		mock.ExpectQuery(fmt.Sprintf(listPrivSQL, 7)).WillReturnRows(sqlmock.NewRows([]string{"result_rid"}).AddRow(own))
		_, err := segmentMask(context.Background(), m, nil,
			&studio.SegmentMaskReq{UserId: 7, ImageUrl: userURL(host, privKey("h"), "x"), Target: "garment"})
		if code, _, _ := xerr.Parse(err); code != xerr.ParamError {
			t.Fatalf("%s 应拒: %v", host, err)
		}
	}
	if len(gw.calls) != 0 {
		t.Fatal("拒收不应调网关")
	}
	met(t, mock)
}

// 白名单地址行为不变：不做归属反查（不查库、不现签），缓存键仍按规范化地址。
func TestSegmentMaskWhitelistUnchanged(t *testing.T) {
	sg := &fakeMaskSigner{host: privHost, paths: map[string]string{}}
	m, mock, gw := newPrivMasker(t, sg)
	_, wantKey, _ := normalizeMaskURL("https://cdn.example.com/a.png")
	mock.ExpectQuery(maskSelectKey(wantKey, "garment")).WillReturnRows(sqlmock.NewRows(maskCols))
	mock.ExpectExec("INSERT INTO `afv_studio_mask_cache`").WillReturnResult(sqlmock.NewResult(1, 1))
	owns := func(u string) bool { return strings.HasPrefix(u, "https://cdn.example.com/") }
	_, err := segmentMask(context.Background(), m, owns,
		&studio.SegmentMaskReq{UserId: 7, ImageUrl: "https://cdn.example.com/a.png?Signature=1", Target: "garment"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sg.asked) != 0 || maskCallImage(gw.calls[0]) != "https://cdn.example.com/a.png?Signature=1" {
		t.Fatalf("白名单地址应原样交网关、不现签: asked=%v", sg.asked)
	}
	// 既不在白名单、也不像私有桶对象：直接拒，不查库
	_, err = segmentMask(context.Background(), m, owns,
		&studio.SegmentMaskReq{UserId: 7, ImageUrl: "https://evil.example.org/a.png", Target: "garment"})
	if code, _, _ := xerr.Parse(err); code != xerr.ParamError {
		t.Fatalf("外部地址应拒: %v", err)
	}
	met(t, mock)
}

// 成品自动蒙版：私有引用按 mr://{rid} 做缓存键（与接口拿签名地址来识别时同一份缓存）。
func TestAttachMaskAsyncPrivateRefKey(t *testing.T) {
	r, mock := newMockRepo(t)
	gw := &fakeMaskGW{resp: segCall("0.01")}
	m := &masker{repo: r.Mask, gw: gw, media: &fakeMaskMedia{cutout: cutoutPNG(t, 8, 8, 255)}, now: time.Now}
	f := &flow{repo: r, mask: m, media: &copyMedia{}}
	refKey := maskRefKey("mr://" + wmRID)
	mock.ExpectQuery(maskSelectKey(refKey, "garment")).WillReturnRows(sqlmock.NewRows(maskCols))
	mock.ExpectExec("INSERT INTO `afv_studio_mask_cache`").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET mask_url = 'https://cdn.example.com/mask.png' WHERE (id = 5)")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	f.attachMaskAsync(&model.Task{ID: 5, Kind: model.KindTryon}, 7, "mr://"+wmRID+".png", nil)
	f.wg.Wait()
	if len(gw.keys) != 1 || gw.keys[0] != "studio-mask:garment:"+refKey {
		t.Fatalf("keys=%v", gw.keys)
	}
	met(t, mock)
}

// ---------------------------------------------------------------- 精确口径（task_id / source_rid）

func ownedResultSQL(taskID string, userID int64) string {
	return regexp.QuoteMeta(fmt.Sprintf("SELECT `result_url` FROM `afv_studio_task` WHERE (task_id = '%s') AND (user_id = %d) AND (deleted = 0) LIMIT 1", taskID, userID))
}

// task_id 指向自己的私有成品（mrw:// 免费档）：直接按 rid 现签，不扫候选；带的 image_url（水印签名地址）与成品同一对象 → 放行，
// 缓存键按 mr://{rid}。
func TestSegmentMaskByTaskIDPrivate(t *testing.T) {
	rid := privRID("i")
	sg := &fakeMaskSigner{host: privHost, paths: map[string]string{rid: "v1/work/2026/11/" + privKey("i") + ".png"}}
	m, mock, gw := newPrivMasker(t, sg)
	refKey := maskRefKey("mr://" + rid)
	mock.ExpectQuery(ownedResultSQL("simg_9", 7)).WillReturnRows(sqlmock.NewRows([]string{"result_url"}).AddRow("mrw://" + rid + ".png"))
	mock.ExpectQuery(maskSelectKey(refKey, "garment")).WillReturnRows(sqlmock.NewRows(maskCols))
	mock.ExpectExec("INSERT INTO `afv_studio_mask_cache`").WillReturnResult(sqlmock.NewResult(1, 1))
	in := &studio.SegmentMaskReq{UserId: 7, TaskId: "simg_9", ImageUrl: userURL(privHost, privKey("i"), "wm&x-oss-process=image/watermark"), Target: "garment"}
	if _, err := segmentMask(context.Background(), m, func(string) bool { return false }, in); err != nil {
		t.Fatal(err)
	}
	if len(gw.keys) != 1 || gw.keys[0] != "studio-mask:garment:"+refKey || strings.Contains(maskCallImage(gw.calls[0]), "watermark") {
		t.Fatalf("应按 rid 现签干净地址、缓存键 mr://{rid}: keys=%v", gw.keys)
	}
	met(t, mock)
}

// task_id 不是自己的 / 已删除 → 拒（1003）；image_url 与任务成品不是同一对象 → 拒；都不调网关。
func TestSegmentMaskByTaskIDRejects(t *testing.T) {
	rid := privRID("j")
	sg := &fakeMaskSigner{host: privHost, paths: map[string]string{rid: "v1/work/2026/11/" + privKey("j") + ".png"}}
	m, mock, gw := newPrivMasker(t, sg)
	mock.ExpectQuery(ownedResultSQL("simg_x", 9)).WillReturnRows(sqlmock.NewRows([]string{"result_url"}))
	_, err := segmentMask(context.Background(), m, nil, &studio.SegmentMaskReq{UserId: 9, TaskId: "simg_x", Target: "garment"})
	if code, _, _ := xerr.Parse(err); code != xerr.ParamError {
		t.Fatalf("别人的任务应拒: %v", err)
	}
	mock.ExpectQuery(ownedResultSQL("simg_y", 7)).WillReturnRows(sqlmock.NewRows([]string{"result_url"}).AddRow("mr://" + rid + ".png"))
	_, err = segmentMask(context.Background(), m, nil,
		&studio.SegmentMaskReq{UserId: 7, TaskId: "simg_y", ImageUrl: userURL(privHost, privKey("k"), "s"), Target: "garment"})
	if code, _, _ := xerr.Parse(err); code != xerr.ParamError {
		t.Fatalf("地址与任务成品不一致应拒: %v", err)
	}
	// 公开成品：地址（去查询串）须一致
	mock.ExpectQuery(ownedResultSQL("simg_z", 7)).WillReturnRows(sqlmock.NewRows([]string{"result_url"}).AddRow("https://cdn.example.com/z.png"))
	_, err = segmentMask(context.Background(), m, nil,
		&studio.SegmentMaskReq{UserId: 7, TaskId: "simg_z", ImageUrl: "https://cdn.example.com/other.png", Target: "garment"})
	if code, _, _ := xerr.Parse(err); code != xerr.ParamError {
		t.Fatalf("公开地址不一致应拒: %v", err)
	}
	if _, err := segmentMask(context.Background(), m, nil,
		&studio.SegmentMaskReq{UserId: 7, TaskId: "a", SourceRid: rid, Target: "garment"}); err == nil {
		t.Fatal("task_id 与 source_rid 同时给应拒")
	}
	if len(gw.calls) != 0 {
		t.Fatal("拒收不应调网关")
	}
	met(t, mock)
}

// task_id 指向公开成品、不带 image_url：按落库的长期地址识别（不查白名单、不现签）。
func TestSegmentMaskByTaskIDPublic(t *testing.T) {
	sg := &fakeMaskSigner{host: privHost, paths: map[string]string{}}
	m, mock, gw := newPrivMasker(t, sg)
	_, key, _ := normalizeMaskURL("https://cdn.example.com/p.png")
	mock.ExpectQuery(ownedResultSQL("simg_p", 7)).WillReturnRows(sqlmock.NewRows([]string{"result_url"}).AddRow("https://cdn.example.com/p.png"))
	mock.ExpectQuery(maskSelectKey(key, "person")).WillReturnRows(sqlmock.NewRows(maskCols))
	mock.ExpectExec("INSERT INTO `afv_studio_mask_cache`").WillReturnResult(sqlmock.NewResult(1, 1))
	if _, err := segmentMask(context.Background(), m, nil, &studio.SegmentMaskReq{UserId: 7, TaskId: "simg_p", Target: "person"}); err != nil {
		t.Fatal(err)
	}
	if len(sg.asked) != 0 || maskCallImage(gw.calls[0]) != "https://cdn.example.com/p.png" {
		t.Fatalf("公开成品应原样交网关: asked=%v", sg.asked)
	}
	met(t, mock)
}

// source_rid（画布候选，网关已确认归属）：直接现签，缓存键 mr://{rid}；不查 studio 表。
func TestSegmentMaskBySourceRID(t *testing.T) {
	rid := privRID("l")
	sg := &fakeMaskSigner{host: privHost, paths: map[string]string{rid: "v1/work/2026/11/" + privKey("l") + ".png"}}
	m, mock, gw := newPrivMasker(t, sg)
	refKey := maskRefKey("mr://" + rid)
	mock.ExpectQuery(maskSelectKey(refKey, "garment")).WillReturnRows(sqlmock.NewRows(maskCols))
	mock.ExpectExec("INSERT INTO `afv_studio_mask_cache`").WillReturnResult(sqlmock.NewResult(1, 1))
	if _, err := segmentMask(context.Background(), m, nil, &studio.SegmentMaskReq{UserId: 7, SourceRid: rid, Target: "garment"}); err != nil {
		t.Fatal(err)
	}
	if len(gw.keys) != 1 || gw.keys[0] != "studio-mask:garment:"+refKey {
		t.Fatalf("keys=%v", gw.keys)
	}
	if _, err := segmentMask(context.Background(), m, nil, &studio.SegmentMaskReq{UserId: 7, SourceRid: "bad", Target: "garment"}); err == nil {
		t.Fatal("不合法 rid 应拒")
	}
	met(t, mock)
}
