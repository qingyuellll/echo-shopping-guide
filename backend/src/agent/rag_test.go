package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// brokenEmbedder 模拟 embedding 接口不可用：索引构建失败时必须退回词面检索。
type brokenEmbedder struct{}

func (brokenEmbedder) Name() string { return "broken" }
func (brokenEmbedder) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	return nil, errors.New("embedding service down")
}

func TestHashEmbeddingIsDeterministic(t *testing.T) {
	embedder := hashEmbedding{dim: 192}
	first, err := embedder.Embed(context.Background(), []string{"轮椅能进吗"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := embedder.Embed(context.Background(), []string{"轮椅能进吗"})
	if err != nil {
		t.Fatal(err)
	}
	if len(first[0]) != 192 || len(second[0]) != 192 {
		t.Fatalf("dimension = %d", len(first[0]))
	}
	for index := range first[0] {
		if first[0][index] != second[0][index] {
			t.Fatal("same text must produce the same vector")
		}
	}
	// 不同文本要落到不同方向，否则召回没有区分度。
	other, _ := embedder.Embed(context.Background(), []string{"停车方便吗"})
	if cosine(first[0], other[0]) > 0.99 {
		t.Fatalf("unrelated queries look identical: %.3f", cosine(first[0], other[0]))
	}
}

func TestKnowledgeSearchFindsColloquialQueries(t *testing.T) {
	index := NewKnowledgeIndex(nil)
	if index.EmbedderName() == "keyword-only" {
		t.Fatal("index should report its embedder")
	}
	cases := map[string]string{
		"坐轮椅能进场吗":    "无障碍与协助",
		"演出中途能拍照吗":   "观演礼仪",
		"剧场里冷吗要带外套吗": "天气与着装",
		"三个人怎么坐一起":   "连座怎么提高成功率",
	}
	for query, expected := range cases {
		hits := index.Search(context.Background(), query, 3)
		if len(hits) == 0 {
			t.Fatalf("%s: no hits", query)
		}
		matched := false
		for _, hit := range hits {
			if strings.Contains(hit.Title, expected) {
				matched = true
			}
		}
		if !matched {
			t.Fatalf("%s: expected %q, got %+v", query, expected, titlesOf(hits))
		}
	}
}

func TestKnowledgeSearchFallsBackToKeyword(t *testing.T) {
	index := NewKnowledgeIndex(brokenEmbedder{})
	hits := index.Search(context.Background(), "迟到了还能进场吗", 3)
	if len(hits) == 0 {
		t.Fatal("broken embedder should not break retrieval")
	}
	if !strings.Contains(hits[0].Title, "入场时间与迟到") {
		t.Fatalf("fallback returned %v", titlesOf(hits))
	}
}

func TestRetrievalKeepsEmptyQueryEmpty(t *testing.T) {
	index := NewKnowledgeIndex(nil)
	if hits := index.Search(context.Background(), "   ", 3); len(hits) != 0 {
		t.Fatalf("blank query should return nothing, got %v", titlesOf(hits))
	}
}

func TestRagEvaluationComparesStrategies(t *testing.T) {
	index := NewKnowledgeIndex(nil)
	report := EvaluateRAG(context.Background(), index, DefaultRagCases(), 3)

	if report.Cases != len(DefaultRagCases()) || report.K != 3 {
		t.Fatalf("report header = %+v", report)
	}
	if len(report.Results) != 3 {
		t.Fatalf("expected three strategies, got %+v", report.Results)
	}
	byName := map[string]RagStrategyResult{}
	for _, result := range report.Results {
		byName[result.Strategy] = result
		if result.AvgMS < 0 {
			t.Fatalf("avg ms = %v", result.AvgMS)
		}
	}
	keyword, ok := byName["keyword"]
	if !ok {
		t.Fatal("missing keyword baseline")
	}
	hybrid, ok := byName["hybrid+rerank"]
	if !ok {
		t.Fatal("missing hybrid strategy")
	}
	// 把口语别名纳入索引之后，混合策略必须比纯词面更好，否则这次改动没有意义。
	if hybrid.Recall < keyword.Recall {
		t.Fatalf("hybrid recall %.2f worse than keyword %.2f", hybrid.Recall, keyword.Recall)
	}
	if hybrid.Recall < 0.9 {
		t.Fatalf("hybrid recall too low: %.2f (%d misses)", hybrid.Recall, hybrid.Misses)
	}
	// 报告要能指出差异来自哪几条问题，而不是只给一个分数。
	if hybrid.Recall > keyword.Recall && len(report.Examples) == 0 {
		t.Fatal("improvement without examples is not auditable")
	}
}

func TestEvaluateRagWithoutIndexRunsBaselineOnly(t *testing.T) {
	report := EvaluateRAG(context.Background(), nil, DefaultRagCases(), 3)
	if len(report.Results) != 1 || report.Results[0].Strategy != "keyword" {
		t.Fatalf("report = %+v", report)
	}
	if report.Embedder != "keyword-only" {
		t.Fatalf("embedder = %q", report.Embedder)
	}
}

func titlesOf(hits []KnowledgeHit) []string {
	out := make([]string, 0, len(hits))
	for _, hit := range hits {
		out = append(out, hit.Title)
	}
	return out
}
