package guide

import (
	"encoding/json"
	"testing"
)

func TestReviewDraftPreservesPersonalNotesAndNeverPublishes(t *testing.T) {
	out := requestRich(t, richFixture(), "POST", "/review-draft", `{"show_id":"1","score":2,"notes":"后排有些台词听不清，我对剧情不太满意。"}`)
	var result struct {
		Title, Content, Mode string
		Score                int
		Published            bool
	}
	json.Unmarshal(out.Body.Bytes(), &result)
	if out.Code != 200 || result.Content != "后排有些台词听不清，我对剧情不太满意。" || result.Score != 2 || result.Published || result.Mode != "notes" {
		t.Fatal(out.Body.String())
	}
}
func TestReviewDraftRequiresUsersScoreAndOwnNotes(t *testing.T) {
	for _, body := range []string{`{"show_id":"1","score":0,"notes":"很不错"}`, `{"show_id":"1","score":5,"notes":""}`, `{"show_id":"bad","score":5,"notes":"很不错"}`} {
		if out := requestRich(t, richFixture(), "POST", "/review-draft", body); out.Code != 400 {
			t.Fatal(out.Body.String())
		}
	}
}
