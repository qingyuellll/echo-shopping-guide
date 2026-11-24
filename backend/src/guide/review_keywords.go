package guide

import (
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// reviewQuote is a short excerpt from a public review that matches the
// visitor's words, so an answer can cite more than the hand-picked
// positive/critical quotes.
type reviewQuote struct {
	Show     string  `json:"show"`
	Author   string  `json:"author"`
	Score    float64 `json:"score"`
	Excerpt  string  `json:"excerpt"`
	ReviewID string  `json:"review_id,string"`
}

// reviewStopword runes: grams containing any of these carry no opinion.
const reviewStopwords = "的了是在有我不也很就都还你个这那为与及等被把从上中里去说想看吗呢吧啊呀哦嘛么很太别点场一次两三很多已经会能要和跟对但可是所以如果就是觉得感觉一下下没有应应该可以让我们他们他她它个位日号年月时分秒钱元价格位子座位"

func isStopGram(gram string) bool {
	for _, r := range gram {
		if !unicode.Is(unicode.Han, r) {
			return true
		}
		if strings.ContainsRune(reviewStopwords, r) {
			return true
		}
	}
	return false
}

type gramCount struct {
	gram    string
	count   int
	reviews int
}

// extractKeywords finds opinions that repeat across reviews. A 2- or 3-character
// run appearing in several reviews is one keyword; shorter runs contained in a
// longer, equally common run are merged into it (意思相近算一个).
func extractKeywords(entries []comparisonEntry, limit int) map[int64][]string {
	out := map[int64][]string{}
	for _, entry := range entries {
		occurrence := map[string]gramCount{}
		reviews := 0
		for _, review := range entry.Reviews.Items {
			content := strings.TrimSpace(review.Content)
			if len([]rune(content)) < 4 {
				continue
			}
			reviews++
			seen := map[string]bool{}
			runes := []rune(content)
			for size := 2; size <= 3; size++ {
				for i := 0; i+size <= len(runes); i++ {
					gram := string(runes[i : i+size])
					if isStopGram(gram) {
						continue
					}
					counter := occurrence[gram]
					counter.gram = gram
					counter.count++
					if !seen[gram] {
						counter.reviews++
						seen[gram] = true
					}
					occurrence[gram] = counter
				}
			}
		}
		if reviews < 2 {
			continue
		}
		candidates := make([]gramCount, 0, len(occurrence))
		for _, counter := range occurrence {
			if counter.count >= 3 && counter.reviews >= 2 {
				candidates = append(candidates, counter)
			}
		}
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].reviews != candidates[j].reviews {
				return candidates[i].reviews > candidates[j].reviews
			}
			if candidates[i].count != candidates[j].count {
				return candidates[i].count > candidates[j].count
			}
			return len([]rune(candidates[i].gram)) > len([]rune(candidates[j].gram))
		})
		kept := make([]gramCount, 0, limit)
		for _, candidate := range candidates {
			merged := false
			for ki := range kept {
				existing := kept[ki]
				longer, shorter := existing.gram, candidate.gram
				if len([]rune(candidate.gram)) > len([]rune(existing.gram)) {
					longer, shorter = candidate.gram, existing.gram
				}
				// Same meaning: one contains the other and neither is much rarer.
				if strings.Contains(longer, shorter) && candidate.count*2 >= existing.count {
					if len([]rune(candidate.gram)) > len([]rune(existing.gram)) && candidate.reviews >= existing.reviews {
						kept[ki].gram = candidate.gram
					}
					merged = true
					break
				}
			}
			if !merged {
				kept = append(kept, candidate)
			}
			if len(kept) >= limit {
				break
			}
		}
		words := make([]string, 0, len(kept))
		for _, counter := range kept {
			words = append(words, counter.gram)
		}
		out[entry.Show.ID] = words
	}
	return out
}

// queryGrams pulls opinion-bearing 2-grams out of the visitor's question.
func queryGrams(query string) []string {
	runes := []rune(strings.TrimSpace(query))
	seen := map[string]bool{}
	grams := []string{}
	for size := 2; size <= 4; size++ {
		for i := 0; i+size <= len(runes); i++ {
			gram := string(runes[i : i+size])
			if isStopGram(gram) || seen[gram] {
				continue
			}
			seen[gram] = true
			grams = append(grams, gram)
		}
	}
	return grams
}

// relatedQuotes cites reviews whose own words overlap the question, so the
// answer quotes reviews that actually talk about what was asked.
func relatedQuotes(entries []comparisonEntry, query string, limit int) map[int64][]reviewQuote {
	grams := queryGrams(query)
	out := map[int64][]reviewQuote{}
	if len(grams) == 0 {
		return out
	}
	type match struct {
		quote reviewQuote
		hits  int
	}
	for _, entry := range entries {
		matches := []match{}
		for _, review := range entry.Reviews.Items {
			content := strings.TrimSpace(review.Content)
			if len([]rune(content)) < 4 {
				continue
			}
			hits := 0
			for _, gram := range grams {
				if strings.Contains(content, gram) {
					hits++
				}
			}
			if hits == 0 {
				continue
			}
			excerpt := []rune(content)
			if len(excerpt) > 110 {
				excerpt = append(excerpt[:110], '…')
			}
			matches = append(matches, match{quote: reviewQuote{
				Show:     entry.Show.Name,
				Author:   review.Author,
				Score:    review.Score,
				Excerpt:  string(excerpt),
				ReviewID: formatID(review.ID),
			}, hits: hits})
		}
		sort.Slice(matches, func(i, j int) bool {
			if matches[i].hits != matches[j].hits {
				return matches[i].hits > matches[j].hits
			}
			return matches[i].quote.Score < matches[j].quote.Score
		})
		for i, item := range matches {
			if i >= limit {
				break
			}
			out[entry.Show.ID] = append(out[entry.Show.ID], item.quote)
		}
	}
	return out
}

func formatID(value int64) string {
	return strconv.FormatInt(value, 10)
}
