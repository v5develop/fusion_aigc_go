package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// 单段精修备注（RedoReq.Note）。
//
// 存放：重做步骤（#u<n>）插入时把备注写进该步骤的 output（{"redoNote":"…"}）。出片步骤在拿到结果前 output 为空、失败也不写 output，
// 所以备注一直留在步骤行里；自动重试步骤建行时原样带过去（carryRedoNote）；出片成功时 output 被结果覆盖，此后不会再重新提交，备注用完即止。
// 不改 job.input（冻结输入，其它段与原方案不受影响），也不需要迁移。
//
// 使用：generateRequest 只对 output 里带备注的步骤，在该段提示词后追加「补充要求：…」；其它步骤的请求逐字节不变。

// RedoNoteMaxRunes 备注上限（字）。
const RedoNoteMaxRunes = 200

const redoNotePrefix = "\n补充要求："

// normalizeRedoNote 去首尾空白、把换行 / 制表等空白压成单个空格（备注只是一句话，不让它改写提示词结构），超长 → 参数错误。
func normalizeRedoNote(raw string) (string, error) {
	n := strings.Join(strings.FieldsFunc(raw, unicode.IsSpace), " ")
	n = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, n)
	if c := len([]rune(n)); c > RedoNoteMaxRunes {
		logx.Infof("replicate: 重做备注拒收 判据=REDO_NOTE_TOO_LONG 字数=%d 上限=%d", c, RedoNoteMaxRunes)
		return "", xerr.Param(fmt.Sprintf("备注最多 %d 个字，请精简后再试", RedoNoteMaxRunes))
	}
	return n, nil
}

func redoNoteOutput(note string) json.RawMessage {
	if note == "" {
		return nil
	}
	b, _ := json.Marshal(map[string]string{"redoNote": note})
	return b
}

func redoNoteOf(out json.RawMessage) string {
	if len(out) == 0 {
		return ""
	}
	var v struct {
		Note string `json:"redoNote"`
	}
	if json.Unmarshal(out, &v) != nil {
		return ""
	}
	return strings.TrimSpace(v.Note)
}

// carryRedoNote 自动重试步骤沿用失败步骤的备注（没有备注 = nil，与原来一致）。
func carryRedoNote(out json.RawMessage) json.RawMessage { return redoNoteOutput(redoNoteOf(out)) }
