package pipeline

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"
)

// 缓存种类（afv_studio_prompt_cache.kind 枚举）。缓存只在批次内复用，唯一键 (batch_id, cache_key, kind)，
// 所以 cache_key 本身不带 kind 前缀；长度 ≤80。
const (
	CacheAnalyze  = "analyze"   // 商拍反推：每个模特一次
	CacheSource   = "source"    // 姿态人物解析：每张人物图一次
	CachePrompt   = "prompt"    // 姿态提示词：每个姿势（+补充说明）一次
	CacheCropPlan = "crop_plan" // 切图方案：每个切图批次一次
)

// SHA1Hex 内容 sha1（小写 hex）。
func SHA1Hex(b []byte) string {
	s := sha1.Sum(b)
	return hex.EncodeToString(s[:])
}

// SHA1String 字符串 sha1。
func SHA1String(s string) string { return SHA1Hex([]byte(s)) }

// AnalyzeKey 商拍反推缓存键 = 模特图 sha1（同批服装固定，同模特多张共用 prompt）。
func AnalyzeKey(modelSHA1 string) string { return strings.ToLower(modelSHA1) }

// SourceKey 姿态人物解析缓存键 = 人物图 sha1。
func SourceKey(imageSHA1 string) string { return strings.ToLower(imageSHA1) }

// PromptKey 姿态提示词缓存键 = 姿势图 sha1；补充说明不同则提示词不同，所以非空 notes 追加其 sha1 前 16 位（总长 57）。
func PromptKey(poseSHA1, notes string) string {
	k := strings.ToLower(poseSHA1)
	if n := strings.TrimSpace(notes); n != "" {
		k += ":" + SHA1String(n)[:16]
	}
	return k
}

// CropPlanKey 切图方案缓存键 = 批次号（reverse.CropPlan 一次调用内部已分批合并，整份结果用 CropPlanKey(0)）。
func CropPlanKey(batch int) string { return fmt.Sprintf("batch-%d", batch) }
