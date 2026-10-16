// billing-check 上线前的计费配置检查：把每个会计费的入口 × 档位真的算一遍价，
// 缺场景 / 档位 / 步骤 / 单价、算出 0 点、档位配错的都会报出来。只读，不动账。
//
//	billing-check -dsn 'user:pass@tcp(host:3306)/db?charset=utf8mb4&parseTime=true&loc=Asia%2FShanghai'
//
// 有 ERROR 退出码 1，只有 WARN 退出码 0。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"aigc-life/pkg/billing"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

func main() {
	dsn := flag.String("dsn", "", "MySQL DSN")
	flag.Parse()
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "缺少 -dsn")
		os.Exit(2)
	}
	logx.Disable()
	fs := billing.Audit(context.Background(), sqlx.NewMysql(*dsn))
	for _, f := range fs {
		fmt.Println(f)
	}
	if billing.HasError(fs) {
		fmt.Println("✗ 计费配置有问题，未修复前不能开计费")
		os.Exit(1)
	}
	fmt.Printf("✓ 计费配置检查通过（%d 条提示）\n", len(fs))
}
