package logic

// 画布 MCP 访问令牌（Go_V1.0.42）：建 / 列 / 吊销 / 校验。格式与作用域见 app/canvas/mcptoken；设计见 docs/design/canvas-agent-tools.md §13。
//
//   - user_id 只来自网关鉴权（管理接口只认登录令牌，网关已拒绝 MCP 令牌管理令牌）。
//   - 明文只在 CreateMcpToken 的响应里出现一次；库里只有 sha256 + 前缀，日志不打明文与哈希全文。
//   - 校验（VerifyMcpToken）收的是哈希，明文不出网关；网关缓存校验结果（≤60 秒），吊销后由网关删缓存。

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"aigc-local-life/app/canvas/mcptoken"
	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/repo"
	"aigc-local-life/app/canvas/rpc/internal/svc"
	"aigc-local-life/pkg/xerr"

	"github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/logx"
)

// MCP 令牌失败原因（xerr reason，网关透传给前端 / MCP 客户端）。
const (
	ReasonMcpTokenLimit   = "MCP_TOKEN_LIMIT"
	ReasonMcpTokenInvalid = "MCP_TOKEN_INVALID"
	ReasonMcpTokenExpired = "MCP_TOKEN_EXPIRED"
	// ReasonMcpTokenUserDisabled 所有者已被禁用（afv_app_user.status≠1）或已删除：令牌随之失效（网关正缓存最长 60 秒）
	ReasonMcpTokenUserDisabled = "MCP_TOKEN_USER_DISABLED"
)

// mcpTokenListLimit 列表最多返回条数（未吊销的，含已过期）。
const mcpTokenListLimit = 50

// McpTokenLogic MCP 访问令牌。
type McpTokenLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewMcpTokenLogic(ctx context.Context, svcCtx *svc.ServiceContext) *McpTokenLogic {
	return &McpTokenLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// CreateMcpToken 新建；每人最多 mcptoken.MaxActivePerUser 个生效中的令牌（事务里锁住该用户生效中的行再计数）。
func (l *McpTokenLogic) CreateMcpToken(in *canvas.CreateMcpTokenReq) (*canvas.CreateMcpTokenResp, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(in.Name)
	switch n := utf8.RuneCountInString(name); {
	case n == 0:
		return nil, xerr.New(xerr.ParamMissing, "请填写令牌名称")
	case n > mcptoken.MaxNameLen:
		return nil, xerr.Newf(xerr.ParamError, "令牌名称最长 %d 字", mcptoken.MaxNameLen)
	}
	if !mcptoken.ValidTTL(in.ExpiresInDays) {
		return nil, xerr.Param("有效期只能是 7 / 30 / 90 天")
	}
	if !mcptoken.ValidScope(in.Scope) {
		return nil, xerr.Param("权限范围只能是 canvas_rw（画布读写）或 canvas_rw_generate（画布读写 + 生成）")
	}
	var (
		plain string
		row   *model.McpToken
	)
	create := func(ctx context.Context, tx *repo.Repo) error {
		n, err := tx.McpToken.CountActiveForUpdate(ctx, in.UserId)
		if err != nil {
			return err
		}
		if n >= mcptoken.MaxActivePerUser {
			return xerr.NewReason(xerr.Conflict, ReasonMcpTokenLimit,
				"最多同时保留 10 个生效中的 MCP 令牌，请先吊销不用的令牌")
		}
		for attempt := 0; ; attempt++ {
			p, hash, display, err := mcptoken.Generate()
			if err != nil {
				return err
			}
			row, err = tx.McpToken.Insert(ctx, &model.McpToken{
				UserID: in.UserId, Name: name, TokenPrefix: display, TokenHash: hash, Scope: in.Scope, CreatedIP: in.ClientIp,
			}, in.ExpiresInDays)
			if err != nil && repo.IsDuplicate(err) && attempt < 2 { // 哈希撞唯一键：理论上不会发生，换一枚重试
				continue
			}
			if err != nil {
				return err
			}
			plain = p
			return nil
		}
	}
	var err error
	// 同一用户并发创建时 FOR UPDATE 的间隙锁可能互相死锁（InnoDB 回滚其一）：整笔重试，最多 3 次
	for attempt := 0; attempt < 3; attempt++ {
		if err = l.svcCtx.Repo.RunInTx(l.ctx, create); !isDeadlock(err) {
			break
		}
	}
	if err != nil {
		if _, _, ok := xerr.Parse(err); ok {
			return nil, err
		}
		l.Errorf("canvas mcp token: create user=%d: %v", in.UserId, err)
		return nil, xerr.New(xerr.ServerError, "创建 MCP 令牌失败，请稍后重试")
	}
	if row == nil {
		return nil, xerr.New(xerr.ServerError, "创建 MCP 令牌失败，请稍后重试")
	}
	l.Infof("canvas mcp token: created id=%d user=%d prefix=%s scope=%s days=%d ip=%s", row.ID, row.UserID, row.TokenPrefix, row.Scope, in.ExpiresInDays, in.ClientIp)
	return &canvas.CreateMcpTokenResp{Token: mcpTokenInfo(row), PlainToken: plain}, nil
}

// ListMcpTokens 未吊销的令牌（含已过期），新的在前。
func (l *McpTokenLogic) ListMcpTokens(in *canvas.ListMcpTokensReq) (*canvas.ListMcpTokensResp, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	rows, err := l.svcCtx.Repo.McpToken.ListLive(l.ctx, in.UserId, mcpTokenListLimit)
	if err != nil {
		l.Errorf("canvas mcp token: list user=%d: %v", in.UserId, err)
		return nil, xerr.New(xerr.ServerError, "查询 MCP 令牌失败，请稍后重试")
	}
	out := &canvas.ListMcpTokensResp{Tokens: make([]*canvas.McpTokenInfo, 0, len(rows)), MaxActive: mcptoken.MaxActivePerUser}
	for i := range rows {
		out.Tokens = append(out.Tokens, mcpTokenInfo(&rows[i]))
		if rows[i].Alive {
			out.ActiveCount++
		}
	}
	return out, nil
}

// RevokeMcpToken 吊销（幂等：已吊销的再吊销照常返回）；他人的 / 不存在 → NotFound（不区分）。
func (l *McpTokenLogic) RevokeMcpToken(in *canvas.RevokeMcpTokenReq) (*canvas.RevokeMcpTokenResp, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	if in.Id <= 0 {
		return nil, xerr.Param("令牌 id 不正确")
	}
	if _, err := l.svcCtx.Repo.McpToken.Revoke(l.ctx, in.UserId, in.Id); err != nil {
		l.Errorf("canvas mcp token: revoke id=%d user=%d: %v", in.Id, in.UserId, err)
		return nil, xerr.New(xerr.ServerError, "吊销失败，请稍后重试")
	}
	row, err := l.svcCtx.Repo.McpToken.Get(l.ctx, in.UserId, in.Id)
	if err != nil {
		l.Errorf("canvas mcp token: revoke reload id=%d user=%d: %v", in.Id, in.UserId, err)
		return nil, xerr.New(xerr.ServerError, "吊销失败，请稍后重试")
	}
	if row == nil {
		return nil, xerr.NotFoundf("令牌")
	}
	l.Infof("canvas mcp token: revoked id=%d user=%d prefix=%s", row.ID, row.UserID, row.TokenPrefix)
	return &canvas.RevokeMcpTokenResp{TokenHash: row.TokenHash}, nil
}

// VerifyMcpToken 按哈希校验；成功时记最后使用时间与 IP（写失败只记日志，不影响鉴权）。
func (l *McpTokenLogic) VerifyMcpToken(in *canvas.VerifyMcpTokenReq) (*canvas.VerifyMcpTokenResp, error) {
	if !isHexSHA256(in.TokenHash) {
		return nil, xerr.NewReason(xerr.Unauthorized, ReasonMcpTokenInvalid, "MCP 令牌无效或已吊销")
	}
	row, err := l.svcCtx.Repo.McpToken.GetByHash(l.ctx, in.TokenHash)
	if err != nil {
		l.Errorf("canvas mcp token: verify: %v", err)
		return nil, xerr.New(xerr.ServerError, "令牌校验失败，请稍后重试")
	}
	switch {
	case row == nil || row.RevokedAt.Valid:
		return nil, xerr.NewReason(xerr.Unauthorized, ReasonMcpTokenInvalid, "MCP 令牌无效或已吊销")
	case !row.Alive:
		return nil, xerr.NewReason(xerr.Unauthorized, ReasonMcpTokenExpired, "MCP 令牌已过期，请在画布页重新生成")
	case !row.UserStatus.Valid || row.UserStatus.Int64 != 1:
		l.Infof("canvas mcp token: 拒绝 id=%d user=%d（check=mcp_token.user_status status=%v 判据=afv_app_user.status≠1 或用户不存在）", row.ID, row.UserID, row.UserStatus)
		return nil, xerr.NewReason(xerr.Unauthorized, ReasonMcpTokenUserDisabled, "账号已被禁用，MCP 令牌不可用")
	}
	if err := l.svcCtx.Repo.McpToken.Touch(l.ctx, row.ID, in.ClientIp); err != nil {
		l.Errorf("canvas mcp token: touch id=%d: %v", row.ID, err)
	}
	return &canvas.VerifyMcpTokenResp{TokenId: row.ID, UserId: row.UserID, Scope: row.Scope, ExpiresAt: ms(row.ExpiresAt)}, nil
}

func mcpTokenInfo(r *model.McpToken) *canvas.McpTokenInfo {
	out := &canvas.McpTokenInfo{
		Id: r.ID, Name: r.Name, TokenPrefix: r.TokenPrefix, Scope: r.Scope,
		ExpiresAt: ms(r.ExpiresAt), LastUsedIp: r.LastUsedIP, CreatedAt: ms(r.CreatedAt), Expired: !r.Alive,
	}
	if r.LastUsedAt.Valid {
		out.LastUsedAt = ms(r.LastUsedAt.Time)
	}
	return out
}

// isDeadlock MySQL 1213（死锁回滚）/ 1205（锁等待超时）。
func isDeadlock(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && (me.Number == 1213 || me.Number == 1205)
}

func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
