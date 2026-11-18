package httpapi

import (
	"context"
	"net/url"
	"strconv"
	"strings"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/pkg/storage"
)

// urlOwner core.URLOwner 的生产实现：http(s) 源视频 / 参考图是不是本站的、属于当前用户的。
//
// 判据（域名由存储配置推导，不设第三方白名单）：
//  1. 结构干净：http/https、有 host、没有 userinfo、没有反斜杠与编码过的 / \；路径里出现空段、「.」、「..」段一律拒收；
//  2. 本存储签发（storage.Store.Owns：公有桶域名 + 对象前缀，来自 afv_system_config / Storage 配置），或命中 InputURLPrefixes（自有域名）；
//  3. 必须符合上传布局 …/<image|video>/<userID>/<yyyymmdd>/<文件名>（storage.ObjectKey），且 userID 是当前用户；
//     不符合布局的本站地址也拒收（证明不了属于谁）。
func urlOwner(st storage.Store, prefixes []string) core.URLOwner {
	if st == nil && len(prefixes) == 0 {
		return nil
	}
	return func(_ context.Context, userID int64, raw string) bool {
		u, ok := cleanURL(raw)
		if !ok {
			return false
		}
		own := st != nil && st.Owns(raw)
		if !own {
			own = matchPrefix(prefixes, u)
		}
		return own && uploadDirOwned(u.Path, userID)
	}
}

func cleanURL(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return nil, false
	}
	if strings.Contains(raw, "\\") {
		return nil, false
	}
	if esc := strings.ToLower(u.EscapedPath()); strings.Contains(esc, "%2f") || strings.Contains(esc, "%5c") {
		return nil, false
	}
	segs := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	for _, sg := range segs {
		if sg == "" || sg == "." || sg == ".." {
			return nil, false
		}
	}
	return u, true
}

// matchPrefix scheme + host 全等，路径按段前缀。
func matchPrefix(prefixes []string, u *url.URL) bool {
	for _, p := range prefixes {
		pu, err := url.Parse(strings.TrimSpace(p))
		if err != nil || pu.Host == "" {
			continue
		}
		if !strings.EqualFold(pu.Scheme, u.Scheme) || !strings.EqualFold(pu.Host, u.Host) {
			continue
		}
		pp := strings.TrimRight(pu.Path, "/")
		if pp == "" || u.Path == pp || strings.HasPrefix(u.Path, pp+"/") {
			return true
		}
	}
	return false
}

// uploadDirOwned 路径必须以下面两种上传布局之一结尾，且路径里的用户 ID 等于当前用户；不符合布局一律 false（证明不了属于谁）：
//   - Go 上传（storage.ObjectKey）：…/<image|video>/<userID>/<8 位日期>/<文件名>
//   - Java 上传（直传 / creaft v1，fusion-aigc ObjectKeyLayout）：…/<images|videos|creaft-v1>/u<userID>/<8 位日期>/<文件名>
//     ——「我的模特」模特库、智能体附件等经 Java 上传的图走这种布局，同样在键里写明了归属。
func uploadDirOwned(path string, userID int64) bool {
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	n := len(segs)
	if n < 4 {
		return false
	}
	kind, uidSeg, date, file := segs[n-4], segs[n-3], segs[n-2], segs[n-1]
	if len(date) != 8 || !digits(date) || file == "" {
		return false
	}
	switch {
	case kind == storage.KindImage || kind == storage.KindVideo:
		// Go 布局：用户 ID 是纯数字段
	case javaUploadDirs[kind] && strings.HasPrefix(uidSeg, "u"):
		uidSeg = uidSeg[1:] // Java 布局：u<userID>
	default:
		return false
	}
	if !digits(uidSeg) {
		return false
	}
	uid, err := strconv.ParseInt(uidSeg, 10, 64)
	return err == nil && uid == userID
}

// javaUploadDirs Java 上传布局的目录段（fusion-aigc ObjectKeyLayout：直传 images / videos，creaft v1 STS 直传 creaft-v1）。
var javaUploadDirs = map[string]bool{"images": true, "videos": true, "creaft-v1": true}

func digits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}
