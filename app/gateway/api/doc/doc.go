// Package doc 接口文档（Swagger），由 make gen-api 从 gateway.api 生成 gateway.json，不要手改。
package doc

import _ "embed"

//go:embed gateway.json
var Spec []byte

// Title 文档页面标题
const Title = "App 接口文档"
