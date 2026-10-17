package model

import (
	"reflect"
	"testing"
)

// 图片表的成品归属：动作迁移与数字人口播是视频，海报与长图是图片。
// 后台列表的 media 过滤与响应字段都依赖这张映射，改动要同时看两处调用方。
func TestMediaOfKind(t *testing.T) {
	cases := map[string]string{
		KindPoster:         MediaImage,
		KindLongImage:      MediaImage,
		KindActionTransfer: MediaVideo,
		KindVoiceover:      MediaVideo,
	}
	for kind, want := range cases {
		if got := MediaOf(kind); got != want {
			t.Errorf("MediaOf(%q) = %q, want %q", kind, got, want)
		}
	}
}

func TestMediaKinds(t *testing.T) {
	if got := MediaKinds(MediaImage); !reflect.DeepEqual(got, []string{KindPoster, KindLongImage}) {
		t.Errorf("image kinds = %v", got)
	}
	if got := MediaKinds(MediaVideo); !reflect.DeepEqual(got, []string{KindActionTransfer, KindVoiceover}) {
		t.Errorf("video kinds = %v", got)
	}
	if got := MediaKinds(""); got != nil {
		t.Errorf("empty media must not restrict, got %v", got)
	}
}
