package media

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

const testOpsToken = "ops-token-0123456789abcdef"

func postLocate(t *testing.T, base, token, rid string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"rid": rid})
	req, err := http.NewRequest(http.MethodPost, base+"/v1/mediasvc/ops/locate", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// 运维定位：凭运维令牌返回类别、存储位置、对象键与路径，与 rid 推导的结果一致。
func TestOpsLocateReturnsPathForRID(t *testing.T) {
	ts := newTestServer(t, Config{AllowPrivateHosts: true, ObjectDir: t.TempDir(), OpsToken: testOpsToken})
	rid := "tst001-work-abcdefghijklmnopqrstuvwxyz"
	resp := postLocate(t, ts.URL, testOpsToken, rid)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body = %s", resp.StatusCode, b)
	}
	var out locateResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Class != classWork || out.Store != storePrivate || out.Path != pathOfRID(t, rid) {
		t.Fatalf("定位结果 = %+v", out)
	}
	if out.ObjectKey == "" || !strings.HasPrefix(out.Path, "v1/work/") {
		t.Fatalf("对象键或路径不对: %+v", out)
	}
}

// 没有配置运维令牌：接口不注册，返回 404（不做无鉴权的开放查询）。
func TestOpsLocateIsAbsentWithoutOpsToken(t *testing.T) {
	ts := newTestServer(t, Config{AllowPrivateHosts: true, ObjectDir: t.TempDir()})
	resp := postLocate(t, ts.URL, testOpsToken, "tst001-work-abcdefghijklmnopqrstuvwxyz")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("未配置运维令牌时应返回 404，got %d", resp.StatusCode)
	}
}

// 业务令牌不能用于运维接口；运维令牌也不能用于业务接口。
func TestOpsAndBusinessTokensAreSeparate(t *testing.T) {
	ts := newTestServer(t, Config{AllowPrivateHosts: true, ObjectDir: t.TempDir(), OpsToken: testOpsToken})
	rid := "tst001-work-abcdefghijklmnopqrstuvwxyz"
	assertAPIError(t, postLocate(t, ts.URL, testToken, rid), http.StatusUnauthorized, "unauthorized")

	body, _ := json.Marshal(map[string]string{"rid": rid})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/mediasvc/resources/urls", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testOpsToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("运维令牌不能用于业务接口，got %d", resp.StatusCode)
	}
}

// 业务响应不含对象键：入库结果里没有 key 与 object_key 字段。
func TestBusinessResponsesDoNotExposeObjectKeys(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeAVClip(t, filepath.Join(f.dir, "k.mp4"))
	resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/ingest", map[string]any{
		"source": f.url("k.mp4"), "purpose": "intermediate"}, testToken, "k-nokey")
	b, _ := io.ReadAll(resp.Body)
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	obj, _ := raw["object"].(map[string]any)
	if _, ok := obj["key"]; ok {
		t.Fatalf("业务响应不应含对象键: %s", b)
	}
	if !strings.Contains(string(b), `"rid"`) {
		t.Fatalf("业务响应应含 rid: %s", b)
	}
}
