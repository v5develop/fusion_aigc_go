package studio

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/app/studio/rpc/studio_client"
	"aigc-local-life/pkg/jsoncase"
)

// 线上字段名（与前端约定死）：请求 modelViews / garmentImageUrls（camelCase），经 jsoncase 补成 snake 后能解进 types；
// 响应里 model_views / garment_image_urls 自动带 modelViews / garmentImageUrls 别名。
func TestWireNamesRequest(t *testing.T) {
	var tryon types.StudioTryonSubmitReq
	var pose types.StudioPoseSubmitReq
	var quote types.StudioPoseQuoteReq
	h := jsoncase.Middleware(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var err error
		switch r.URL.Path {
		case "/tryon":
			err = json.Unmarshal(body, &tryon)
		case "/pose":
			err = json.Unmarshal(body, &pose)
		case "/quote":
			err = json.Unmarshal(body, &quote)
		}
		if err != nil {
			t.Errorf("%s: %v", r.URL.Path, err)
		}
	})
	post := func(path, body string) {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		h(httptest.NewRecorder(), req)
	}
	post("/tryon", `{"modelImageUrls":["https://a/1.jpg","https://a/2.jpg"],"modelViews":["front","back"],"items":["top"],"countPerModel":1}`)
	if got := strings.Join(tryon.ModelViews, ","); got != "front,back" || len(tryon.ModelImageUrls) != 2 {
		t.Fatalf("modelViews 没解进来：%+v", tryon)
	}
	post("/pose", `{"sourceImageUrl":"https://a/s.jpg","poseImageUrls":["https://a/p.jpg"],"garmentImageUrls":["https://a/g1.jpg","https://a/g2.jpg"]}`)
	if len(pose.GarmentImageUrls) != 2 || pose.GarmentImageUrls[1] != "https://a/g2.jpg" {
		t.Fatalf("garmentImageUrls 没解进来：%+v", pose)
	}
	post("/quote", `{"poseImageUrls":["https://a/p.jpg"],"garmentImageUrls":["https://a/g1.jpg"]}`)
	if len(quote.GarmentImageUrls) != 1 {
		t.Fatalf("报价 garmentImageUrls 没解进来：%+v", quote)
	}
	// 老写法（snake）与不传都照旧
	tryon = types.StudioTryonSubmitReq{}
	post("/tryon", `{"model_image_urls":["https://a/1.jpg"],"model_views":["side"]}`)
	if len(tryon.ModelViews) != 1 || tryon.ModelViews[0] != "side" {
		t.Fatalf("snake 写法：%+v", tryon)
	}
	tryon = types.StudioTryonSubmitReq{}
	post("/tryon", `{"model_image_urls":["https://a/1.jpg"]}`)
	if tryon.ModelViews != nil {
		t.Fatalf("不传应为 nil：%+v", tryon)
	}
}

// 历史 / 详情：批次 payload 里的 model_views / garment_image_urls 回成顶层字段，响应带 camelCase 别名；老批次回 []。
func TestWireNamesBatchResponse(t *testing.T) {
	tryonPayload := `{"model_image_urls":["a","b"],"model_views":["back","front"],"items":["top"]}`
	posePayload := `{"source_image_url":"s","garment_image_urls":["g1","g2"]}`
	h := jsoncase.Middleware(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		out := []types.StudioBatch{
			toBatch(&studio_client.BatchInfo{Id: 1, Kind: "tryon", Payload: tryonPayload}),
			toBatch(&studio_client.BatchInfo{Id: 2, Kind: "pose", Payload: posePayload}),
			toBatch(&studio_client.BatchInfo{Id: 3, Kind: "tryon", Payload: `{"model_image_urls":["a"]}`}),
			toBatch(&studio_client.BatchInfo{Id: 4, Kind: "imagegen", Payload: ``}),
			toBatch(&studio_client.BatchInfo{Id: 5, Kind: "tryon", Payload: `not json`}),
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"items": out}})
	})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/history", nil))
	var resp struct {
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	strs := func(v any) []string {
		arr, ok := v.([]any)
		if !ok {
			t.Fatalf("不是数组：%v（%T）", v, v)
		}
		out := make([]string, len(arr))
		for i, x := range arr {
			out[i], _ = x.(string)
		}
		return out
	}
	it := resp.Data.Items
	if got := strings.Join(strs(it[0]["modelViews"]), ","); got != "back,front" {
		t.Fatalf("history modelViews = %v", it[0]["modelViews"])
	}
	if got := strings.Join(strs(it[0]["model_views"]), ","); got != "back,front" {
		t.Fatalf("snake 原键应保留：%v", it[0]["model_views"])
	}
	if got := strings.Join(strs(it[1]["garmentImageUrls"]), ","); got != "g1,g2" {
		t.Fatalf("history garmentImageUrls = %v", it[1]["garmentImageUrls"])
	}
	for _, i := range []int{2, 3, 4} { // 没传过 / 空 / 坏 payload：[]，不是 null
		if len(strs(it[i]["modelViews"])) != 0 || len(strs(it[i]["garmentImageUrls"])) != 0 {
			t.Fatalf("第 %d 条应为空数组：%v", i, it[i])
		}
	}
	// payload 原样保留（前端老的回填方式不受影响）
	if it[0]["payload"] != tryonPayload {
		t.Fatalf("payload 被改了：%v", it[0]["payload"])
	}
}
