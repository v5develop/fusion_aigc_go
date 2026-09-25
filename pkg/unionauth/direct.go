package unionauth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// DirectConf 不调联盟的 HTTP 接口，自己按它的做法验临时 token。
//
// 为什么要这条路：我们的服务器访问不到联盟的公网域名（443 不通），而联盟的 Redis 和
// 用户库都在同一个内网、用户库甚至和我们在同一个 MySQL 实例。与其等网络放行，不如照它的方式验。
//
// ⚠️ 代价是**耦合它的内部实现**：Redis 的 key 格式、value 内容、用户表结构。
// 它们一改我们就挂，而且不会有人通知我们。所以：
//   - 这是降级方案，配了才启用；没配仍走 HTTP 接口（见 Client.Login）；
//   - 下面每个约定都注明了出处，对方改动时照着核。
//
// 出处（mall_user_svc_api）：
//
//	public/model/redis/temp_token.go      key = "temp:token:<token>"
//	.../auth/get_auth_token_logic.go      value = userId 的十进制字符串，TTL 5 分钟
//	.../auth/get_user_info_logic.go       取到后查 uec_app_users，然后 DEL 掉（一次性）
type DirectConf struct {
	// RedisAddr 联盟的 Redis，如 192.168.0.52:6379；留空则不启用这条路
	RedisAddr string `json:",optional"`
	RedisPass string `json:",optional"`
	// UserDSN 联盟的用户库。留空则复用本服务的 DataSource——
	// 两个库在同一个 MySQL 实例时这样最省事，但**本服务的账号要有该库的读权限**。
	UserDSN string `json:",optional"`
	// UserTable 用户表，默认 unionec_app.uec_app_users（带库名，因为通常是跨库查）
	UserTable string `json:",default=unionec_app.uec_app_users"`
	// PhoneAesKey 手机号在它库里是 AES-128-CBC（IV=key）加密的。
	// 配了才解出来再脱敏；不配则手机号留空——不影响登录，只是 App 上显示不了。
	PhoneAesKey string `json:",optional"`
}

// direct 自己验临时 token 的实现。
type direct struct {
	conf  DirectConf
	rds   *redis.Redis
	conn  sqlx.SqlConn
	table string
}

func newDirect(c DirectConf, fallbackDSN string) *direct {
	if strings.TrimSpace(c.RedisAddr) == "" {
		return nil
	}
	dsn := c.UserDSN
	if strings.TrimSpace(dsn) == "" {
		dsn = fallbackDSN
	}
	if strings.TrimSpace(dsn) == "" {
		return nil
	}
	table := c.UserTable
	if table == "" {
		table = "unionec_app.uec_app_users"
	}
	opts := []redis.Option{}
	if c.RedisPass != "" {
		opts = append(opts, redis.WithPass(c.RedisPass))
	}
	return &direct{conf: c, rds: redis.New(c.RedisAddr, opts...), conn: sqlx.NewMysql(dsn), table: table}
}

// tempTokenKey 和联盟一致：temp:token:<token>
func tempTokenKey(token string) string { return "temp:token:" + token }

// userRow 只取我们建档用得上的字段。列名来自 uec_app_users。
type userRow struct {
	Id             int64          `db:"id"`
	Nickname       sql.NullString `db:"nickname"`
	Avatar         sql.NullString `db:"avatar"`
	Phone          sql.NullString `db:"phone"` // AES 密文
	Gender         sql.NullInt64  `db:"gender"`
	Birthday       sql.NullTime   `db:"birthday"`
	Province       sql.NullString `db:"province"`
	City           sql.NullString `db:"city"`
	RealNameStatus sql.NullString `db:"real_name_status"`
	Status         sql.NullString `db:"status"`
	Roles          sql.NullString `db:"roles"`
}

// login 读 Redis 拿 userId → 查用户表 → 删掉 token（一次性，和联盟行为一致）。
func (d *direct) login(ctx context.Context, tempToken string) (*User, error) {
	key := tempTokenKey(tempToken)
	idStr, err := d.rds.GetCtx(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("unionauth: 读临时 token 失败: %w", err)
	}
	if idStr == "" { // go-zero 的 Get 对不存在的 key 返回空串而不是错
		return nil, fmt.Errorf("%w（临时 token 不存在或已过期）", ErrUnauthorized)
	}
	userID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || userID <= 0 {
		return nil, fmt.Errorf("unionauth: 临时 token 里的 userId 不对: %q", idStr)
	}

	var r userRow
	q := "SELECT `id`,`nickname`,`avatar`,`phone`,`gender`,`birthday`,`province`,`city`," +
		"`real_name_status`,`status`,`roles` FROM " + d.table + " WHERE `id` = ? AND `deleted_at` IS NULL LIMIT 1"
	switch err := d.conn.QueryRowCtx(ctx, &r, q, userID); {
	case errors.Is(err, sqlx.ErrNotFound):
		return nil, fmt.Errorf("%w（联盟里没有这个用户 %d）", ErrUnauthorized, userID)
	case err != nil:
		return nil, fmt.Errorf("unionauth: 查联盟用户失败: %w", err)
	}
	// 用过就删，和联盟一致。删失败不影响这次登录（5 分钟后自己过期）
	_, _ = d.rds.DelCtx(ctx, key)

	u := &User{
		UserID:         json.Number(strconv.FormatInt(r.Id, 10)),
		Nickname:       r.Nickname.String,
		Avatar:         r.Avatar.String,
		Gender:         int32(r.Gender.Int64),
		Province:       r.Province.String,
		City:           r.City.String,
		RealNameStatus: r.RealNameStatus.String,
		Status:         r.Status.String,
		Phone:          d.phone(r.Phone.String),
	}
	if r.Birthday.Valid {
		u.Birthday = r.Birthday.Time.Format("2006-01-02")
	}
	if r.Roles.Valid && r.Roles.String != "" {
		u.Roles = strings.Split(r.Roles.String, ",")
	}
	return u, nil
}

// phone 解密并脱敏。没配密钥或解不出来就返回空——登录照常，只是显示不了手机号。
func (d *direct) phone(encrypted string) string {
	if encrypted == "" || d.conf.PhoneAesKey == "" {
		return ""
	}
	plain, err := decryptAesCBC(encrypted, d.conf.PhoneAesKey)
	if err != nil {
		return ""
	}
	return maskPhone(plain)
}

// decryptAesCBC 联盟的手机号加密方式：AES-CBC，**IV 就是 key 本身**，PKCS7 填充，外层 base64。
// 出处：go-zero-core/plugin/encrypt/aes/aes.go 的 AesDecrypt。
func decryptAesCBC(data, key string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return "", err
	}
	size := block.BlockSize()
	if len(raw) == 0 || len(raw)%size != 0 || len(key) < size {
		return "", errors.New("unionauth: 密文长度不对")
	}
	out := make([]byte, len(raw))
	cipher.NewCBCDecrypter(block, []byte(key)[:size]).CryptBlocks(out, raw)
	n := int(out[len(out)-1]) // 去 PKCS7 填充
	if n <= 0 || n > size || n > len(out) {
		return "", errors.New("unionauth: 填充不对（密钥可能不对）")
	}
	return string(out[:len(out)-n]), nil
}

// maskPhone 和联盟一样脱敏成 150****6792。
func maskPhone(p string) string {
	if len(p) != 11 {
		return p
	}
	return p[:3] + "****" + p[7:]
}
