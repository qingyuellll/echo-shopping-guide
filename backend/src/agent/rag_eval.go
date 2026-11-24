package agent

import (
	"context"
	"strings"
	"time"
)

// RagCase 是检索评估的一条标注：一个问题，以及它必须命中哪些条目。
// 用标题做黄金标注而不是 id，是为了让标注文件可读、可手改。
type RagCase struct {
	Query string   `json:"query"`
	Gold  []string `json:"gold"`
}

// DefaultRagCases 刻意混入两类问题：
//   - 词面能对上的（「迟到」「秒杀」），基线就该答对；
//   - 只有口语说法能对上的（「轮椅」「带孩子」「冷吗」），
//     单靠原标题正文匹配必然漏，才能看出接别名与向量召回的价值。
func DefaultRagCases() []RagCase {
	return []RagCase{
		{Query: "带孩子看演出有身高要求吗", Gold: []string{"儿童与身高限制"}},
		{Query: "迟到了还能进场吗", Gold: []string{"入场时间与迟到"}},
		{Query: "演出中途能拍照吗", Gold: []string{"观演礼仪"}},
		{Query: "这个票能退吗", Gold: []string{"退改规则怎么读"}},
		{Query: "买前排还是看台", Gold: []string{"选座：前排还是看台"}},
		{Query: "三个人怎么坐一起", Gold: []string{"连座怎么提高成功率"}},
		{Query: "剧场停车方便吗", Gold: []string{"交通与到场缓冲"}},
		{Query: "外语剧有字幕吗", Gold: []string{"字幕与语言"}},
		{Query: "坐轮椅能进场吗", Gold: []string{"无障碍与协助"}},
		{Query: "秒杀票有什么限制", Gold: []string{"秒杀票的注意点"}},
		{Query: "散场后还赶得上末班车吗", Gold: []string{"散场与返程"}},
		{Query: "剧场里冷吗要带外套吗", Gold: []string{"天气与着装"}},
		// 多主题长句：一口气问好几件事时，先答他真正关心的那件。
		// 这类问题纯向量容易把「停车」「拍照」这类附带诉求排到第一。
		{Query: "周六带孩子去看话剧，票能退吗，附近停车方便吗", Gold: []string{"退改规则怎么读"}},
		{Query: "第一次看演出，要提前多久到，中途能拍照吗", Gold: []string{"入场时间与迟到"}},
		{Query: "外语剧听不懂，字幕清楚吗，坐哪一排比较好", Gold: []string{"字幕与语言"}},
	}
}

// RagStrategyResult 是某一种检索策略的成绩。
type RagStrategyResult struct {
	Strategy string  `json:"strategy"`
	Recall   float64 `json:"recall_at_k"` // 命中率：gold 条目出现在前 k 条的问题占比
	MRR      float64 `json:"mrr_at_k"`    // 平均倒数排名：命中得越靠前分越高
	AvgMS    float64 `json:"avg_ms"`
	Misses   int     `json:"misses"`
}

// RagMismatch 记录「基线没中、混合策略中了」的例子，
// 让报告不只给结论，还能指出差异来自哪几条问题。
type RagMismatch struct {
	Query    string `json:"query"`
	Gold     string `json:"gold"`
	Baseline string `json:"baseline"` // 基线首条命中，空表示一条都没召回
	Hybrid   string `json:"hybrid"`
}

// RagReport 是三种策略的对照结果：纯关键词、纯向量、向量召回 + 重排。
type RagReport struct {
	Embedder string              `json:"embedder"`
	K        int                 `json:"k"`
	Cases    int                 `json:"cases"`
	Results  []RagStrategyResult `json:"results"`
	Examples []RagMismatch       `json:"examples,omitempty"`
}

// EvaluateRAG 在固定标注集上跑三种策略。
//
// 三种策略共用同一批用例与同一个 k，否则指标不可比；
// index 为 nil 时只跑关键词基线（未启用检索的部署也能出数）。
func EvaluateRAG(ctx context.Context, index *KnowledgeIndex, cases []RagCase, k int) RagReport {
	if len(cases) == 0 {
		cases = DefaultRagCases()
	}
	if k <= 0 || k > 5 {
		k = 3
	}
	report := RagReport{K: k, Cases: len(cases), Embedder: "keyword-only"}
	if index != nil {
		report.Embedder = index.EmbedderName()
	}

	type strategy struct {
		name string
		run  func(ctx context.Context, query string) []KnowledgeHit
	}
	strategies := []strategy{
		{name: "keyword", run: func(_ context.Context, query string) []KnowledgeHit {
			return searchKnowledgeKeyword(query, k)
		}},
	}
	if index != nil {
		strategies = append(strategies,
			strategy{name: "vector", run: func(ctx context.Context, query string) []KnowledgeHit {
				return index.Retrieve(ctx, query, k)
			}},
			strategy{name: "hybrid+rerank", run: func(ctx context.Context, query string) []KnowledgeHit {
				return index.Search(ctx, query, k)
			}},
		)
	}

	// 先分别跑，把每条问题的结果留在内存里，最后算指标 + 找差异例子。
	type outcome struct {
		rank     int // 首个命中位置，0 表示没中
		topTitle string
	}
	table := map[string][]outcome{}
	for _, item := range strategies {
		results := make([]outcome, 0, len(cases))
		var totalMS float64
		for _, test := range cases {
			started := time.Now()
			hits := item.run(ctx, test.Query)
			totalMS += float64(time.Since(started).Microseconds()) / 1000
			entry := outcome{}
			if len(hits) > 0 {
				entry.topTitle = hits[0].Title
			}
			for position, hit := range hits {
				if matchesGold(hit.Title, test.Gold) {
					entry.rank = position + 1
					break
				}
			}
			results = append(results, entry)
		}

		hits, mrr, misses := 0, 0.0, 0
		for _, entry := range results {
			if entry.rank > 0 {
				hits++
				mrr += 1 / float64(entry.rank)
			} else {
				misses++
			}
		}
		report.Results = append(report.Results, RagStrategyResult{
			Strategy: item.name,
			Recall:   round2(float64(hits) / float64(len(cases))),
			MRR:      round2(mrr / float64(len(cases))),
			AvgMS:    round2(totalMS / float64(len(cases))),
			Misses:   misses,
		})
		table[item.name] = results
	}

	// 差异例子：基线漏掉但混合策略命中的问题，最多列 5 条。
	baseline, hybrid := table["keyword"], table["hybrid+rerank"]
	for index, entry := range baseline {
		if len(report.Examples) >= 5 {
			break
		}
		if entry.rank > 0 || index >= len(hybrid) || hybrid[index].rank == 0 {
			continue
		}
		report.Examples = append(report.Examples, RagMismatch{
			Query:    cases[index].Query,
			Gold:     strings.Join(cases[index].Gold, "、"),
			Baseline: entry.topTitle,
			Hybrid:   hybrid[index].topTitle,
		})
	}
	return report
}

func matchesGold(title string, gold []string) bool {
	for _, expected := range gold {
		if strings.TrimSpace(expected) == "" {
			continue
		}
		if strings.Contains(title, expected) || strings.Contains(expected, title) {
			return true
		}
	}
	return false
}
