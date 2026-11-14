package core

import "time"

// cst 自然日按北京时间切（FixedZone 不依赖容器里的 tzdata）。
var cst = time.FixedZone("CST", 8*3600)
