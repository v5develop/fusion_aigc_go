// mock-aigc 本地模拟 AIGC 执行面（仅开发/测试用），接口与《任务调用与轮询对接文档.md》一致：
//
//	POST /aigc/generate        提交任务
//	GET  /aigc/getRes/<id>     轮询：第 1 次 waiting、第 2 次 running、之后 success
//
// 请求参数里含 "mock-fail" 的任务返回 failed，用来测试失败流程；
// 含 "mock-fail-<task_type>"（如 mock-fail-75）的只让该类型失败，方便测多步流程中的某一步；
// 含 "mock-partial" 的每两个任务失败一个，用来测一次出多张时的部分成功。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

type task struct {
	ID     string
	Type   int
	Images int
	Fail   bool
	Polls  int
	Body   map[string]any
}

var (
	mu      sync.Mutex
	tasks   = map[string]*task{}
	seq     int
	partial int
)

func main() {
	addr := flag.String("addr", ":5055", "listen address")
	flag.Parse()
	http.HandleFunc("/aigc/generate", generate)
	http.HandleFunc("/aigc/getRes/", getRes)
	http.HandleFunc("/mock/task/", taskBody) // 仅测试用：查看某任务收到的请求体
	log.Printf("mock aigc listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func generate(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"code": 400, "message": "invalid json", "success": false})
		return
	}
	raw, _ := json.Marshal(body)
	tt, _ := body["task_type"].(float64)
	t := &task{Type: int(tt), Fail: shouldFail(string(raw), int(tt)), Body: body}
	switch t.Type {
	case 85:
		for _, k := range []string{"image_path", "audio_path", "aspectRatio", "resolution", "duration"} {
			if body[k] == nil || body[k] == "" {
				writeJSON(w, 400, map[string]any{"code": 400, "message": "task_type=85 缺少 " + k, "success": false})
				return
			}
		}
	case 74:
		if s, _ := body["user_text"].(string); s == "" {
			writeJSON(w, 400, map[string]any{"code": 400, "message": "task_type=74 缺少 user_text", "success": false})
			return
		}
	case 9, 86:
		for _, k := range []string{"input_video", "reference_image"} {
			if v, _ := body[k].(string); v == "" {
				writeJSON(w, 400, map[string]any{"code": 400, "message": fmt.Sprintf("task_type=%d 缺少 %s", t.Type, k), "success": false})
				return
			}
		}
		// 真实行为：只有 model=lite 留在本地 9，缺省或其它值都路由到 86
		if m, _ := body["model"].(string); m != "lite" {
			t.Type = 86
		}
	case 92:
		for _, k := range []string{"text", "CHAR_01_url"} {
			if v, _ := body[k].(string); v == "" {
				writeJSON(w, 400, map[string]any{"code": 400, "message": "task_type=92 缺少 " + k, "success": false})
				return
			}
		}
		if d, _ := body["duration"].(float64); d != 15 && d != 30 {
			writeJSON(w, 400, map[string]any{"code": 400, "message": "task_type=92 duration 须为 15 或 30", "success": false})
			return
		}
		// 真实行为：model=lite 路由到本地 91，缺省 / seedance2.5 留在云端 92
		if m, _ := body["model"].(string); m == "lite" {
			t.Type = 91
		}
	case 76:
		for _, k := range []string{"user_text", "template_url"} {
			if s, _ := body[k].(string); s == "" {
				writeJSON(w, 400, map[string]any{"code": 400, "message": "task_type=76 缺少 " + k, "success": false})
				return
			}
		}
	case 78:
		// 真实 78 的 materials 是否必填待 AIGC 确认；这里只校验 template
		if s, _ := body["template"].(string); s == "" {
			writeJSON(w, 400, map[string]any{"code": 400, "message": "task_type=78 缺少 template", "success": false})
			return
		}
	case 75:
		imgs, _ := body["images"].([]any)
		if len(imgs) == 0 {
			writeJSON(w, 400, map[string]any{"code": 400, "message": "task_type=75 缺少 images", "success": false})
			return
		}
		// 按最新对接文档：参考图字段为 image_urls，reference_materials 是它的 1 起下标
		refs, _ := body["image_urls"].([]any)
		for _, im := range imgs {
			m, _ := im.(map[string]any)
			rm, _ := m["reference_materials"].([]any)
			for _, r := range rm {
				if n, _ := r.(float64); int(n) < 1 || int(n) > len(refs) {
					writeJSON(w, 400, map[string]any{"code": 400, "message": "task_type=75 reference_materials 超出 image_urls", "success": false})
					return
				}
			}
		}
		t.Images = len(imgs)
	}
	mu.Lock()
	if strings.Contains(string(raw), "mock-partial") {
		partial++
		t.Fail = partial%2 == 0 // 每两个失败一个
	}
	id, _ := body["task_id"].(string)
	if id == "" {
		seq++
		id = fmt.Sprintf("mock_%d", seq)
	}
	t.ID = id
	tasks[id] = t
	n := len(tasks)
	mu.Unlock()
	log.Printf("generate %s type=%d fail=%v", id, t.Type, t.Fail)
	writeJSON(w, 200, map[string]any{"code": 200, "message": "success", "success": true,
		"data": map[string]any{"task_id": id, "task_type": t.Type, "queue_length": n}})
}

func getRes(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/aigc/getRes/")
	mu.Lock()
	t, ok := tasks[id]
	if ok {
		t.Polls++
	}
	mu.Unlock()
	switch {
	case !ok:
		writeJSON(w, 200, map[string]any{"task_id": id, "status": "not_found", "message": "任务不存在", "original_task": nil})
	case t.Polls == 1:
		writeJSON(w, 200, map[string]any{"task_id": id, "status": "waiting", "message": "任务正在队列中等待", "original_task": nil})
	case t.Polls == 2:
		writeJSON(w, 200, map[string]any{"task_id": id, "status": "running", "message": "任务正在处理中", "original_task": nil})
	case t.Fail:
		writeJSON(w, 200, map[string]any{"task_id": id, "task_type": t.Type, "status": "failed",
			"error_msg": "mock: 模拟生成失败", "error_code": nil, "upstream": nil, "pool": "platform", "cost": 0})
	case t.Type == 74:
		writeJSON(w, 200, map[string]any{"task_id": id, "task_type": 74, "status": "success",
			"generated_image_path": nil, "pool": "platform", "cost": 0, "script": mockScript(id, t.Body)})
	case t.Type == 75:
		paths := make([]string, t.Images)
		for i := range paths {
			paths[i] = fmt.Sprintf("https://mock-oss.example.com/aigc/%s_%d.png", id, i)
		}
		writeJSON(w, 200, map[string]any{"task_id": id, "task_type": 75, "status": "success",
			"generated_image_path": "", "image_paths": paths, "pool": "platform", "cost": 0})
	default:
		writeJSON(w, 200, map[string]any{"task_id": id, "task_type": t.Type, "status": "success",
			"generated_image_path": fmt.Sprintf("https://mock-oss.example.com/aigc/%s.mp4", id), "pool": "platform", "cost": 0})
	}
}

// mockScript 按 AIGC 同事给的 74 真实返回结构造一份：note{title,content,tags} + images[] + images_url[]。
// images_url 依次为 person_url、material_urls（与真实行为一致：参考图拼进 images_url）。
func mockScript(id string, body map[string]any) map[string]any {
	var refs []string
	if p, _ := body["person_url"].(string); p != "" {
		refs = append(refs, p)
	}
	if ms, ok := body["material_urls"].([]any); ok {
		for _, m := range ms {
			if s, _ := m.(string); s != "" {
				refs = append(refs, fmt.Sprintf("https://mock-oss.example.com/aigc_temp/%s_%d.jpg", id, len(refs)+1))
				_ = s
			}
		}
	}
	n := 1
	if c, ok := body["image_count"].(float64); ok && c > 0 {
		n = int(c)
	}
	images := make([]map[string]any, n)
	for i := range images {
		ref := []int{}
		for j := range refs {
			if j < 4 {
				ref = append(ref, j+1)
			}
		}
		images[i] = map[string]any{"index": i + 1, "image_prompt": fmt.Sprintf("mock 第 %d 张：%v", i+1, body["user_text"]), "reference_materials": ref}
	}
	if refs == nil {
		refs = []string{}
	}
	return map[string]any{
		"images":     images,
		"images_url": refs,
		"note": map[string]any{
			"title":   fmt.Sprintf("%v｜mock 标题", body["note_type"]),
			"content": fmt.Sprintf("mock 正文：%v\n\n第二段。", body["user_text"]),
			"tags":    []string{"#本地探店", "#周末去哪儿", "本地探店"},
		},
	}
}

var (
	failRe    = regexp.MustCompile(`mock-fail(?:-(\d+))?`)
	taskTypes = map[string]bool{"9": true, "45": true, "74": true, "75": true, "76": true, "78": true, "85": true, "86": true, "91": true, "92": true}
)

// shouldFail "mock-fail-<任务号>" 只让该类型失败；其余 "mock-fail…"（包括后面跟的不是任务号）让所有类型失败。
func shouldFail(raw string, taskType int) bool {
	for _, m := range failRe.FindAllStringSubmatch(raw, -1) {
		if !taskTypes[m[1]] || m[1] == fmt.Sprint(taskType) {
			return true
		}
	}
	return false
}

func taskBody(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	t, ok := tasks[strings.TrimPrefix(r.URL.Path, "/mock/task/")]
	mu.Unlock()
	if !ok {
		writeJSON(w, 404, map[string]any{"message": "not found"})
		return
	}
	writeJSON(w, 200, t.Body)
}
