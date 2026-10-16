// mock-fusion 本地模拟现有 Java 服务（fusion_aigc）的联盟开放接口，仅开发/测试用：
//
//	GET  /api/v1/union/user-exists?unionUserId=...   用户是否已建档
//	POST /api/v1/union/user-register                 建档，返回 userId
//
// 它**会真的写库**（afv_app_user + afv_team + afv_team_member），因为真的 Java 服务
// 就是这么干的——我们建完要回查同一张表。
//
// 最有价值的部分是**验签**：完全按对方文档独立算一遍 HMAC-SHA256，
// 我们的签名拼错一个换行这里就会 401，免得上线连不上才发现。
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

var (
	db        *sql.DB
	appKey    = flag.String("app-key", "12345678910", "应用 key")
	appSecret = flag.String("app-secret", "mock-secret", "应用密钥")
	mu        sync.Mutex
	calls     = map[string]int{} // 各接口被调了几次，e2e 用来验证走没走这条路
)

func main() {
	addr := flag.String("addr", ":5057", "listen address")
	dsn := flag.String("dsn", "", "MySQL DSN（和网关同一个库）")
	flag.Parse()

	var err error
	if db, err = sql.Open("mysql", *dsn); err != nil {
		log.Fatalf("mock-fusion: 连库失败: %v", err)
	}
	http.HandleFunc("/api/v1/union/user-exists", signed(userExists))
	http.HandleFunc("/api/v1/union/user-register", signed(userRegister))
	http.HandleFunc("/debug/calls", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		out := map[string]int{}
		for k, v := range calls {
			out[k] = v
		}
		if r.URL.Query().Get("reset") != "" {
			calls = map[string]int{}
		}
		mu.Unlock()
		writeJSON(w, 200, out)
	})
	log.Printf("mock-fusion listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

// signed 验签中间件：待签串 method\nuri\nquery\nsha256(body)\nts\nnonce\nappKey
func signed(next func(http.ResponseWriter, *http.Request, []byte)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		mu.Lock()
		calls[r.URL.Path]++
		mu.Unlock()

		ts := r.Header.Get("X-Union-Timestamp")
		nonce := r.Header.Get("X-Union-Nonce")
		sec, err := strconv.ParseInt(ts, 10, 64)
		switch {
		case r.Header.Get("X-Union-App-Key") != *appKey:
			fail(w, http.StatusUnauthorized, 4001, "appKey 不对")
			return
		case err != nil || absSeconds(sec) > 300:
			fail(w, http.StatusUnauthorized, 4001, "时间戳不在 5 分钟窗口内")
			return
		case len(nonce) != 32:
			fail(w, http.StatusUnauthorized, 4001, "nonce 不对")
			return
		}
		sum := sha256.Sum256(body)
		src := r.Method + "\n" + r.URL.Path + "\n" + r.URL.RawQuery + "\n" +
			hex.EncodeToString(sum[:]) + "\n" + ts + "\n" + nonce + "\n" + *appKey
		mac := hmac.New(sha256.New, []byte(*appSecret))
		mac.Write([]byte(src))
		if !hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(r.Header.Get("X-Union-Signature"))) {
			log.Printf("mock-fusion: 验签不过，待签串=%q", src)
			fail(w, http.StatusUnauthorized, 4001, "签名错误")
			return
		}
		next(w, r, body)
	}
}

func absSeconds(unix int64) int64 {
	d := time.Now().Unix() - unix
	if d < 0 {
		return -d
	}
	return d
}

func userExists(w http.ResponseWriter, r *http.Request, _ []byte) {
	var id int64
	err := db.QueryRow("SELECT id FROM afv_app_user WHERE union_user_id = ? AND deleted = 0",
		r.URL.Query().Get("unionUserId")).Scan(&id)
	switch err {
	case nil:
		ok(w, true)
	case sql.ErrNoRows:
		ok(w, false)
	default:
		fail(w, 500, 500, err.Error())
	}
}

type registerReq struct {
	UnionUserID    string   `json:"unionUserId"`
	Nickname       string   `json:"nickname"`
	Avatar         string   `json:"avatar"`
	Phone          string   `json:"phone"`
	Birthday       string   `json:"birthday"`
	Gender         int32    `json:"gender"`
	Province       string   `json:"province"`
	City           string   `json:"city"`
	RealNameStatus string   `json:"realNameStatus"`
	Roles          []string `json:"roles"`
	Status         string   `json:"status"`
}

func userRegister(w http.ResponseWriter, r *http.Request, body []byte) {
	var req registerReq
	if err := json.Unmarshal(body, &req); err != nil {
		fail(w, http.StatusBadRequest, 1001, "请求体解析失败")
		return
	}
	if req.UnionUserID == "" {
		fail(w, http.StatusBadRequest, 1001, "unionUserId 不能为空")
		return
	}
	if req.Roles == nil {
		fail(w, http.StatusBadRequest, 1001, "roles 不能为空")
		return
	}
	var id int64
	err := db.QueryRow("SELECT id FROM afv_app_user WHERE union_user_id = ? AND deleted = 0", req.UnionUserID).Scan(&id)
	if err == nil { // 已存在：真实服务是幂等命中，返回原来的人
		ok(w, map[string]any{"userId": id, "nickname": req.Nickname})
		return
	}
	if err != sql.ErrNoRows {
		fail(w, 500, 500, err.Error())
		return
	}
	id, err = insertUser(req)
	if err != nil {
		if strings.Contains(err.Error(), "Duplicate entry") { // 并发撞车
			fail(w, http.StatusConflict, 4003, "用户已存在")
			return
		}
		fail(w, 500, 500, err.Error())
		return
	}
	ok(w, map[string]any{"userId": id, "nickname": req.Nickname})
}

// insertUser 按 Java 的口径写用户 + 个人团队。
func insertUser(req registerReq) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	status := 1
	switch req.Status {
	case "frozen", "deactivating", "deactivated":
		status = 0
	}
	res, err := tx.Exec("INSERT INTO afv_app_user (username,password,nickname,avatar,phone,gender,birthday,"+
		"province,city,real_name_status,source_app_key,external_user_id,union_user_id,login_source,union_bound,"+
		"union_bound_time,data_source,union_nickname,union_avatar,status) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,'UNION',1,NOW(),2,?,?,?)",
		"union_"+req.UnionUserID, "$2a$10$mockmockmockmockmockmockmockmockmockmockmo",
		nz(req.Nickname), nz(req.Avatar), nz(req.Phone), req.Gender, nz(req.Birthday),
		nz(req.Province), nz(req.City), nz(req.RealNameStatus), *appKey, req.UnionUserID, req.UnionUserID,
		nz(req.Nickname), nz(req.Avatar), status)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	nickname := req.Nickname
	if nickname == "" {
		nickname = fmt.Sprintf("用户%d", id)
		if _, err := tx.Exec("UPDATE afv_app_user SET nickname = ? WHERE id = ?", nickname, id); err != nil {
			return 0, err
		}
	}
	team, err := tx.Exec("INSERT INTO afv_team (name,owner_user_id,status) VALUES (?,?,1)", nickname, id)
	if err != nil {
		return 0, err
	}
	teamID, err := team.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec("INSERT INTO afv_team_member (team_id,user_id,role,status,join_time) VALUES (?,?,1,1,NOW())",
		teamID, id); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func nz(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func ok(w http.ResponseWriter, data any) {
	writeJSON(w, 200, map[string]any{"code": 0, "msg": "ok", "data": data})
}

func fail(w http.ResponseWriter, httpStatus, code int, msg string) {
	writeJSON(w, httpStatus, map[string]any{"code": code, "msg": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Println("mock-fusion: write:", err)
	}
}
