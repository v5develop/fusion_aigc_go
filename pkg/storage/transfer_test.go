package storage

import "testing"

func TestRegionOf(t *testing.T) {
	cases := []struct {
		cfg  ossConfig
		want string
	}{
		{ossConfig{Region: "cn-hangzhou", Endpoint: "https://oss-cn-shanghai.aliyuncs.com", Bucket: "b"}, "cn-hangzhou"},
		{ossConfig{Endpoint: "https://oss-cn-shanghai.aliyuncs.com", Bucket: "b"}, "cn-shanghai"},
		{ossConfig{Endpoint: "https://oss-cn-hangzhou-internal.aliyuncs.com", Bucket: "b"}, "cn-hangzhou-internal"},
		{ossConfig{Endpoint: "https://example.com", Bucket: "b"}, ""},
	}
	for i, c := range cases {
		if got := regionOf(&c.cfg); got != c.want {
			t.Errorf("case %d: got %q want %q", i, got, c.want)
		}
	}
}

func TestStripQueryAndExt(t *testing.T) {
	u := "http://b.oss-cn-shanghai.aliyuncs.com/a/b.png?OSSAccessKeyId=x&Signature=y"
	if got := stripQuery(u); got != "http://b.oss-cn-shanghai.aliyuncs.com/a/b.png" {
		t.Errorf("stripQuery: %s", got)
	}
	if got := ext(u); got != "png" {
		t.Errorf("ext: %s", got)
	}
	if got := ext("http://h/x/y"); got != "bin" {
		t.Errorf("ext fallback: %s", got)
	}
	if got := objectKeyOf(u); got != "a/b.png" {
		t.Errorf("objectKeyOf: %s", got)
	}
}
