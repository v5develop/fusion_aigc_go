// mock-linkparse 本地模拟同事的分享链接解析服务（parse_share_link），仅开发/测试用：
//
//	POST /parse  {"url": "<分享链接或整段分享文案>"} → 作品信息 + 媒体直链
//
// 真实服务在 http://101.37.203.33:8000（线上配到那个地址）。这里只保证**形状一样**，
// 并且自己把视频和封面也一起供出去，好让下载转存那一步能真的跑通。
//
// 链接里带上关键字可以模拟异常：
//
//	notsupport → 422（平台不支持 / 文案里没链接）
//	nophoto    → 图集作品（medias 里全是 image，没有视频）
//	mux        → B站 音视频分离（needs_mux=true，直接下会是哑片）
//	long       → 时长 10 分钟（超过 3 分钟上限）
//	huge       → 视频体积很大（超过 50M 上限）
//	down500    → 直链下载 500
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

var addr = flag.String("addr", ":5058", "listen address")

// hugeBytes 「超大视频」的体积，比 50M 上限大一点就够，用来验证下载限流
const hugeBytes = 55 << 20

func main() {
	flag.Parse()
	http.HandleFunc("/parse", parse)
	http.HandleFunc("/sample.mp4", media)
	http.HandleFunc("/huge.mp4", media)
	http.HandleFunc("/down500.mp4", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	http.HandleFunc("/cover.jpg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("mock-cover"))
	})
	log.Printf("mock-linkparse listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func parse(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL           string `json:"url"`
		PreferQuality bool   `json:"prefer_quality"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	_ = json.Unmarshal(body, &req)
	link := req.URL

	if link == "" || strings.Contains(link, "notsupport") {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"detail": "不支持的平台或没找到链接"})
		return
	}
	base := "http://127.0.0.1" + *addr
	out := map[string]any{
		"platform": "douyin", "item_id": "7300000000000000000",
		"source_url": "https://www.douyin.com/video/7300000000000000000",
		"title":      "mock 作品标题",
		"cover":      map[string]any{"url": base + "/cover.jpg", "kind": "cover"},
		"medias": []map[string]any{{
			"url": base + "/sample.mp4", "kind": "video", "duration": 15.0, "user_agent": nil,
			"referer_policy": "none",
		}},
		"needs_mux": false, "engine": "mock", "layer": "L1", "elapsed_ms": 12, "cached": false,
	}
	switch {
	case strings.Contains(link, "nophoto"):
		out["platform"] = "xhs"
		out["medias"] = []map[string]any{{"url": base + "/1.jpg", "kind": "image"}}
	case strings.Contains(link, "mux"):
		out["platform"] = "bilibili"
		out["needs_mux"] = true
		out["medias"] = []map[string]any{
			{"url": base + "/sample.mp4", "kind": "video", "user_agent": "Mozilla/5.0 mock"},
			{"url": base + "/a.m4a", "kind": "audio"},
		}
	case strings.Contains(link, "long"):
		out["medias"] = []map[string]any{{"url": base + "/sample.mp4", "kind": "video", "duration": 600.0}}
	case strings.Contains(link, "huge"):
		out["medias"] = []map[string]any{{"url": base + "/huge.mp4", "kind": "video", "duration": 20.0}}
	case strings.Contains(link, "down500"):
		out["medias"] = []map[string]any{{"url": base + "/down500.mp4", "kind": "video", "duration": 20.0}}
	}
	writeJSON(w, http.StatusOK, out)
}

// media 供出「视频文件」。huge 那个按上限以上的体积吐，用来验证边下边数会断掉。
func media(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "video/mp4")
	if strings.Contains(r.URL.Path, "huge") {
		// 不设 Content-Length，逼调用方边读边数
		chunk := make([]byte, 1<<20)
		for i := 0; i < hugeBytes>>20; i++ {
			if _, err := w.Write(chunk); err != nil {
				return // 对方按上限断了连接，正是我们要验的
			}
		}
		return
	}
	_, _ = w.Write([]byte("mock-mp4-body"))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		fmt.Println("mock-linkparse: write:", err)
	}
}
