package logic

// MCP 访问令牌（Go_V1.0.42）真库测试：临时库只建 afv_canvas_mcp_token，结束 DROP。
// 无 MySQL 时跳过；CANVAS_AGENT_E2E_DSN 可指定（不带库名，如 root:123456@tcp(127.0.0.1:43306)/）。

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"aigc-local-life/app/canvas/mcptoken"
	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/repo"
	"aigc-local-life/app/canvas/rpc/internal/svc"
	"aigc-local-life/pkg/bundb"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

func mcpTokenDB(t *testing.T) (*McpTokenLogic, *sql.DB) {
	t.Helper()
	base := os.Getenv("CANVAS_AGENT_E2E_DSN")
	if base == "" {
		base = "root:123456@tcp(127.0.0.1:43306)/"
	}
	// 会话时区与 loc 一致（与线上 DSN 同口径）：expires_at 由库按 NOW(3) 算，读回按 loc 解析
	opts := "?multiStatements=true&parseTime=true&loc=Asia%2FShanghai&time_zone=%27%2B08%3A00%27&charset=utf8mb4&timeout=2s"
	admin, err := sql.Open("mysql", base+opts)
	if err != nil {
		t.Skipf("无 MySQL：%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		_ = admin.Close()
		t.Skipf("无 MySQL（%s）：%v", base, err)
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	name := "scratch_canvas_mcp_token_" + hex.EncodeToString(b)
	if _, err := admin.Exec("CREATE DATABASE `" + name + "` DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Skipf("建临时库失败：%v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP DATABASE IF EXISTS `" + name + "`")
		_ = admin.Close()
	})
	db, err := sql.Open("mysql", base+name+opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	raw, err := os.ReadFile("../../../../../deploy/sql/Go_V1.0.42__canvas_mcp_token.sql")
	if err != nil {
		t.Fatal(err)
	}
	// 整个迁移文件原样执行（multiStatements）：顺带验证可执行、可重复执行
	for i := 0; i < 2; i++ {
		if _, err := db.Exec(string(raw)); err != nil {
			t.Fatalf("执行迁移失败（第 %d 次）: %v", i+1, err)
		}
	}
	// Java 用户表的最小子集（校验令牌时联查 status / deleted）：测试里用到的用户都启用
	if _, err := db.Exec("CREATE TABLE afv_app_user (id bigint PRIMARY KEY, status tinyint NOT NULL DEFAULT 1, deleted tinyint NOT NULL DEFAULT 0)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO afv_app_user (id) VALUES (7001),(7002),(7100),(7101)"); err != nil {
		t.Fatal(err)
	}
	conn := sqlx.NewSqlConnFromDB(db)
	bdb, err := bundb.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	s := &svc.ServiceContext{Conn: conn, DB: bdb, Repo: repo.New(bdb)}
	return NewMcpTokenLogic(context.Background(), s), db
}

func reasonOf(err error) string { return xerr.ReasonOf(err) }

func TestMcpTokenLifecycleRealDB(t *testing.T) {
	l, db := mcpTokenDB(t)
	const uid = 7001

	// 参数校验
	for _, bad := range []*canvas.CreateMcpTokenReq{
		{UserId: uid, Name: "", ExpiresInDays: 7, Scope: mcptoken.ScopeRW},
		{UserId: uid, Name: strings.Repeat("长", 51), ExpiresInDays: 7, Scope: mcptoken.ScopeRW},
		{UserId: uid, Name: "x", ExpiresInDays: 8, Scope: mcptoken.ScopeRW},
		{UserId: uid, Name: "x", ExpiresInDays: 7, Scope: "admin"},
		{UserId: 0, Name: "x", ExpiresInDays: 7, Scope: mcptoken.ScopeRW},
	} {
		if _, err := l.CreateMcpToken(bad); err == nil {
			t.Fatalf("应拒绝: %+v", bad)
		}
	}

	out, err := l.CreateMcpToken(&canvas.CreateMcpTokenReq{UserId: uid, Name: " Claude Desktop ", ExpiresInDays: 30, Scope: mcptoken.ScopeRW, ClientIp: "203.0.113.1"})
	if err != nil {
		t.Fatal(err)
	}
	plain := out.PlainToken
	if !mcptoken.Looks(plain) || out.Token.TokenPrefix != plain[:mcptoken.DisplayPrefixLen] || out.Token.Name != "Claude Desktop" {
		t.Fatalf("创建结果不对: %+v plain=%q", out.Token, plain)
	}
	days := time.Until(time.UnixMilli(out.Token.ExpiresAt)).Hours() / 24
	if days < 29.9 || days > 30.1 || out.Token.Expired {
		t.Fatalf("过期时间不对: %.2f 天 expired=%v", days, out.Token.Expired)
	}

	// 哈希存储：库里没有明文，只有 sha256 与前缀
	var hash, prefix string
	var n int
	if err := db.QueryRow("SELECT token_hash, token_prefix FROM afv_canvas_mcp_token WHERE id = ?", out.Token.Id).Scan(&hash, &prefix); err != nil {
		t.Fatal(err)
	}
	if hash != mcptoken.Hash(plain) || prefix != plain[:12] {
		t.Fatalf("哈希 / 前缀不对: %s %s", hash, prefix)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM afv_canvas_mcp_token WHERE CONCAT_WS('|', name, token_prefix, token_hash, scope, created_ip) LIKE ?", "%"+plain+"%").Scan(&n); err != nil || n != 0 {
		t.Fatalf("库里不应出现明文: n=%d err=%v", n, err)
	}

	// 校验：成功 + 记最后使用
	v, err := l.VerifyMcpToken(&canvas.VerifyMcpTokenReq{TokenHash: mcptoken.Hash(plain), ClientIp: "198.51.100.9"})
	if err != nil || v.UserId != uid || v.Scope != mcptoken.ScopeRW || v.TokenId != out.Token.Id {
		t.Fatalf("校验: %+v %v", v, err)
	}
	list, err := l.ListMcpTokens(&canvas.ListMcpTokensReq{UserId: uid})
	if err != nil || len(list.Tokens) != 1 || list.ActiveCount != 1 || list.MaxActive != 10 {
		t.Fatalf("列表: %+v %v", list, err)
	}
	if list.Tokens[0].LastUsedIp != "198.51.100.9" || list.Tokens[0].LastUsedAt == 0 {
		t.Fatalf("最后使用没记上: %+v", list.Tokens[0])
	}
	// 他人看不到、吊销不了
	if other, _ := l.ListMcpTokens(&canvas.ListMcpTokensReq{UserId: uid + 1}); len(other.Tokens) != 0 {
		t.Fatal("他人列表不应看到")
	}
	if _, err := l.RevokeMcpToken(&canvas.RevokeMcpTokenReq{UserId: uid + 1, Id: out.Token.Id}); err == nil {
		t.Fatal("他人吊销应 NotFound")
	} else if code, _, _ := xerr.Parse(err); code != xerr.NotFound {
		t.Fatalf("他人吊销应 NotFound: %v", err)
	}
	if _, err := l.VerifyMcpToken(&canvas.VerifyMcpTokenReq{TokenHash: mcptoken.Hash(plain)}); err != nil {
		t.Fatal("他人吊销失败后应仍可用")
	}

	// 错误令牌 / 非法哈希
	if _, err := l.VerifyMcpToken(&canvas.VerifyMcpTokenReq{TokenHash: mcptoken.Hash(plain + "x")}); reasonOf(err) != ReasonMcpTokenInvalid {
		t.Fatalf("错误令牌: %v", err)
	}
	if _, err := l.VerifyMcpToken(&canvas.VerifyMcpTokenReq{TokenHash: plain}); reasonOf(err) != ReasonMcpTokenInvalid {
		t.Fatalf("明文当哈希应拒绝: %v", err)
	}

	// 过期
	exp, err := l.CreateMcpToken(&canvas.CreateMcpTokenReq{UserId: uid, Name: "exp", ExpiresInDays: 7, Scope: mcptoken.ScopeRWGenerate})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE afv_canvas_mcp_token SET expires_at = DATE_SUB(NOW(3), INTERVAL 1 SECOND) WHERE id = ?", exp.Token.Id); err != nil {
		t.Fatal(err)
	}
	if _, err := l.VerifyMcpToken(&canvas.VerifyMcpTokenReq{TokenHash: mcptoken.Hash(exp.PlainToken)}); reasonOf(err) != ReasonMcpTokenExpired {
		t.Fatalf("过期应拒绝: %v", err)
	} else if code, _, _ := xerr.Parse(err); code != xerr.Unauthorized {
		t.Fatalf("过期应 Unauthorized: %v", err)
	}
	list, _ = l.ListMcpTokens(&canvas.ListMcpTokensReq{UserId: uid})
	if len(list.Tokens) != 2 || list.ActiveCount != 1 || !list.Tokens[0].Expired {
		t.Fatalf("过期的应在列表里标 expired 且不计入生效数: %+v", list)
	}

	// 吊销：即时失效，幂等
	rv, err := l.RevokeMcpToken(&canvas.RevokeMcpTokenReq{UserId: uid, Id: out.Token.Id})
	if err != nil || rv.TokenHash != mcptoken.Hash(plain) {
		t.Fatalf("吊销: %+v %v", rv, err)
	}
	if _, err := l.VerifyMcpToken(&canvas.VerifyMcpTokenReq{TokenHash: mcptoken.Hash(plain)}); reasonOf(err) != ReasonMcpTokenInvalid {
		t.Fatalf("吊销后应拒绝: %v", err)
	}
	if _, err := l.RevokeMcpToken(&canvas.RevokeMcpTokenReq{UserId: uid, Id: out.Token.Id}); err != nil {
		t.Fatalf("重复吊销应幂等: %v", err)
	}
	list, _ = l.ListMcpTokens(&canvas.ListMcpTokensReq{UserId: uid})
	if len(list.Tokens) != 1 {
		t.Fatalf("吊销的不应出现在列表: %+v", list)
	}
}

func TestMcpTokenLimitRealDB(t *testing.T) {
	l, _ := mcpTokenDB(t)
	const uid = 7100
	// 并发建 14 个：恰好 10 个成功，其余报 MCP_TOKEN_LIMIT（事务内加锁计数）
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		ok, lim  int
		firstID  int64
		otherErr []error
	)
	for i := 0; i < 14; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := l.CreateMcpToken(&canvas.CreateMcpTokenReq{UserId: uid, Name: "t", ExpiresInDays: 7, Scope: mcptoken.ScopeRW})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
				if firstID == 0 {
					firstID = out.Token.Id
				}
			case reasonOf(err) == ReasonMcpTokenLimit:
				lim++
			default:
				otherErr = append(otherErr, err)
			}
		}()
	}
	wg.Wait()
	// 并发加锁可能有个别死锁回滚（InnoDB 检测后报错），不算超额；关键是成功数不超过 10
	if ok > mcptoken.MaxActivePerUser || ok+lim+len(otherErr) != 14 {
		t.Fatalf("并发上限: ok=%d limit=%d other=%v", ok, lim, otherErr)
	}
	for ok < mcptoken.MaxActivePerUser { // 补齐到 10（死锁回滚的那几个）
		if _, err := l.CreateMcpToken(&canvas.CreateMcpTokenReq{UserId: uid, Name: "t", ExpiresInDays: 7, Scope: mcptoken.ScopeRW}); err != nil {
			t.Fatalf("补齐: %v", err)
		}
		ok++
	}
	if _, err := l.CreateMcpToken(&canvas.CreateMcpTokenReq{UserId: uid, Name: "t", ExpiresInDays: 7, Scope: mcptoken.ScopeRW}); reasonOf(err) != ReasonMcpTokenLimit {
		t.Fatalf("第 11 个应报上限: %v", err)
	}
	// 吊销一个后可以再建
	if _, err := l.RevokeMcpToken(&canvas.RevokeMcpTokenReq{UserId: uid, Id: firstID}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.CreateMcpToken(&canvas.CreateMcpTokenReq{UserId: uid, Name: "t", ExpiresInDays: 7, Scope: mcptoken.ScopeRW}); err != nil {
		t.Fatalf("吊销后应可再建: %v", err)
	}
	// 其他用户不受影响
	if _, err := l.CreateMcpToken(&canvas.CreateMcpTokenReq{UserId: uid + 1, Name: "t", ExpiresInDays: 7, Scope: mcptoken.ScopeRW}); err != nil {
		t.Fatalf("他人不应受上限影响: %v", err)
	}
}

func TestMcpTokenUserDisabledRealDB(t *testing.T) {
	l, db := mcpTokenDB(t)
	out, err := l.CreateMcpToken(&canvas.CreateMcpTokenReq{UserId: 7001, Name: "x", ExpiresInDays: 7, Scope: mcptoken.ScopeRW})
	if err != nil {
		t.Fatal(err)
	}
	h := mcptoken.Hash(out.PlainToken)
	if _, err := l.VerifyMcpToken(&canvas.VerifyMcpTokenReq{TokenHash: h}); err != nil {
		t.Fatalf("启用用户应通过: %v", err)
	}
	// 禁用（afv_app_user.status=0）→ 失效
	if _, err := db.Exec("UPDATE afv_app_user SET status = 0 WHERE id = 7001"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.VerifyMcpToken(&canvas.VerifyMcpTokenReq{TokenHash: h}); reasonOf(err) != ReasonMcpTokenUserDisabled {
		t.Fatalf("禁用用户应拒绝: %v", err)
	} else if code, _, _ := xerr.Parse(err); code != xerr.Unauthorized {
		t.Fatalf("应 Unauthorized: %v", err)
	}
	// 恢复启用 → 可用；逻辑删除 → 失效
	if _, err := db.Exec("UPDATE afv_app_user SET status = 1 WHERE id = 7001"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.VerifyMcpToken(&canvas.VerifyMcpTokenReq{TokenHash: h}); err != nil {
		t.Fatalf("恢复后应通过: %v", err)
	}
	if _, err := db.Exec("UPDATE afv_app_user SET deleted = 1 WHERE id = 7001"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.VerifyMcpToken(&canvas.VerifyMcpTokenReq{TokenHash: h}); reasonOf(err) != ReasonMcpTokenUserDisabled {
		t.Fatalf("已删除用户应拒绝: %v", err)
	}
	// 用户表里根本没有这个人
	other, err := l.CreateMcpToken(&canvas.CreateMcpTokenReq{UserId: 9999, Name: "x", ExpiresInDays: 7, Scope: mcptoken.ScopeRW})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.VerifyMcpToken(&canvas.VerifyMcpTokenReq{TokenHash: mcptoken.Hash(other.PlainToken)}); reasonOf(err) != ReasonMcpTokenUserDisabled {
		t.Fatalf("不存在的用户应拒绝: %v", err)
	}
}
