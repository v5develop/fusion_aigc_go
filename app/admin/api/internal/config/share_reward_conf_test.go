package config

import (
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
)

func loadShareReward(t *testing.T, y string) (bool, int, int) {
	t.Helper()
	var c Config
	if err := conf.LoadFromYamlBytes([]byte(y), &c); err != nil {
		t.Fatal(err)
	}
	return c.ShareRewardDefaults()
}

// ReplicateAdmin.ShareReward 缺省 true / 30 / 3；显式 0（不限）与关闭必须原样保留。
func TestShareRewardConfDefaults(t *testing.T) {
	if e, p, l := loadShareReward(t, "Name: x\nPort: 1\n"); !e || p != 30 || l != 3 {
		t.Fatalf("缺省：%v %d %d", e, p, l)
	}
	if e, p, l := loadShareReward(t, "Name: x\nPort: 1\nReplicateAdmin:\n  ShareReward:\n    Enabled: false\n    Points: 50\n    MonthlyLimit: 0\n"); e || p != 50 || l != 0 {
		t.Fatalf("显式值：%v %d %d", e, p, l)
	}
	if e, p, l := loadShareReward(t, "Name: x\nPort: 1\nReplicateAdmin:\n  ShareReward:\n    Points: 10\n"); !e || p != 10 || l != 3 {
		t.Fatalf("部分显式：%v %d %d", e, p, l)
	}
}
