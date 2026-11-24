package ticketing

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// GlobalReviews includes public review posts whose associated show is no longer listed.
func (c *Client) GlobalReviews(ctx context.Context, refresh bool) (ReviewReport, error) {
	c.feedMu.Lock()
	defer c.feedMu.Unlock()
	if !refresh && c.feedSnapshot != nil && time.Now().Before(c.feedExpires) {
		return *c.feedSnapshot, nil
	}
	result := ReviewReport{Items: []Review{}, Positive: []Review{}, Critical: []Review{}, Warnings: []string{}}
	seen := map[int64]bool{}
	for page := 1; page <= 2000; page++ {
		var envelope struct {
			Success bool        `json:"success"`
			Data    []apiReview `json:"data"`
			Total   wireString  `json:"total"`
		}
		err := c.getJSON(ctx, "/show-review/hot", url.Values{"current": {strconv.Itoa(page)}, "size": {"50"}}, &envelope)
		if err != nil || !envelope.Success {
			if !result.Summary.Available {
				return result, fmt.Errorf("public review feed unavailable")
			}
			result.Warnings = append(result.Warnings, "全站评测暂未读取完整，可刷新重试")
			break
		}
		result.Summary.Available = true
		result.Summary.Total = max(result.Summary.Total, int(parseInt64(envelope.Total)))
		added := 0
		for _, record := range envelope.Data {
			id := parseInt64(record.ID)
			if id <= 0 || seen[id] {
				continue
			}
			seen[id] = true
			added++
			result.Items = append(result.Items, record.toReview())
		}
		if result.Summary.Total > 0 && len(result.Items) >= result.Summary.Total {
			result.Summary.Complete = true
			break
		}
		if len(envelope.Data) == 0 || added == 0 || result.Summary.Total == 0 && len(envelope.Data) < 50 {
			result.Summary.Complete = result.Summary.Total == 0
			break
		}
	}
	result = summarizeReviews(result)
	c.feedSnapshot = &result
	c.feedExpires = time.Now().Add(30 * time.Second)
	if !result.Summary.Complete {
		c.feedExpires = time.Now().Add(5 * time.Second)
	}
	return result, nil
}
