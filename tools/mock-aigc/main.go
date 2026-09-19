// mock-aigc 本地模拟 AIGC 执行面（仅开发/测试用），接口与《任务调用与轮询对接文档.md》一致：
//
//	POST /aigc/generate        提交任务
//	GET  /aigc/getRes/<id>     轮询：第 1 次 waiting、第 2 次 running、之后 success
//
// 请求参数里含 "mock-fail" 的任务返回 failed，用来测试失败流程。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
)

type task struct {
	ID     string
	Type   int
	Images int
	Fail   bool
	Polls  int
}

var (
	mu    sync.Mutex
	tasks = map[string]*task{}
	seq   int
)

func main() {
	addr := flag.String("addr", ":5055", "listen address")
	flag.Parse()
	http.HandleFunc("/aigc/generate", generate)
	http.HandleFunc("/aigc/getRes/", getRes)
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
	t := &task{Type: int(tt), Fail: strings.Contains(string(raw), "mock-fail")}
	switch t.Type {
	case 85:
		for _, k := range []string{"image_path", "audio_path", "aspectRatio", "resolution", "duration"} {
			if body[k] == nil || body[k] == "" {
				writeJSON(w, 400, map[string]any{"code": 400, "message": "task_type=85 缺少 " + k, "success": false})
				return
			}
		}
	case 75:
		imgs, _ := body["images"].([]any)
		if len(imgs) == 0 {
			writeJSON(w, 400, map[string]any{"code": 400, "message": "task_type=75 缺少 images", "success": false})
			return
		}
		t.Images = len(imgs)
	}
	mu.Lock()
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
