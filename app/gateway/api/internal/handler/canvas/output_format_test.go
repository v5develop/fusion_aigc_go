package canvas

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/pkg/jsoncase"

	"github.com/zeromicro/go-zero/rest/pathvar"
)

// 网页发驼峰 outputFormat：经 jsoncase 中间件（白名单含 output_format）后，执行接口按 output_format 读到用户选的格式，
// 不会被静默丢掉、退回缺省 jpeg。
func TestRunReqCamelOutputFormatThroughJsoncase(t *testing.T) {
	for body, want := range map[string]string{
		`{"runId":"r1","kind":"image","modelId":"m1","outputFormat":"png"}`:  "png",
		`{"run_id":"r1","kind":"image","model_id":"m1","output_format":"webp"}`: "webp",
	} {
		var got types.CanvasRunReq
		var perr error
		h := jsoncase.Middleware(func(w http.ResponseWriter, r *http.Request) {
			r = pathvar.WithVars(r, map[string]string{"id": "cv_1", "node_id": "m_1"})
			perr = parseRaw(r, &got)
		})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/canvas/canvases/cv_1/nodes/m_1/run", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "r1")
		h(httptest.NewRecorder(), req)
		if perr != nil {
			t.Fatal(perr)
		}
		if got.OutputFormat != want || got.RunId != "r1" || got.ModelId != "m1" {
			t.Fatalf("%s → %+v，应读到 output_format=%s", body, got, want)
		}
	}
}
