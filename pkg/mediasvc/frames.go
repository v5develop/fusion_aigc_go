package mediasvc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Frame 抽出的一帧（服务端 FrameItem）。
type Frame struct {
	TimestampS float64 `json:"timestamp_s"`
	RID        string  `json:"rid"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`
}

// Frames 按 count 均匀抽帧（POST /v1/mediasvc/frames，count 1–32；服务端只支持均匀抽，不支持指定时间点）。
// 帧落中间产物私有桶；同一 rid + count 得到同一组帧（自动幂等键由入参派生）。路由到 rid 所属实例。
func (c *Client) Frames(ctx context.Context, rid string, count int, idem string) ([]Frame, error) {
	if count < 1 || count > 32 {
		return nil, &Error{Status: http.StatusBadRequest, Code: "invalid_request", Message: "抽帧数量需要 1-32"}
	}
	idem = pick(idem, "frames", rid, count)
	body, _ := json.Marshal(map[string]any{"source": map[string]string{"rid": rid}, "count": count})
	raw, err := c.post(ctx, rid, "/v1/mediasvc/frames", body, "application/json", idem)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Frames []struct {
			TimestampS float64 `json:"timestamp_s"`
			Object     struct {
				RID    string `json:"rid"`
				Width  int    `json:"width"`
				Height int    `json:"height"`
			} `json:"object"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("媒体服务返回无法解析: %w", err)
	}
	out := make([]Frame, 0, len(resp.Frames))
	for _, f := range resp.Frames {
		if f.Object.RID == "" {
			return nil, fmt.Errorf("媒体服务返回的帧缺少 rid")
		}
		out = append(out, Frame{TimestampS: f.TimestampS, RID: f.Object.RID, Width: f.Object.Width, Height: f.Object.Height})
	}
	return out, nil
}
