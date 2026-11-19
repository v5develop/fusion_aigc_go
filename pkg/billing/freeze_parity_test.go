package billing

import "testing"

// 冻结公式 Go / Java 一致性的固定向量（边界：token 公式的档位门、token 下 quality / per_ref / aux 照样校验、
// "token_model": null 退回表公式、整数字段严格类型、文本 / 对象字段类型、currency 去空白）。
// Java 侧 ActualCostFreezeCapParityTest 逐条相同（两侧各自钉死，不跨仓共享文件）。汇率 7.2、加成 1.3。
// want：-1 = 整份非法（按价目表冻结），0 = 合法但不封顶，> 0 = 每张上限点数。
func TestFreezeCapParityVectors(t *testing.T) {
	rate, markup := rat("7.2"), rat("1.3")
	cases := []struct {
		name      string
		json      string
		res       string
		quality   string
		refs      int
		chars     int
		refPixels []int64
		want      int64
	}{
		{"token 基准 1k high 100 字", `{"currency":"USD","default_quality":"high","tier_max_pixels":{"1k":1048576,"2k":4300800,"4k":8294400},"token_model":{"price_per_mtok":{"text_in":"5","image_in":"8","image_out":"32"},"text_chars_per_token":"2","ref_base_tokens":1756,"ref_base_pixels":1048576,"ref_max_pixels":4194304,"rate_factor":"0.35","buffer":"0.15","output_tokens":{"low":{"1k":1991,"2k":4033,"4k":5600},"medium":{"1k":3160,"2k":6400,"4k":8888},"high":{"1k":7113,"2k":14406,"4k":20006}}}}`, "1k", "high", 0, 100, nil, 86},
		{"token 档位不在 tier_max_pixels（output_tokens 有）→ 不封顶", `{"currency":"USD","default_quality":"high","tier_max_pixels":{"1k":1048576,"2k":4300800},"token_model":{"price_per_mtok":{"text_in":"5","image_in":"8","image_out":"32"},"text_chars_per_token":"2","ref_base_tokens":1756,"ref_base_pixels":1048576,"ref_max_pixels":4194304,"rate_factor":"0.35","buffer":"0.15","output_tokens":{"low":{"1k":1991,"2k":4033,"4k":5600},"medium":{"1k":3160,"2k":6400,"4k":8888},"high":{"1k":7113,"2k":14406,"4k":20006}}}}`, "4k", "medium", 0, 300, nil, 0},
		{"token 下 quality 表也校验：负数 → 整份非法", `{"currency":"USD","quality":{"medium":"-1"},"default_quality":"high","tier_max_pixels":{"1k":1048576,"2k":4300800,"4k":8294400},"token_model":{"price_per_mtok":{"text_in":"5","image_in":"8","image_out":"32"},"text_chars_per_token":"2","ref_base_tokens":1756,"ref_base_pixels":1048576,"ref_max_pixels":4194304,"rate_factor":"0.35","buffer":"0.15","output_tokens":{"low":{"1k":1991,"2k":4033,"4k":5600},"medium":{"1k":3160,"2k":6400,"4k":8888},"high":{"1k":7113,"2k":14406,"4k":20006}}}}`, "1k", "high", 0, 100, nil, -1},
		{"token 下 quality 合法 → 仍走 token", `{"currency":"USD","quality":{"high":"0.2"},"default_quality":"high","tier_max_pixels":{"1k":1048576,"2k":4300800,"4k":8294400},"token_model":{"price_per_mtok":{"text_in":"5","image_in":"8","image_out":"32"},"text_chars_per_token":"2","ref_base_tokens":1756,"ref_base_pixels":1048576,"ref_max_pixels":4194304,"rate_factor":"0.35","buffer":"0.15","output_tokens":{"low":{"1k":1991,"2k":4033,"4k":5600},"medium":{"1k":3160,"2k":6400,"4k":8888},"high":{"1k":7113,"2k":14406,"4k":20006}}}}`, "1k", "high", 0, 100, nil, 86},
		{"token 下 per_ref 负数 → 整份非法", `{"currency":"USD","per_ref":"-0.1","default_quality":"high","tier_max_pixels":{"1k":1048576,"2k":4300800,"4k":8294400},"token_model":{"price_per_mtok":{"text_in":"5","image_in":"8","image_out":"32"},"text_chars_per_token":"2","ref_base_tokens":1756,"ref_base_pixels":1048576,"ref_max_pixels":4194304,"rate_factor":"0.35","buffer":"0.15","output_tokens":{"low":{"1k":1991,"2k":4033,"4k":5600},"medium":{"1k":3160,"2k":6400,"4k":8888},"high":{"1k":7113,"2k":14406,"4k":20006}}}}`, "1k", "high", 0, 100, nil, -1},
		{"token 下 aux_per_image 非数字 → 整份非法", `{"currency":"USD","aux_per_image":"abc","default_quality":"high","tier_max_pixels":{"1k":1048576,"2k":4300800,"4k":8294400},"token_model":{"price_per_mtok":{"text_in":"5","image_in":"8","image_out":"32"},"text_chars_per_token":"2","ref_base_tokens":1756,"ref_base_pixels":1048576,"ref_max_pixels":4194304,"rate_factor":"0.35","buffer":"0.15","output_tokens":{"low":{"1k":1991,"2k":4033,"4k":5600},"medium":{"1k":3160,"2k":6400,"4k":8888},"high":{"1k":7113,"2k":14406,"4k":20006}}}}`, "1k", "high", 0, 100, nil, -1},
		{"token_model 为 null → 表公式", `{"currency":"USD","quality":{"low":"0.0687","medium":"0.1061","high":"0.2326"},"default_quality":"medium","base_pixels":1048576,"tier_max_pixels":{"1k":1048576,"2k":4300800,"4k":8294400},"token_model":null,"per_ref":"0.056192","aux_per_image":"0"}`, "1k", "medium", 0, 0, nil, 100},
		{"token_model 为 null 且无 quality → 整份非法", `{"currency":"USD","default_quality":"high","tier_max_pixels":{"1k":1048576,"2k":4300800,"4k":8294400},"token_model":null,"unused":{"price_per_mtok":{"text_in":"5","image_in":"8","image_out":"32"},"text_chars_per_token":"2","ref_base_tokens":1756,"ref_base_pixels":1048576,"ref_max_pixels":4194304,"rate_factor":"0.35","buffer":"0.15","output_tokens":{"low":{"1k":1991,"2k":4033,"4k":5600},"medium":{"1k":3160,"2k":6400,"4k":8888},"high":{"1k":7113,"2k":14406,"4k":20006}}}}`, "1k", "high", 0, 100, nil, -1},
		{"token_model 非对象 → 整份非法", `{"currency":"USD","quality":{"low":"0.0687","medium":"0.1061","high":"0.2326"},"default_quality":"medium","base_pixels":1048576,"tier_max_pixels":{"1k":1048576,"2k":4300800,"4k":8294400},"token_model":5,"per_ref":"0.056192","aux_per_image":"0"}`, "1k", "medium", 0, 0, nil, -1},
		{"currency 带空白小写 → 合法", `{"currency":" usd ","quality":{"low":"0.0687","medium":"0.1061","high":"0.2326"},"default_quality":"medium","base_pixels":1048576,"tier_max_pixels":{"1k":1048576,"2k":4300800,"4k":8294400},"per_ref":"0.056192","aux_per_image":"0"}`, "1k", "medium", 0, 0, nil, 100},
		{"base_pixels 写成字符串 → 整份非法", `{"currency":"USD","quality":{"low":"0.0687","medium":"0.1061","high":"0.2326"},"default_quality":"medium","base_pixels":"1048576","tier_max_pixels":{"1k":1048576,"2k":4300800,"4k":8294400},"per_ref":"0.056192","aux_per_image":"0"}`, "1k", "medium", 0, 0, nil, -1},
		{"base_pixels 写成小数 → 整份非法", `{"currency":"USD","quality":{"low":"0.0687","medium":"0.1061","high":"0.2326"},"default_quality":"medium","base_pixels":1048576.0,"tier_max_pixels":{"1k":1048576,"2k":4300800,"4k":8294400},"per_ref":"0.056192","aux_per_image":"0"}`, "1k", "medium", 0, 0, nil, -1},
		{"tier_max_pixels 值写成字符串 → 整份非法", `{"currency":"USD","quality":{"low":"0.0687","medium":"0.1061","high":"0.2326"},"default_quality":"medium","base_pixels":1048576,"tier_max_pixels":{"1k":"1048576","2k":4300800,"4k":8294400},"per_ref":"0.056192","aux_per_image":"0"}`, "1k", "medium", 0, 0, nil, -1},
		{"tier_max_pixels 值写成小数 → 整份非法", `{"currency":"USD","quality":{"low":"0.0687","medium":"0.1061","high":"0.2326"},"default_quality":"medium","base_pixels":1048576,"tier_max_pixels":{"1k":1048576.5,"2k":4300800,"4k":8294400},"per_ref":"0.056192","aux_per_image":"0"}`, "1k", "medium", 0, 0, nil, -1},
		{"ref_base_tokens 写成字符串 → 整份非法", `{"currency":"USD","default_quality":"high","tier_max_pixels":{"1k":1048576,"2k":4300800,"4k":8294400},"token_model":{"price_per_mtok":{"text_in":"5","image_in":"8","image_out":"32"},"text_chars_per_token":"2","ref_base_tokens":"1756","ref_base_pixels":1048576,"ref_max_pixels":4194304,"rate_factor":"0.35","buffer":"0.15","output_tokens":{"low":{"1k":1991,"2k":4033,"4k":5600},"medium":{"1k":3160,"2k":6400,"4k":8888},"high":{"1k":7113,"2k":14406,"4k":20006}}}}`, "1k", "high", 0, 100, nil, -1},
		{"output_tokens 值写成小数 → 整份非法", `{"currency":"USD","default_quality":"high","tier_max_pixels":{"1k":1048576,"2k":4300800,"4k":8294400},"token_model":{"price_per_mtok":{"text_in":"5","image_in":"8","image_out":"32"},"text_chars_per_token":"2","ref_base_tokens":1756,"ref_base_pixels":1048576,"ref_max_pixels":4194304,"rate_factor":"0.35","buffer":"0.15","output_tokens":{"low":{"1k":1991.0,"2k":4033,"4k":5600},"medium":{"1k":3160,"2k":6400,"4k":8888},"high":{"1k":7113,"2k":14406,"4k":20006}}}}`, "1k", "high", 0, 100, nil, -1},
		{"output_tokens 内层非对象 → 整份非法", `{"currency":"USD","default_quality":"high","tier_max_pixels":{"1k":1048576,"2k":4300800,"4k":8294400},"token_model":{"price_per_mtok":{"text_in":"5","image_in":"8","image_out":"32"},"text_chars_per_token":"2","ref_base_tokens":1756,"ref_base_pixels":1048576,"ref_max_pixels":4194304,"rate_factor":"0.35","buffer":"0.15","output_tokens":{"low":5,"medium":{"1k":3160,"2k":6400,"4k":8888},"high":{"1k":7113,"2k":14406,"4k":20006}}}}`, "1k", "high", 0, 100, nil, -1},
		{"default_quality 非字符串 → 整份非法", `{"currency":"USD","quality":{"low":"0.0687","medium":"0.1061","high":"0.2326"},"default_quality":1,"base_pixels":1048576,"tier_max_pixels":{"1k":1048576,"2k":4300800,"4k":8294400},"per_ref":"0.056192","aux_per_image":"0"}`, "1k", "medium", 0, 0, nil, -1},
		{"quality 写成数组 → 整份非法", `{"currency":"USD","quality":[],"default_quality":"medium","base_pixels":1048576,"tier_max_pixels":{"1k":1048576,"2k":4300800,"4k":8294400},"per_ref":"0.056192","aux_per_image":"0"}`, "1k", "medium", 0, 0, nil, -1},
		{"金额写数字与字符串等价 + 14 张参考图", `{"currency":"USD","quality":{"low":"0.0687","medium":0.1061,"high":"0.2326"},"default_quality":"medium","base_pixels":1048576,"tier_max_pixels":{"1k":1048576,"2k":4300800,"4k":8294400},"per_ref":0.056192,"aux_per_image":"0"}`, "1k", "medium", 14, 0, nil, 836},
		{"token 参考图像素逐张给（2k high 200 字）", `{"currency":"USD","default_quality":"high","tier_max_pixels":{"1k":1048576,"2k":4300800,"4k":8294400},"token_model":{"price_per_mtok":{"text_in":"5","image_in":"8","image_out":"32"},"text_chars_per_token":"2","ref_base_tokens":1756,"ref_base_pixels":1048576,"ref_max_pixels":4194304,"rate_factor":"0.35","buffer":"0.15","output_tokens":{"low":{"1k":1991,"2k":4033,"4k":5600},"medium":{"1k":3160,"2k":6400,"4k":8888},"high":{"1k":7113,"2k":14406,"4k":20006}}}}`, "2k", "high", 0, 200, []int64{1572864, 1572864}, 190},
	}
	for _, c := range cases {
		fc, ok := ParseFreezeCap(c.json)
		if c.want == -1 {
			if ok {
				t.Errorf("%s: 应整份非法，却解析成功", c.name)
			}
			continue
		}
		if !ok {
			t.Errorf("%s: 应合法，却解析失败", c.name)
			continue
		}
		got, capped := fc.UnitCapPoints(c.res, ImageUsage{Quality: c.quality, RefImages: c.refs, PromptChars: c.chars, RefPixels: c.refPixels}, rate, markup)
		if c.want == 0 {
			if capped {
				t.Errorf("%s: 应不封顶，却得 %d", c.name, got)
			}
			continue
		}
		if !capped || got != c.want {
			t.Errorf("%s: got %d capped=%v want %d", c.name, got, capped, c.want)
		}
	}
}
