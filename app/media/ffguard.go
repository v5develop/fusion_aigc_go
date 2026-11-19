package media

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
)

// ffProtocolWhitelist 输入只许本地文件与管道。
// 输入文件已由下载器落盘；不限制协议时，素材里夹带的 HLS/concat 清单可让 ffmpeg 去读 http://内网、file:///etc/passwd 之类（SSRF / 任意读）。
const ffProtocolWhitelist = "file,pipe"

// guardInputs 在每个 -i 之前插入 -protocol_whitelist（它是输入选项，必须出现在对应 -i 之前才对该输入生效）；
// ffprobe 没有 -i，直接放在最前面。其余参数顺序不动。
func guardInputs(bin string, args []string) []string {
	out := make([]string, 0, len(args)+4)
	if strings.HasPrefix(filepath.Base(bin), "ffprobe") {
		out = append(out, "-protocol_whitelist", ffProtocolWhitelist)
		return append(out, args...)
	}
	for _, a := range args {
		if a == "-i" {
			out = append(out, "-protocol_whitelist", ffProtocolWhitelist)
		}
		out = append(out, a)
	}
	return out
}

// ffCommand 等同 exec.CommandContext，但对 ffmpeg / ffprobe 的输入加协议白名单。
func ffCommand(ctx context.Context, bin string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, bin, guardInputs(bin, args)...)
}
