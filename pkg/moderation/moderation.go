// Package moderation 内容审核。
//
// 现有系统的文本审核走网易易盾（afv_moderation_log.provider = yidun），但凭证不在库里，
// 所以这里先提供关键词过滤，词库放在 afv_copywriting_option 的 BLOCKWORD 类型中，运营可维护。
//
// TODO 接入现有易盾通道后：换成调用它，并按现有约定写一条 afv_moderation_log
// （需要凭证与写该表的许可）。Checker 接口不变，替换实现即可。
package moderation

import (
	"context"
	"strings"
)

// Result 审核结果。Passed 为 false 时 Label 说明命中了什么。
type Result struct {
	Passed bool
	Label  string
}

var pass = Result{Passed: true}

type Checker interface {
	// Check 审核一段文本。scene 用于区分入参/出参，便于日志排查。
	Check(ctx context.Context, scene, text string) (Result, error)
}

// Noop 不审核。
type Noop struct{}

func (Noop) Check(context.Context, string, string) (Result, error) { return pass, nil }

// WordSource 提供敏感词列表，由调用方从数据库加载。
type WordSource func(ctx context.Context) ([]string, error)

// Keyword 关键词过滤。词库为空时等同于不审核。
type Keyword struct {
	words WordSource
}

func NewKeyword(words WordSource) *Keyword {
	return &Keyword{words: words}
}

func (k *Keyword) Check(ctx context.Context, _, text string) (Result, error) {
	if strings.TrimSpace(text) == "" {
		return pass, nil
	}
	words, err := k.words(ctx)
	if err != nil {
		return pass, err // 词库读取失败不拦截业务，由调用方记录日志
	}
	lower := strings.ToLower(text)
	for _, w := range words {
		if w == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(w)) {
			return Result{Passed: false, Label: w}, nil
		}
	}
	return pass, nil
}

// New 按模式创建：off 不审核，keyword 关键词过滤。
func New(mode string, words WordSource) Checker {
	switch mode {
	case "off":
		return Noop{}
	default:
		return NewKeyword(words)
	}
}
