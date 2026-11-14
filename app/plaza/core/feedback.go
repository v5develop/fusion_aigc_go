package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"aigc-local-life/app/plaza/model"
	"aigc-local-life/pkg/bundb"
	"aigc-local-life/pkg/xerr"

	"github.com/uptrace/bun"

	"github.com/zeromicro/go-zero/core/logx"
)

// ---------------------------------------------------------------- 清洗

var (
	phoneRe  = regexp.MustCompile(`1[3-9]\d[\s-]?\d{4}[\s-]?\d{4}`)
	emailRe  = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	idCardRe = regexp.MustCompile(`\d{17}[\dXx]`)
	digitsRe = regexp.MustCompile(`^\d+$`)
	sidRe    = regexp.MustCompile(`^[A-Za-z0-9_-]{8,32}$`)
)

// cleanText 去首尾空白、折叠空白、去控制字符，截断到 max 个字符。
func cleanText(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && !unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > max {
		s = string([]rune(s)[:max])
	}
	return strings.TrimSpace(s)
}

// looksPersonal 疑似手机号 / 邮箱 / 证件号。
func looksPersonal(s string) bool {
	return phoneRe.MatchString(s) || emailRe.MatchString(s) || idCardRe.MatchString(s)
}

// normText 规范化：小写、去空白与标点符号，用于聚合。
func normText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, s)
}

// HashIP IP 的加盐哈希（取前 16 个十六进制字符），只用于限频与去重，不可逆。
func (s *Service) HashIP(ip string) string {
	if strings.TrimSpace(ip) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s.cfg.IPHashSalt + "|" + strings.TrimSpace(ip)))
	return hex.EncodeToString(sum[:])[:16]
}

// ---------------------------------------------------------------- 行业建议（16.3）

const (
	suggestionMaxRunes  = 40
	suggestionPerDay    = 3
	suggestionHintReply = "今天提交得有点多，明天再来告诉我们"
)

// SuggestionInput 「没有我的行业？告诉我们」的提交。
type SuggestionInput struct {
	Text   string
	Sid    string
	UserID int64 // 0 = 匿名
	IPHash string
}

// SubmitSuggestion 匿名可提交。清洗后：疑似手机号 / 邮箱的文本直接丢弃（对调用方仍返回成功，不泄露判据）；
// 同 sid 同规范化文本同日只记一条；每 sid / IP 每天 ≤3 条；text 可空（只表示「想要新行业」）。
func (s *Service) SubmitSuggestion(ctx context.Context, in SuggestionInput) error {
	if s.db == nil {
		return ErrNotReady()
	}
	sid := strings.TrimSpace(in.Sid)
	if sid == "" {
		return xerr.New(xerr.ParamMissing, "缺少 sid")
	}
	if !sidRe.MatchString(sid) {
		return errParam("sid 格式不正确")
	}
	text := cleanText(in.Text, suggestionMaxRunes)
	if text != "" && looksPersonal(text) {
		logx.WithContext(ctx).Infof("plaza: 行业建议丢弃 判据=疑似联系方式 len=%d", utf8.RuneCountInString(text))
		return nil
	}
	norm := normText(text)
	if text != "" && norm == "" { // 全是标点
		text = ""
	}
	ipHash := in.IPHash
	if ipHash == "" {
		ipHash = s.HashIP("unknown")
	}
	dayStart := time.Date(s.now().In(cst).Year(), s.now().In(cst).Month(), s.now().In(cst).Day(), 0, 0, 0, 0, cst)

	// 同日同 sid 同规范化文本去重
	if norm != "" {
		n, err := s.db.NewSelect().Model((*model.IndustrySuggestion)(nil)).
			Where("sid = ?", sid).Where("norm_text = ?", norm).
			Where("create_time >= FROM_UNIXTIME(? / 1000)", dayStart.UnixMilli()).Count(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
	}
	n, err := s.db.NewSelect().Model((*model.IndustrySuggestion)(nil)).
		WhereGroup(" AND ", func(q *bun.SelectQuery) *bun.SelectQuery {
			return q.Where("sid = ?", sid).WhereOr("ip_hash = ?", ipHash)
		}).
		Where("create_time >= FROM_UNIXTIME(? / 1000)", dayStart.UnixMilli()).Count(ctx)
	if err != nil {
		return err
	}
	if n >= suggestionPerDay {
		return xerr.NewReason(xerr.Conflict, ReasonSuggestionRateLimited, suggestionHintReply)
	}
	row := model.IndustrySuggestion{Sid: sid, IPHash: ipHash}
	if text != "" {
		row.Text = sql.NullString{String: text, Valid: true}
		row.NormText = sql.NullString{String: norm, Valid: true}
	}
	if in.UserID > 0 {
		row.UserID = sql.NullInt64{Int64: in.UserID, Valid: true}
	}
	_, err = s.db.NewInsert().Model(&row).Exec(ctx)
	return err
}

// ---------------------------------------------------------------- 搜索缺口（17.4）

const (
	missMaxRunes = 30
	missDayTTL   = 48 * time.Hour
)

func missKey(day, industry string) string { return "aigc:plaza:miss:" + day + ":" + industry }

// cleanMissTerm 清洗零结果搜索词：trim、折叠空白、转小写、截断 ≤30 字；
// 疑似手机号 / 邮箱 / 证件号与纯数字串丢弃（返回 ""）。
func cleanMissTerm(s string) string {
	t := strings.ToLower(cleanText(s, missMaxRunes))
	if t == "" || looksPersonal(t) || digitsRe.MatchString(strings.ReplaceAll(t, " ", "")) {
		return ""
	}
	return t
}

// recordSearchMiss 记一次行业内搜索缺口（只在 Templates 首页 0 结果时调；不记 user_id / sid / 原始 IP）。
// 去重：(日, 行业, 词哈希, IP 哈希) 的 SET NX，所以同一人同一天重复搜同一个词只计一次；IP 哈希只存 Redis，不落库。
// 有 Redis：请求内只做 HINCRBY（由 FlushSearchMisses 每 10 分钟入库）；没有 Redis：直接 upsert（开发 / 单实例，不去重）。
func (s *Service) recordSearchMiss(ctx context.Context, industryCode, keyword, ipHash string) {
	term := cleanMissTerm(keyword)
	if term == "" {
		return
	}
	day := s.now().In(cst).Format("20060102")
	if s.rd != nil {
		sum := sha256.Sum256([]byte(term))
		dk := fmt.Sprintf("aigc:plaza:missdedupe:%s:%s:%s:%s", day, industryCode, hex.EncodeToString(sum[:8]), ipHash)
		ok, err := s.rd.SetNX(ctx, dk, 1, missDayTTL).Result()
		if err != nil {
			logx.WithContext(ctx).Errorf("plaza: 搜索缺口去重失败（不计数）: %v", err)
			return
		}
		if !ok {
			return
		}
		pipe := s.rd.Pipeline()
		pipe.HIncrBy(ctx, missKey(day, industryCode), term, 1)
		pipe.Expire(ctx, missKey(day, industryCode), missDayTTL)
		if _, err := pipe.Exec(ctx); err != nil {
			logx.WithContext(ctx).Errorf("plaza: 搜索缺口计数失败: %v", err)
		}
		return
	}
	if s.db != nil {
		if err := s.upsertMiss(ctx, s.now().In(cst), industryCode, term, 1); err != nil {
			logx.WithContext(ctx).Errorf("plaza: 搜索缺口入库失败: %v", err)
		}
	}
}

// upsertMiss 日表与汇总表各 upsert 一次（DUPLICATE KEY UPDATE 累加），同一事务：
// 两条分开提交时，汇总表失败而日表已累加，放回 Redis 重试就会让日表重复计数。
func (s *Service) upsertMiss(ctx context.Context, day time.Time, industry, term string, n int) error {
	d := day.In(cst).Format("2006-01-02")
	return bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO afv_plaza_search_miss_daily (`day`, industry_code, norm_term, cnt) VALUES (?, ?, ?, ?) "+
				"ON DUPLICATE KEY UPDATE cnt = cnt + VALUES(cnt)", d, industry, term, n); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			"INSERT INTO afv_plaza_search_miss_term (industry_code, norm_term, total_cnt, first_day, last_day, status) VALUES (?, ?, ?, ?, ?, 'NEW') "+
				"ON DUPLICATE KEY UPDATE total_cnt = total_cnt + VALUES(total_cnt), last_day = GREATEST(last_day, VALUES(last_day))",
			industry, term, n, d, d)
		return err
	})
}

const (
	missPrefix     = "aigc:plaza:miss:"
	missProcPrefix = "aigc:plaza:miss:proc:"
)

// FlushSearchMisses 把 Redis 里的增量入库（每 10 分钟，由后台任务调用；多副本靠 Redis 锁只让一个跑）。
// 先 RENAME 成处理中的 key 再读，所以并发新增不会丢；某个词入库失败就把它的增量放回原 key（并补上 TTL，不留永不过期的 key）。
// 上一轮读处理中 key 失败 / 进程中途退出留下的处理中 key（带 TTL）本轮直接接着处理，不再 RENAME。
func (s *Service) FlushSearchMisses(ctx context.Context) (int, error) {
	if s.rd == nil || s.db == nil {
		return 0, nil
	}
	var keys []string
	var cursor uint64
	for {
		ks, next, err := s.rd.Scan(ctx, cursor, missPrefix+"*", 200).Result()
		if err != nil {
			return 0, err
		}
		for _, k := range ks {
			if !strings.HasPrefix(k, "aigc:plaza:missdedupe:") {
				keys = append(keys, k)
			}
		}
		if cursor = next; cursor == 0 {
			break
		}
	}
	total := 0
	for _, k := range keys {
		leftover := strings.HasPrefix(k, missProcPrefix)
		rest := strings.TrimPrefix(strings.TrimPrefix(k, missProcPrefix), missPrefix) // yyyymmdd:industry
		i := strings.Index(rest, ":")
		if i <= 0 {
			continue
		}
		dayStr, industry := rest[:i], rest[i+1:]
		day, err := time.ParseInLocation("20060102", dayStr, cst)
		if err != nil {
			continue
		}
		src, proc := missPrefix+rest, missProcPrefix+rest
		if !leftover {
			if err := s.rd.Rename(ctx, k, proc).Err(); err != nil {
				continue // 没了（被别的实例处理走）
			}
		}
		all, err := s.rd.HGetAll(ctx, proc).Result()
		if err != nil {
			// 读不出来：给处理中 key 补 TTL（下一轮作为遗留 key 接着处理），不让它永不过期
			_ = s.rd.Expire(ctx, proc, missDayTTL).Err()
			logx.WithContext(ctx).Errorf("plaza: 搜索缺口入库中断 判据=读取处理中 key 失败 key=%s，下一轮接着处理: %v", proc, err)
			return total, err
		}
		for term, cnt := range all {
			var n int
			if _, err := fmt.Sscan(cnt, &n); err != nil || n <= 0 {
				continue
			}
			if err := s.upsertMiss(ctx, day, industry, term, n); err != nil {
				// 放回原 key（带 TTL），下个周期重试；事务保证日表 / 汇总表都没累加
				pipe := s.rd.Pipeline()
				pipe.HIncrBy(ctx, src, term, int64(n))
				pipe.Expire(ctx, src, missDayTTL)
				if _, perr := pipe.Exec(ctx); perr != nil {
					logx.WithContext(ctx).Errorf("plaza: 搜索缺口放回失败（丢弃本条增量 %d 次） key=%s: %v", n, src, perr)
				}
				logx.WithContext(ctx).Errorf("plaza: 搜索缺口入库失败 判据=upsert 事务出错 行业=%s 已放回 Redis: %v", industry, err)
				continue
			}
			total += n
		}
		_ = s.rd.Del(ctx, proc).Err()
	}
	return total, nil
}

// PurgeOldMissDaily 日表保留 90 天。
func (s *Service) PurgeOldMissDaily(ctx context.Context) error {
	if s.db == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, "DELETE FROM afv_plaza_search_miss_daily WHERE `day` < DATE_SUB(CURDATE(), INTERVAL 90 DAY)")
	return err
}
