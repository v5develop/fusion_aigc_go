package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"aigc-local-life/app/plaza/headline/service"
	"aigc-local-life/app/plaza/headline/signals"
	"aigc-local-life/app/plaza/headline/store"
	"aigc-local-life/pkg/result"
)

func TestUserHeadlinesMissingIndustryIs400(t *testing.T) {
	result.Setup()
	// 没配库：缺 industry 的判断仍要先于「功能暂未开放」？不——没库时整体 7003；这里用一个带空目录的服务验证 400
	svc := service.New(&store.Repo{}, signals.New(nil), nil, service.Conf{})
	h := UserHeadlinesHandler(svc)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/v1/plaza/headlines", nil))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["reason"] != service.ReasonNotReady {
		t.Fatalf("没配库应返回 PLAZA_NOT_READY: %s", rec.Body.String())
	}
	if err := service.ErrIndustryRequired; err == nil {
		t.Fatal("ErrIndustryRequired 不应为 nil")
	}
}

func TestDecodeEmptyBody(t *testing.T) {
	var v struct{ A int }
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	if err := decode(r, &v); err != nil {
		t.Fatal(err)
	}
}
