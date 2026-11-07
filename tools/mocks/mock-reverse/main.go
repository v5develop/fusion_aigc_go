// mock-reverse 模拟 prompt-reverse sidecar（tools/prompt-reverse/serve_reverse.py）的 7 条路由，仅开发/测试用。
//
//	GET  /api/status
//	POST /api/reverse/crop-plan | crop-check | analyze | analyze-batch
//	POST /api/pose/source | prompt
//
// 行为可控：
//
//	MOCK_REVERSE_FAIL=analyze,crop-plan   这些路由一律返回 502（名字取路径最后一段：crop-plan / crop-check / analyze / analyze-batch / source / prompt）
//	label 含 "bad" 的裁图在 crop-check 里判 ok=false confidence=0.9，其余全部 ok=true
//	analyze / analyze-batch 把请求里的 fabric 原样回显进 prompt（e2e 据此断言）
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"unicode/utf8"
)

const version = "mock-2026-10-30"

func failSet() map[string]bool {
	m := map[string]bool{}
	for _, s := range strings.Split(os.Getenv("MOCK_REVERSE_FAIL"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			m[s] = true
		}
	}
	return m
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// handler 每次请求读取 MOCK_REVERSE_FAIL，便于测试里 Setenv 切换。
func handler() http.Handler {
	mux := http.NewServeMux()
	route := func(method, path string, fn func(w http.ResponseWriter, body map[string]any)) {
		name := path[strings.LastIndex(path, "/")+1:]
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != method {
				writeJSON(w, 404, map[string]any{"error": "not found"})
				return
			}
			if failSet()[name] {
				writeJSON(w, 502, map[string]any{"error": "mock: 上游读图失败 (" + name + ")"})
				return
			}
			body := map[string]any{}
			if method == http.MethodPost {
				r.Body = http.MaxBytesReader(w, r.Body, 24<<20)
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					writeJSON(w, 400, map[string]any{"error": "bad json"})
					return
				}
			}
			fn(w, body)
		})
	}

	route("GET", "/api/status", func(w http.ResponseWriter, _ map[string]any) {
		writeJSON(w, 200, map[string]any{"service": "prompt-reverse", "version": version, "llm_configured": true,
			"model": "deepseek-flash", "model_label": "DeepSeek Flash (mock)", "vision": true, "mock": true})
	})

	route("POST", "/api/reverse/crop-plan", func(w http.ResponseWriter, b map[string]any) {
		imgs, _ := b["images"].([]any)
		if len(imgs) > 6 {
			imgs = imgs[:6]
		}
		items, _ := b["items"].([]any)
		prefix := "上衣"
		if len(items) > 0 {
			prefix = fmt.Sprint(items[0])
		}
		plans := []any{}
		for i, it := range imgs {
			angle := "front"
			if m, ok := it.(map[string]any); ok {
				if a, ok := m["angle"].(string); ok && a != "" {
					angle = a
				}
			}
			plans = append(plans, map[string]any{
				"index": i, "angle": angle,
				"garment_box": []float64{0.1, 0.1, 0.8, 0.8},
				"crops": []any{
					map[string]any{"label": prefix + "·领口", "box": []float64{0.30, 0.10, 0.40, 0.25}, "confidence": 0.9},
					map[string]any{"label": prefix + "·下摆", "box": []float64{0.20, 0.60, 0.50, 0.30}, "confidence": 0.85},
				},
			})
		}
		writeJSON(w, 200, map[string]any{"plans": plans, "meta": map[string]any{"images": len(imgs), "plans": len(plans), "elapsed_ms": 1}})
	})

	route("POST", "/api/reverse/crop-check", func(w http.ResponseWriter, b map[string]any) {
		items, _ := b["items"].([]any)
		if len(items) > 12 {
			items = items[:12]
		}
		res := []any{}
		for i, it := range items {
			label := ""
			if m, ok := it.(map[string]any); ok {
				label, _ = m["label"].(string)
			}
			if strings.Contains(label, "bad") {
				res = append(res, map[string]any{"index": i, "ok": false, "confidence": 0.9})
			} else {
				res = append(res, map[string]any{"index": i, "ok": true, "confidence": 0.95})
			}
		}
		writeJSON(w, 200, map[string]any{"results": res, "meta": map[string]any{"items": len(items), "elapsed_ms": 1}})
	})

	route("POST", "/api/reverse/analyze", func(w http.ResponseWriter, b map[string]any) {
		prompt := analyzePrompt(b, "")
		writeJSON(w, 200, map[string]any{"prompt": prompt, "warning": "",
			"meta": map[string]any{"category": b["category"], "model": "deepseek-flash", "degraded": false, "vision": true, "images": lenOf(b["images"])}})
	})

	route("POST", "/api/reverse/analyze-batch", func(w http.ResponseWriter, b map[string]any) {
		models, _ := b["models"].([]any)
		results := []any{}
		for i, m := range models {
			id := ""
			if mm, ok := m.(map[string]any); ok {
				id = fmt.Sprint(mm["id"])
			}
			results = append(results, map[string]any{"index": i, "model_id": id, "prompt": analyzePrompt(b, id),
				"meta": map[string]any{"degraded": false}, "warning": ""})
		}
		writeJSON(w, 200, map[string]any{"results": results,
			"meta": map[string]any{"models": len(models), "concurrency": 3, "prompt_version": "new", "target_model": b["target_model"]}})
	})

	route("POST", "/api/pose/source", func(w http.ResponseWriter, b map[string]any) {
		if s, _ := b["image"].(string); s == "" {
			writeJSON(w, 422, map[string]any{"error": "缺少 image"})
			return
		}
		writeJSON(w, 200, map[string]any{
			"source": map[string]any{"has_person": true, "gender_age": "年轻女性", "face": "鹅蛋脸", "hair": "黑色长发",
				"outfit": []string{"白色衬衫", "牛仔裤"}, "scene": "影棚", "background": "纯灰色", "light": "柔光", "camera": "平视", "framing": "全身",
				// 2026-11-07.1 多人字段：单人也有 people[1]、person_count=1
				"person_count": 1, "group": "P1 在画面中间", "interaction": "无", "people_truncated": false,
				"people": []map[string]any{{"label": "P1", "position": "画面中间", "primary": true, "gender_age": "年轻女性",
					"face": "鹅蛋脸", "hair": "黑色长发", "outfit": []string{"白色衬衫", "牛仔裤"}, "accessories": []string{}, "pose": "站立", "occluded": ""}}},
			"meta": map[string]any{"model": "deepseek-flash", "elapsed_ms": 1}})
	})

	route("POST", "/api/pose/prompt", func(w http.ResponseWriter, b map[string]any) {
		var sb strings.Builder
		sb.WriteString("【任务】将图1人物的姿势改为图2的姿势，保持人物、服装、场景不变。\n")
		for utf8.RuneCountInString(sb.String()) < 4200 {
			sb.WriteString("【保持】人物面部、发型、服装款式与材质、背景与光线保持与图1一致，不得改动；姿势、朝向与手脚位置以图2为准。\n")
		}
		if n, _ := b["notes"].(string); n != "" {
			sb.WriteString("【补充】" + n + "\n")
		}
		// 多人：姿势图人数按回传 source.people 的人数模拟；mapping 原样回显（合法时），否则默认左→左；单人回 null
		src, _ := b["source"].(map[string]any)
		n := lenOf(src["people"])
		people := []map[string]any{}
		for i := 0; i < max(n, 1); i++ {
			people = append(people, map[string]any{"label": fmt.Sprintf("P%d", i+1), "position": "画面中间", "primary": i == 0,
				"has_person": true, "orientation_deg": "正面0°", "arms": "自然下垂", "outline": "正面站立", "confidence": "高"})
		}
		var mapping any
		if n >= 2 {
			def := make([]int, n)
			for i := range def {
				def[i] = i
			}
			mapping = def
			if m, ok := b["mapping"].([]any); ok && len(m) == n {
				mapping = m
			}
		}
		writeJSON(w, 200, map[string]any{"prompt": sb.String(), "mapping": mapping,
			"pose": map[string]any{"orientation": "正面", "orientation_deg": "正面0°", "arms": "自然下垂", "legs": "并拢", "expression": "平静", "confidence": "高", // 与 sidecar _norm_pose 一致：confidence 是 高/中/低
				"person_count": len(people), "group": "P1 在画面中间", "interaction": "无", "people": people, "people_truncated": false},
			"meta": map[string]any{"model": "deepseek-flash", "elapsed_ms": 1}})
	})
	return mux
}

func lenOf(v any) int { a, _ := v.([]any); return len(a) }

// analyzePrompt 确定性长提示词（>800 字），fabric 原样回显。
func analyzePrompt(b map[string]any, modelID string) string {
	fabric, _ := b["fabric"].(string)
	var sb strings.Builder
	sb.WriteString("【任务】把图2及之后的服装细节穿到图1的模特身上，生成一张商拍图。\n")
	if modelID != "" {
		sb.WriteString("【模特】" + modelID + "\n")
	}
	sb.WriteString("【面料】" + fabric + "\n")
	for utf8.RuneCountInString(sb.String()) < 1000 {
		sb.WriteString("【设计细节保留】参考图中的版型、领口、袖口、下摆、缝线与图案必须原样保留，不得简化成基础款。\n")
	}
	return sb.String()
}

func main() {
	addr := flag.String("addr", ":5057", "listen address")
	flag.Parse()
	log.Printf("mock-reverse listening on %s (MOCK_REVERSE_FAIL=%q)", *addr, os.Getenv("MOCK_REVERSE_FAIL"))
	log.Fatal(http.ListenAndServe(*addr, handler()))
}
