# 接口文档页面的静态资源（Swagger UI 5.32.14）

- `swagger-ui-bundle.js`、`swagger-ui.css`：来自 `org.webjars:swagger-ui:5.32.14`（与 fusion-aigc 的 springdoc 2.9.1 固定的版本一致）。
- 许可证：Apache-2.0，见 `LICENSE.swagger-ui-bundle.txt`。
- 由 `pkg/swaggerui` 用 `go:embed` 打进二进制，运行时不访问外网。

更新版本时：从同一版本的 webjar 取这两个文件，替换后重新 `go build`。
