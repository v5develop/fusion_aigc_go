package canvas

import (
	"encoding/json"
	"io"
	"net/http"

	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/rest/httpx"
)

// maxBody 保存画布的请求体上限：快照 2MB + 余量（超 2MB 的快照由 canvas-rpc 回 1003）。
const maxBody = 4 << 20

// parseRaw 路径 / 头按 go-zero 规则解析，请求体用 encoding/json 解（go-zero 的 mapping 不支持 interface{} 字段，
// 而 snapshot / params 是前端私有 JSON，需原样透传）。
func parseRaw(r *http.Request, v any) error {
	b, err := readBody(r, v)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return xerr.Param("请求体不是合法 JSON：" + err.Error())
	}
	return nil
}

// parseSave 保存画布：snapshot 以 json.RawMessage 原样透传（不经 interface{} 解码再编码：大整数不丢精度、
// 不做 HTML 转义膨胀），体积由 logic 按 2MB 拦。
func parseSave(r *http.Request, req *types.CanvasSaveReq) error {
	b, err := readBody(r, req)
	if err != nil {
		return err
	}
	// 外层同名字段比内嵌的浅，encoding/json 只填外层的 Snapshot
	body := struct {
		*types.CanvasSaveReq
		Snapshot json.RawMessage `json:"snapshot"`
	}{CanvasSaveReq: req}
	if err := json.Unmarshal(b, &body); err != nil {
		return xerr.Param("请求体不是合法 JSON：" + err.Error())
	}
	req.Snapshot = nil
	if len(body.Snapshot) > 0 && string(body.Snapshot) != "null" {
		req.Snapshot = body.Snapshot
	}
	return nil
}

func readBody(r *http.Request, v any) ([]byte, error) {
	if err := httpx.ParsePath(r, v); err != nil {
		return nil, err
	}
	if err := httpx.ParseHeaders(r, v); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxBody {
		return nil, xerr.Param("请求体过大（画布内容上限 2MB）")
	}
	if len(b) == 0 {
		return nil, xerr.New(xerr.ParamMissing, "缺少请求体")
	}
	return b, nil
}
