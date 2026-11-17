package core

// 一键方案建议（CONTRACT §J1）：由分析结果确定性算出，不调模型。

// 建议的 key（只增）。
const (
	SuggestSwapModel   = "swap_model"
	SuggestSwapProduct = "swap_product"
	SuggestSwapGarment = "swap_garment"
	SuggestCleanText   = "clean_text"
)

// maxSuggestions 最多给几条。
const maxSuggestions = 3

// Suggestion 一条建议：Label 是给用户看的中文短句，Desc 是一句说明，Needs 是还缺哪张图（person_image / product_image / garment_image；空 = 不缺图）。
type Suggestion struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Desc  string `json:"desc"`
	Needs string `json:"needs"`
}

// suggestionsOf 按固定顺序（换人 > 换商品 > 换衣服 > 去字幕水印）取命中的前 3 条；分析结果为空返回 nil。
func suggestionsOf(r *AnalysisResult) []Suggestion {
	if r == nil {
		return nil
	}
	var out []Suggestion
	add := func(ok bool, s Suggestion) {
		if ok && len(out) < maxSuggestions {
			out = append(out, s)
		}
	}
	add(len(r.Subjects) > 0 || r.Dims["model"].Present, Suggestion{Key: SuggestSwapModel, Label: "换成我的模特",
		Desc: "把视频里的人换成你的模特，动作和场景保持不变。需要一张人物照片", Needs: "person_image"})
	add(r.Dims["product"].Present, Suggestion{Key: SuggestSwapProduct, Label: "换成我的商品",
		Desc: "把视频里推荐的商品换成你的商品，其余保持不变。需要一张商品图", Needs: "product_image"})
	add(r.Dims["garment"].Present, Suggestion{Key: SuggestSwapGarment, Label: "换一套衣服",
		Desc: "把视频里人物穿的衣服换成你的衣服。需要一张衣服的图", Needs: "garment_image"})
	add(hasOverlayText(r), Suggestion{Key: SuggestCleanText, Label: "去掉字幕和水印",
		Desc: "做出来的视频不带原来的字幕和水印；需要的文字可以由系统重新加上，不用提供图片"})
	return out
}

// hasOverlayText 画面里有字幕 / 水印 / 贴纸文字（价格、标题属于卖点，不算要清理的）。
func hasOverlayText(r *AnalysisResult) bool {
	for _, o := range r.OnScreenText {
		switch o.Kind {
		case "subtitle", "watermark", "sticker":
			return true
		}
	}
	return false
}
