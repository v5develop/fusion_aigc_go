package taskevent

import (
	"errors"
	"fmt"
	"strconv"

	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/xerr"
)

// FromError 把错误转成事件的失败说明。网关 / Java（业务码）错误只取错误码与类别，
// message 是固定句式——它们的原始响应体可能回显用户提示词，绝不写进事件表；其余本地错误给脱敏后的短文案（≤120 字符）。
func FromError(err error, check string) Fail {
	if err == nil {
		return Fail{Check: check}
	}
	var ge *aigateway.Error
	if errors.As(err, &ge) {
		code := ge.Code
		if code == "" {
			code = "GATEWAY_ERROR"
			if ge.HTTPStatus == 0 {
				code = "GATEWAY_NETWORK"
			}
		}
		code = SanitizeN(code, 64)
		return Fail{Class: "GATEWAY", Code: code, Check: check,
			Message: fmt.Sprintf("网关错误 http=%d code=%s", ge.HTTPStatus, code)}
	}
	if code, _, ok := xerr.Parse(err); ok {
		c := strconv.Itoa(code)
		return Fail{Class: "BUSINESS", Code: c, Check: check, Message: "业务错误 code=" + c}
	}
	return Fail{Class: "LOCAL", Code: "ERROR", Check: check, Message: SanitizeN(err.Error(), maxFailMessageRunes)}
}
