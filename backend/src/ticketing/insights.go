package ticketing

import (
	"context"
	"net/url"
	"strconv"
	"strings"
)

type DimensionStat struct {
	Average float64 `json:"average"`
	Count   int     `json:"count"`
}
type ReviewTopic struct {
	Name   string   `json:"name"`
	Count  int      `json:"count"`
	Quotes []Review `json:"quotes"`
}
type ReviewInsights struct {
	Positive   []ReviewTopic            `json:"positive"`
	Critical   []ReviewTopic            `json:"critical"`
	Dimensions map[string]DimensionStat `json:"dimensions"`
	SeatAreas  []string                 `json:"seat_areas"`
}

func (c *Client) enrichReviews(ctx context.Context, showID int64, result ReviewReport) ReviewReport {
	var envelope apiEnvelope[struct {
		Items []struct {
			ReviewID   wireString     `json:"reviewId"`
			Purchased  bool           `json:"purchased"`
			Verified   bool           `json:"verified"`
			SeatArea   string         `json:"seatArea"`
			Dimensions map[string]int `json:"dimensions"`
		} `json:"items"`
	}]
	if err := c.getJSON(ctx, "/show-review/guide-insights", url.Values{"showId": {strconv.FormatInt(showID, 10)}}, &envelope); err != nil || !envelope.Success {
		result.Warnings = append(result.Warnings, "购票凭据与分项评分暂未读取；历史评测可能没有这些信息")
	} else {
		for _, meta := range envelope.Data.Items {
			for i := range result.Items {
				if strconv.FormatInt(result.Items[i].ID, 10) == string(meta.ReviewID) {
					result.Items[i].Purchased = meta.Purchased
					result.Items[i].Verified = meta.Verified
					result.Items[i].EvidenceKnown = true
					result.Items[i].SeatArea = meta.SeatArea
					result.Items[i].Dimensions = meta.Dimensions
				}
			}
		}
	}
	return result
}

// Topics describe what actual reviews mention, rather than inventing a consensus.
func reviewInsights(items []Review) ReviewInsights {
	out := ReviewInsights{Positive: []ReviewTopic{}, Critical: []ReviewTopic{}, Dimensions: map[string]DimensionStat{}, SeatAreas: []string{}}
	names := []struct {
		key, name string
		words     []string
	}{
		{"story", "剧情", []string{"剧情", "故事", "叙事", "剧本"}},
		{"performance", "表演", []string{"表演", "演员", "演技", "唱功", "演唱"}},
		{"sound", "音效", []string{"音效", "音响", "声音", "收音"}},
		{"view", "视野", []string{"视野", "遮挡", "座位", "前排", "后排"}},
		{"value", "性价比", []string{"性价比", "票价", "值得", "划算"}},
	}
	areas := map[string]bool{}
	for _, r := range items {
		if r.SeatArea != "" && !areas[r.SeatArea] {
			areas[r.SeatArea] = true
			out.SeatAreas = append(out.SeatAreas, r.SeatArea)
		}
		for _, n := range names {
			if v := r.Dimensions[n.key]; v >= 1 && v <= 5 {
				s := out.Dimensions[n.key]
				s.Average += float64(v)
				s.Count++
				out.Dimensions[n.key] = s
			}
		}
	}
	for k, v := range out.Dimensions {
		v.Average /= float64(v.Count)
		out.Dimensions[k] = v
	}
	for _, n := range names {
		positive := ReviewTopic{Name: n.name, Quotes: []Review{}}
		critical := ReviewTopic{Name: n.name, Quotes: []Review{}}
		for _, r := range items {
			text := r.Title + " " + r.Content
			matched := false
			for _, word := range n.words {
				if strings.Contains(text, word) {
					matched = true
					break
				}
			}
			if !matched || r.Content == "" {
				continue
			}
			if r.Score >= 4 && r.Score <= 5 {
				positive.Count++
				if len(positive.Quotes) < 2 {
					positive.Quotes = append(positive.Quotes, r)
				}
			} else if r.Score >= 1 && r.Score <= 3 {
				critical.Count++
				if len(critical.Quotes) < 2 {
					critical.Quotes = append(critical.Quotes, r)
				}
			}
		}
		if positive.Count > 0 {
			out.Positive = append(out.Positive, positive)
		}
		if critical.Count > 0 {
			out.Critical = append(out.Critical, critical)
		}
	}
	return out
}
