package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckDeleteKey(t *testing.T) {
	ok := []string{"aigc/plaza-template/202611/a.png", "aigc/x"}
	for _, k := range ok {
		if err := checkDeleteKey("aigc", k); err != nil {
			t.Errorf("%q 应放行: %v", k, err)
		}
	}
	bad := []string{"", "aigc", "aigc/", "/aigc/x", "aigc/../etc/passwd", "other/x", "aigcx/y", "aigc//x", "aigc/x?a=1", "aigc/x#f", `aigc\x`}
	for _, k := range bad {
		if err := checkDeleteKey("aigc", k); err == nil {
			t.Errorf("%q 应拒绝", k)
		}
	}
	if checkDeleteKey("", "a/b") == nil || checkDeleteKey("/", "a/b") == nil {
		t.Error("prefix 为空必须拒绝")
	}
}

func TestLocalStoreDelete(t *testing.T) {
	dir := t.TempDir()
	s := &localStore{dir: dir, baseURL: "http://x/files", prefix: "aigc"}
	ctx := context.Background()
	if _, err := s.Put(ctx, "aigc/p/a.png", "image/png", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "aigc/p/a.png"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "aigc/p/a.png")); !os.IsNotExist(err) {
		t.Fatal("文件应已删除")
	}
	if err := s.Delete(ctx, "aigc/p/a.png"); err != nil {
		t.Fatalf("重复删除应幂等: %v", err)
	}
	if err := s.Delete(ctx, "other/p/a.png"); err == nil {
		t.Fatal("prefix 之外必须拒绝")
	}
}

func TestOSSStoreDeleteRejectsOutOfPrefixBeforeAnyIO(t *testing.T) {
	s := &ossStore{prefix: "aigc"} // conn 为 nil：若越过 key 校验去读配置会 panic
	if err := s.Delete(context.Background(), "other/x.png"); err == nil {
		t.Fatal("prefix 之外必须拒绝")
	}
}
