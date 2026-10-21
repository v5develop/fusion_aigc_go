package media

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"
)

// 同一个键在同样的成员集合上总选同一个 owner，且 owner 一定来自候选集合。
func TestPickOwnerIsStableAndAlwaysAMember(t *testing.T) {
	ids := []string{"aaa111", "bbb222", "ccc333"}
	for i := 0; i < 50; i++ {
		key := fmt.Sprintf("job-%d", i)
		first := pickOwner(ids, key)
		if !slices.Contains(ids, first) {
			t.Fatalf("owner %q 不在候选集合里", first)
		}
		if pickOwner(ids, key) != first {
			t.Fatal("同一个键在同样的成员下应得到同一个 owner")
		}
	}
	if pickOwner(nil, "k") != "" {
		t.Fatal("没有候选时应返回空串")
	}
}

// 成员变化只移动映射到变动实例的键：没选中的实例离开，owner 不变。
func TestPickOwnerMovesOnlyKeysOfTheRemovedMember(t *testing.T) {
	ids := []string{"aaa111", "bbb222", "ccc333"}
	counts := map[string]int{}
	for i := 0; i < 300; i++ {
		key := fmt.Sprintf("key-%d", i)
		owner := pickOwner(ids, key)
		counts[owner]++
		if owner == "ccc333" {
			continue
		}
		// 去掉 ccc333：原来属于其它实例的键必须保持不变
		if moved := pickOwner([]string{"aaa111", "bbb222"}, key); moved != owner {
			t.Fatalf("键 %s 的 owner 从 %s 变成 %s：不应移动", key, owner, moved)
		}
	}
	for _, id := range ids {
		if counts[id] == 0 {
			t.Fatalf("实例 %s 没有分到任何键，哈希分布异常", id)
		}
	}
}

// 没有注册中心：owner 恒为本实例。
func TestOwnerPickerWithoutRegistryIsSelf(t *testing.T) {
	p := &ownerPicker{self: "tst001"}
	if got := p.ownerFor(context.Background(), "anything"); got != "tst001" {
		t.Fatalf("没有注册中心时 owner 应是本实例，got %s", got)
	}
}

// 注册中心失败时沿用上一次的候选；从未成功过则只有本实例。
func TestOwnerPickerKeepsLastKnownMembersWhenRegistryFails(t *testing.T) {
	calls := 0
	p := &ownerPicker{self: "tst001", list: func(context.Context) ([]string, error) {
		calls++
		if calls == 1 {
			return []string{"oth001"}, nil
		}
		return nil, fmt.Errorf("registry down")
	}}
	first := p.candidates(context.Background())
	if !slices.Contains(first, "oth001") {
		t.Fatalf("注册中心的成员应进入候选: %v", first)
	}
	p.at = time.Now().Add(-time.Hour) // 缓存过期，强制再查一次（这次失败）
	if got := p.candidates(context.Background()); !slices.Equal(got, first) {
		t.Fatalf("注册中心失败时应沿用上次候选，got %v want %v", got, first)
	}
	fresh := &ownerPicker{self: "tst001", list: func(context.Context) ([]string, error) { return nil, fmt.Errorf("down") }}
	if got := fresh.candidates(context.Background()); !slices.Equal(got, []string{"tst001"}) {
		t.Fatalf("从未成功过时只有本实例，got %v", got)
	}
}

// 真实注册中心：另一个实例注册的 ID 会进入候选集合（它就是这个键的 owner 候选）。
func TestOwnerCandidatesIncludeLiveInstancesFromRegistry(t *testing.T) {
	addr := nacosAddr(t)
	service := fmt.Sprintf("test-media-owner-%d", time.Now().UnixNano())
	cfg := RegistryConfig{ServerAddr: addr, ServiceName: service, Advertise: "http://127.0.0.1:18995"}
	other := uniqueInstanceID()
	deregister, err := Register(context.Background(), cfg, other, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer deregister()
	waitFor(t, func() bool { return instanceCount(t, addr, service) == 1 }, "另一个实例应当出现在注册中心")

	p := newOwnerPicker(Config{Registry: cfg, InstanceID: "tst001"}, "tst001", new([]func()))
	var got []string
	waitFor(t, func() bool {
		p.at = time.Time{}
		got = p.candidates(context.Background())
		return slices.Contains(got, other)
	}, "候选集合里应包含注册中心的存活实例")
	if !slices.Contains(got, "tst001") {
		t.Fatalf("候选集合应包含本实例: %v", got)
	}
}
