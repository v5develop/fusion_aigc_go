package jsoncase

// 对外只读入口：给运维工具（如 tools/ops/canvas-snapshot-clean）复用与中间件完全相同的白名单与大小写转换，
// 不另抄一份规则（抄的规则会和中间件慢慢走样）。

// IsKnownSnake name 是否在请求侧白名单（known_gen.go 的 knownSnake）里。
func IsKnownSnake(name string) bool { return knownSnake[name] }

// ToCamel snake_case → camelCase，与中间件补 camel 别名时同一函数。
func ToCamel(s string) string { return toCamel(s) }

// ToSnake camelCase → snake_case，与中间件补 snake 别名时同一函数。
func ToSnake(s string) string { return toSnake(s) }
