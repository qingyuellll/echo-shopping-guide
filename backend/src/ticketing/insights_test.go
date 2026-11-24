package ticketing

import "testing"

func TestInsightsKeepSourcesAndSeparateDimensionSamples(t *testing.T) {
	report := summarizeReviews(ReviewReport{Items: []Review{
		{ID: 1, Score: 5, Content: "剧情节奏流畅，表演很好", Dimensions: map[string]int{"story": 5, "view": 2}, Purchased: true},
		{ID: 2, Score: 4, Content: "剧情很有趣", Dimensions: map[string]int{"story": 3}},
		{ID: 3, Score: 2, Content: "后排视野有遮挡，声音太响", SeatArea: "B区", Verified: true},
		{ID: 4, Score: 5},
	}, Summary: RatingSummary{Complete: true, Available: true}})
	if report.Insights.Dimensions["story"].Count != 2 || report.Insights.Dimensions["story"].Average != 4 || report.Insights.Dimensions["view"].Count != 1 || report.Insights.Dimensions["sound"].Count != 0 {
		t.Fatalf("mixed rating denominators: %+v", report.Insights)
	}
	if len(report.Insights.Positive) == 0 || report.Insights.Positive[0].Count != 2 || report.Insights.Positive[0].Quotes[0].ID != 1 {
		t.Fatalf("missing original evidence: %+v", report.Insights)
	}
	if len(report.Insights.Critical) != 2 || len(report.Insights.SeatAreas) != 1 || report.Insights.SeatAreas[0] != "B区" {
		t.Fatalf("lost disagreement or seat area: %+v", report.Insights)
	}
	if report.Positive[0].Verified || !report.Positive[0].Purchased || !report.Critical[0].Verified {
		t.Fatal("conflated purchase and verification")
	}
}

func TestPureScoresNeverGenerateWrittenOpinion(t *testing.T) {
	insights := reviewInsights([]Review{{ID: 1, Score: 5, Title: "剧情"}, {ID: 2, Score: 1}})
	if len(insights.Positive) != 0 || len(insights.Critical) != 0 {
		t.Fatal("invented a written opinion")
	}
}
