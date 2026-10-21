package mediasvc

import (
	"slices"
	"testing"

	"github.com/nacos-group/nacos-sdk-go/v2/model"
)

const testRID = "tst001-user-abcdefghijklmnopqrstuvwxyz"

// 资源 ID 先到所属实例；所属实例不在线时按哈希顺序兜底；URL 键的顺序不变。
func TestRankSendsRidToItsOwnerFirst(t *testing.T) {
	bases := []string{"http://10.0.0.1:18090", "http://10.0.0.2:18090", "http://10.0.0.3:18090"}
	c := &Client{bases: bases, owners: map[string]string{"tst001": "http://10.0.0.3:18090"}}

	if got := c.rank(testRID); got[0] != "http://10.0.0.3:18090" || len(got) != len(bases) {
		t.Fatalf("所属实例应排第一: %v", got)
	}
	gone := &Client{bases: bases, owners: map[string]string{"zzz999": "http://10.0.0.9:18090"}}
	plain := &Client{bases: bases}
	if !slices.Equal(gone.rank(testRID), plain.rank(testRID)) {
		t.Fatal("所属实例不在线时应回退到哈希顺序")
	}
	url := "https://cdn.example.com/a.mp4"
	if !slices.Equal(c.rank(url), plain.rank(url)) {
		t.Fatal("URL 键的顺序不应受所属映射影响")
	}
}

func TestOwnerOfOnlyReadsWellFormedResourceIDs(t *testing.T) {
	if ownerOf(testRID) != "tst001" {
		t.Fatal("合法资源 ID 应解析出所属实例")
	}
	for _, key := range []string{"https://cdn.example.com/a.mp4", "tst001-user-short", ""} {
		if ownerOf(key) != "" {
			t.Fatalf("不是资源 ID 的键 %q 不应解析出所属实例", key)
		}
	}
}

// 注册元数据里的 instance_id 会进入所属映射；没有 ID 的实例不进映射。
func TestSetInstancesBuildsOwnerMap(t *testing.T) {
	c := &Client{fallback: []string{"http://fallback:18090"}}
	c.setInstances([]model.Instance{
		{Ip: "10.0.0.3", Port: 18090, Healthy: true, Enable: true, Metadata: map[string]string{"instance_id": "tst001"}},
		{Ip: "10.0.0.4", Port: 18090, Healthy: true, Enable: true},
	})
	if c.owners["tst001"] != "http://10.0.0.3:18090" || len(c.owners) != 1 {
		t.Fatalf("所属映射不正确: %v", c.owners)
	}
}
