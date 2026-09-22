// Package doc 接口文档（Swagger），由 make gen-api 从 admin.api 生成 admin.json，不要手改。
package doc

import _ "embed"

//go:embed admin.json
var Spec []byte

// Title 文档页面标题
const Title = "运营后台接口文档"
