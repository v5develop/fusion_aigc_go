package billing

import (
	"context"
	"errors"

	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// 下面这几个是各业务服务（笔记 / 数字人 / 图片视频）真正调用的入口，把"计费出了什么错该怎么办"收在一处：
//
//	提交前  Hold    冻结点数。on：失败就拒绝提交；shadow：只算价记日志，出任何错都不拦用户；off：什么都不做
//	出结果  Done    成功 → 确认扣费（多图部分成功传实际张数）
//	失败    Fail    失败 / 超时 → 把冻结退回
//
// 预留单靠 (用户, 请求号) 找回：冻结发生在提交接口，确认 / 退回发生在轮询器（另一个进程，甚至重启之后），
// 所以请求号必须是库里能重新拿到的值——用提交执行面的 task_id（每次重做都不同），
// 图片类用客户端的 X-Request-Id（一条记录只提交一次）。

// TierOf 模型档位 → 计费档次。lite 是本地 GPU，其余都按云端。
func TierOf(model string) string {
	if model == "lite" {
		return TierLite
	}
	return TierCloud
}

// Hold 提交生成前冻结点数。返回 nil 表示这次没有冻结（off / shadow / 0 点），调用方照常往下走。
//
// 冻结成功之后，如果后面的步骤失败了（落库失败、提交执行面失败），调用方要自己调 Fail 退回。
func (s *Service) Hold(ctx context.Context, c Charge) (*Reservation, error) {
	if s == nil || s.conf.Mode == ModeOff {
		return nil, nil
	}
	r, err := s.Freeze(ctx, c)
	if err == nil {
		return r, nil
	}
	if s.conf.Mode == ModeShadow {
		// 影子期的目的就是把"真开了会拦谁、为什么"暴露出来，所以只记不拦
		logx.WithContext(ctx).Errorf("billing shadow: 用户 %d 的 %s 若开启计费会被拒: %v", c.UserID, c.EntryCode, err)
		return nil, nil
	}
	return nil, s.userError(ctx, c, err)
}

// Enough 提交前的快速余额检查（不冻结）。数字人要等配音出来才知道时长，先用最短时长拦一道明显不够的。
func (s *Service) Enough(ctx context.Context, c Charge) error {
	if s == nil || s.conf.Mode != ModeOn {
		return nil
	}
	q, err := s.Quote(ctx, c)
	if err != nil {
		return s.userError(ctx, c, err)
	}
	a, err := s.Balance(ctx, c.UserID)
	if err != nil {
		logx.WithContext(ctx).Errorf("billing: 查用户 %d 余额失败: %v", c.UserID, err)
		return xerr.New(xerr.ServerError, "暂时无法计费，请稍后重试")
	}
	if a.Balance < q.PricePoints {
		return notEnoughErr(&NotEnough{Need: q.PricePoints, Available: a.Balance})
	}
	return nil
}

// Done 生成成功，确认扣费。actual 是实际产出量（多图部分成功传成功的张数），0 表示全额。
// 失败只记错误日志：预留单留在 HELD，到期会被回收退回，不会多扣。
func (s *Service) Done(ctx context.Context, userID int64, requestID string, actual int) {
	if s == nil || s.conf.Mode != ModeOn || requestID == "" {
		return
	}
	r, err := s.Load(ctx, userID, requestID)
	if err != nil {
		logx.WithContext(ctx).Errorf("billing: 找用户 %d 的预留单 %s 失败（成功后未确认）: %v", userID, requestID, err)
		return
	}
	if r == nil {
		return
	}
	if err := s.Confirm(ctx, r, actual); err != nil {
		logx.WithContext(ctx).Errorf("billing: 确认扣费失败 用户 %d 预留单 %s: %v", userID, r.No, err)
	}
}

// Fail 生成失败或超时，退回冻结。
func (s *Service) Fail(ctx context.Context, userID int64, requestID, reason string) {
	if s == nil || s.conf.Mode != ModeOn || requestID == "" {
		return
	}
	r, err := s.Load(ctx, userID, requestID)
	if err != nil {
		logx.WithContext(ctx).Errorf("billing: 找用户 %d 的预留单 %s 失败（失败后未退回）: %v", userID, requestID, err)
		return
	}
	if r == nil {
		return
	}
	if err := s.Refund(ctx, r, reason); err != nil {
		logx.WithContext(ctx).Errorf("billing: 退回失败 用户 %d 预留单 %s: %v", userID, r.No, err)
	}
}

func notEnoughErr(e *NotEnough) error {
	return xerr.Newf(xerr.InsufficientPoints, "点数不足，还差 %d 点", e.Missing())
}

// userError 把计费错误翻译成用户能看懂的业务错误；内部原因写日志，不暴露给用户。
func (s *Service) userError(ctx context.Context, c Charge, err error) error {
	var ne *NotEnough
	switch {
	case errors.As(err, &ne):
		return notEnoughErr(ne)
	case errors.Is(err, ErrNoFeature):
		return xerr.New(xerr.Forbidden, "暂未开通该功能")
	}
	// 缺计费配置、命中未实现的计价规则、库异常：都是我们这边的问题，拦下并报警，别放行成免费
	logx.WithContext(ctx).Errorf("billing: 用户 %d 的 %s 无法计费: %v", c.UserID, c.EntryCode, err)
	return xerr.New(xerr.ServerError, "暂时无法计费，请稍后重试")
}
