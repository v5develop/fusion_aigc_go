// Package swaggerui 在网关上提供接口文档页面（Swagger UI）。
//
// 文档由 goctl api swagger 从 .api 文件生成（make gen-api），与路由同源，不会和代码对不上。
// Swagger UI 的静态文件打进二进制，不依赖外网 CDN。
package swaggerui

import (
	_ "embed"
	"fmt"
	"net/http"

	"github.com/zeromicro/go-zero/rest"
)

var (
	//go:embed dist/swagger-ui-bundle.js
	bundleJS []byte
	//go:embed dist/swagger-ui.css
	css []byte
)

// Path 文档页面地址
const Path = "/swagger"

// Register 注册 /swagger（页面）与 /swagger/doc.json（文档）。title 用作浏览器标题。
func Register(server *rest.Server, title string, spec []byte) {
	page := []byte(fmt.Sprintf(pageTpl, title, Path, Path, Path))
	serve := func(contentType string, body []byte) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", contentType)
			_, _ = w.Write(body)
		}
	}
	server.AddRoutes([]rest.Route{
		{Method: http.MethodGet, Path: Path, Handler: serve("text/html; charset=utf-8", page)},
		{Method: http.MethodGet, Path: Path + "/doc.json", Handler: serve("application/json; charset=utf-8", spec)},
		{Method: http.MethodGet, Path: Path + "/swagger-ui-bundle.js", Handler: serve("application/javascript", bundleJS)},
		{Method: http.MethodGet, Path: Path + "/swagger-ui.css", Handler: serve("text/css", css)},
	})
}

const pageTpl = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>%s</title>
<link rel="stylesheet" href="%s/swagger-ui.css">
</head>
<body>
<div id="swagger-ui"></div>
<script src="%s/swagger-ui-bundle.js"></script>
<script>
window.ui = SwaggerUIBundle({
  url: "%s/doc.json",
  dom_id: "#swagger-ui",
  deepLinking: true,
  persistAuthorization: true,   // 填过的 X-User-Id 刷新后还在
  displayRequestDuration: true,
  docExpansion: "list",
  tryItOutEnabled: true
});
</script>
</body>
</html>`
