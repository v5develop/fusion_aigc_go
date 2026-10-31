package canvas

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"

	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/rest/httpx"
)

// maxBody 保存画布的请求体上限：快照 2MB + 余量（超 2MB 的快照由 canvas-rpc 回 1003）。
const maxBody = 4 << 20

// parseRaw 路径 / 头按 go-zero 规则解析，请求体用 encoding/json 解（go-zero 的 mapping 不支持 interface{} 字段，
// 而 snapshot / params 是前端私有 JSON，需原样透传）。
func parseRaw(r *http.Request, v any) error {
	b, err := readBody(r)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return xerr.Param("请求体不是合法 JSON：" + err.Error())
	}
	// 路径 / 头最后写入：请求体里同名（大小写不敏感）的 id / node_id / key 不得覆盖
	return parsePathHeaders(r, v)
}

// parseSave 保存画布：snapshot 以 json.RawMessage 原样透传（不经 interface{} 解码再编码：大整数不丢精度、
// 不做 HTML 转义膨胀），体积由 logic 按 2MB 拦。
func parseSave(r *http.Request, req *types.CanvasSaveReq) error {
	b, err := readBody(r)
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
	return parsePathHeaders(r, req)
}

// parsePathHeaders 先清空 path / header 绑定的字段（防请求体同名字段残留，如可选的 Idempotency-Key 头缺失时
// 体内的 "key" 顶替），再按 go-zero 规则解析。
func parsePathHeaders(r *http.Request, v any) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Ptr && rv.Elem().Kind() == reflect.Struct {
		rv = rv.Elem()
		for i := 0; i < rv.NumField(); i++ {
			tag := rv.Type().Field(i).Tag
			if _, ok := tag.Lookup("path"); ok || tag.Get("header") != "" {
				rv.Field(i).SetZero()
			}
		}
	}
	if err := httpx.ParsePath(r, v); err != nil {
		return err
	}
	return httpx.ParseHeaders(r, v)
}

func readBody(r *http.Request) ([]byte, error) {
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
