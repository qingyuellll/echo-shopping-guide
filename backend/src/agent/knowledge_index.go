package agent

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/LYP-leo/echo-shopping-guide/backend/src/llm"
)

// Embedder 把文本映射成向量。两套实现，按有没有模型 Key 自动选：
//
//   - apiEmbedding：走 OpenAI 兼容 /embeddings，语义质量最好；
//   - hashEmbedding：字符 n-gram 符号哈希，跨进程确定性、零依赖、离线可跑。
//
// 离线那套不是「假实现」：词面相近的条目确实会落在相近方向，
// 所以召回 → 重排 → 评测整条链路在任何环境都能验证；接上真 embedding
// 只是把召回质量再抬一档，上层代码一行都不用改。
type Embedder interface {
	Name() string
	Embed(ctx context.Context, texts []string) ([][]float64, error)
}

// 检索参数。topN 是召回池，limit 是最终给出条数：
// 先多召回再重排，比直接在向量分上截断更能压住「关键词撞车」的误命中。
const (
	retrievalTopN   = 6
	defaultHitLimit = 3
)

// KnowledgeIndex 是常识库的检索索引：向量召回 + 混合重排。
type KnowledgeIndex struct {
	mu       sync.Mutex
	embedder Embedder
	entries  []KnowledgeHit
	vectors  [][]float64
	built    bool
	buildErr error
	topN     int
}

// NewKnowledgeIndex 构建索引；embedder 为 nil 时用离线哈希向量。
func NewKnowledgeIndex(embedder Embedder) *KnowledgeIndex {
	if embedder == nil {
		embedder = hashEmbedding{dim: 192}
	}
	return &KnowledgeIndex{embedder: embedder, entries: knowledgeBase, topN: retrievalTopN}
}

// EmbedderName 供管理端展示「这次检索用的是哪套向量」。
func (k *KnowledgeIndex) EmbedderName() string {
	if k == nil || k.embedder == nil {
		return "keyword-only"
	}
	return k.embedder.Name()
}

// Size 返回常识库条目数，供管理端展示检索范围。
func (k *KnowledgeIndex) Size() int {
	if k == nil {
		return 0
	}
	return len(k.entries)
}

// Warm 预构建索引。启动时调用一次，避免首个咨询卡在构建上。
func (k *KnowledgeIndex) Warm(ctx context.Context) error {
	if k == nil {
		return nil
	}
	return k.ensure(ctx)
}

// ensure 懒构建，且只尝试一次：embedding 接口挂掉时不能每个请求都重试一遍。
func (k *KnowledgeIndex) ensure(ctx context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.built {
		return k.buildErr
	}
	if len(k.entries) == 0 {
		k.entries = knowledgeBase
	}
	texts := make([]string, len(k.entries))
	for index, entry := range k.entries {
		texts[index] = entry.embeddedText()
	}
	vectors, err := k.embedder.Embed(ctx, texts)
	if err != nil {
		// 记住失败：调用方会退回关键词检索，而不是每次请求都等一次超时。
		k.built, k.buildErr = true, err
		return err
	}
	if len(vectors) != len(k.entries) {
		k.built = true
		k.buildErr = fmt.Errorf("向量数量 %d 与条目数 %d 不一致", len(vectors), len(k.entries))
		return k.buildErr
	}
	k.vectors = vectors
	k.built = true
	return nil
}

// Search 是对外入口：召回 topN，再混合重排，取前 limit 条。
// 任何一步失败都退回纯词面检索——检索质量可以降，但不能因此没有答案。
func (k *KnowledgeIndex) Search(ctx context.Context, query string, limit int) []KnowledgeHit {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil
	}
	if limit <= 0 || limit > 5 {
		limit = defaultHitLimit
	}
	if k == nil {
		return searchKnowledgeKeyword(query, limit)
	}
	candidates, err := k.recall(ctx, query, k.topN)
	if err != nil || len(candidates) == 0 {
		return searchKnowledgeKeyword(query, limit)
	}
	reranked := rerankKnowledge(query, candidates)
	if len(reranked) > limit {
		reranked = reranked[:limit]
	}
	return reranked
}

// Retrieve 是纯向量召回，评估时用来对比「只召回不重排」的效果。
func (k *KnowledgeIndex) Retrieve(ctx context.Context, query string, limit int) []KnowledgeHit {
	if limit <= 0 || limit > 10 {
		limit = retrievalTopN
	}
	candidates, err := k.recall(ctx, query, limit)
	if err != nil {
		return nil
	}
	return candidates
}

// recall 把 query 向量化后与条目向量做余弦，返回相似度最高的若干条。
func (k *KnowledgeIndex) recall(ctx context.Context, query string, limit int) ([]KnowledgeHit, error) {
	if err := k.ensure(ctx); err != nil {
		return nil, err
	}
	vectors, err := k.embedder.Embed(ctx, []string{query})
	if err != nil || len(vectors) == 0 {
		return nil, fmt.Errorf("query 向量化失败: %w", err)
	}

	k.mu.Lock()
	entries := k.entries
	docVectors := k.vectors
	k.mu.Unlock()

	type scored struct {
		entry  KnowledgeHit
		cosine float64
	}
	ranked := make([]scored, 0, len(entries))
	for index, entry := range entries {
		if index >= len(docVectors) {
			break
		}
		similarity := cosine(vectors[0], docVectors[index])
		if similarity <= 0 {
			continue
		}
		ranked = append(ranked, scored{entry: entry, cosine: similarity})
	}
	if len(ranked) == 0 {
		return nil, nil
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].cosine > ranked[j].cosine })
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	out := make([]KnowledgeHit, 0, len(ranked))
	for _, item := range ranked {
		hit := item.entry
		hit.Score = int(math.Round(item.cosine * 100))
		out = append(out, hit)
	}
	return out, nil
}

// rerankKnowledge 是二阶段重排：向量分定候选，词面信号定顺序。
//
// 为什么不能只看向量：长句里「停车方便吗」和「能退票吗」会同时抬高两条候选的
// 余弦，但用户真正要答的是退票。重排用「关键词命中条数」把主诉求顶回第一位。
//
// 这里刻意用绝对计数而不是命中率：一句 30 个字的长问句，命中 2 个关键词
// 已经说明「问的就是这条」，除以句长反而让短问句占便宜。
func rerankKnowledge(query string, candidates []KnowledgeHit) []KnowledgeHit {
	type item struct {
		hit   KnowledgeHit
		score float64
	}
	scored := make([]item, 0, len(candidates))
	for _, candidate := range candidates {
		titleHits, contentHits := lexicalHits(query, candidate)
		titleSignal := math.Min(1, float64(titleHits)/2)
		contentSignal := math.Min(1, float64(contentHits)/4)
		// 向量分与两路词面信号都归一到 0..1 后加权。
		score := 0.5*float64(candidate.Score)/100 + 0.3*titleSignal + 0.2*contentSignal
		hit := candidate
		hit.Score = int(math.Round(score * 100))
		scored = append(scored, item{hit: hit, score: score})
	}
	sort.SliceStable(scored, func(i, j int) bool { return scored[i].score > scored[j].score })
	out := make([]KnowledgeHit, 0, len(scored))
	for _, entry := range scored {
		out = append(out, entry.hit)
	}
	return out
}

// keywordText 是「标题 + 别名」，重排时用它判断「问的是不是这条」。
func (h KnowledgeHit) keywordText() string {
	if len(h.Aliases) == 0 {
		return h.Title
	}
	return h.Title + " " + strings.Join(h.Aliases, " ")
}

// embeddedText 是建索引用到的文本：标题、口语别名、正文。
//
// 别名不是装饰：用户搜「轮椅」「拍照」「带孩子」，正文里未必出现这些词，
// 但它们是真实说法，必须能被检索到。
func (h KnowledgeHit) embeddedText() string {
	parts := []string{h.Title}
	if len(h.Aliases) > 0 {
		parts = append(parts, strings.Join(h.Aliases, " "))
	}
	parts = append(parts, h.Content)
	return strings.Join(parts, "。")
}

// lexicalHits 统计查询的二元组落在「标题 + 别名」和「正文」上的条数。
//
// 标题这一侧带上别名：「能退票」不会出现在「退改规则怎么读」这六个字里，
// 但它确实是这条知识的标准说法，不认别名就会把主诉求判成噪声。
func lexicalHits(query string, entry KnowledgeHit) (title int, content int) {
	grams := bigrams(query)
	if len(grams) == 0 {
		return 0, 0
	}
	headline := entry.keywordText()
	for _, gram := range grams {
		switch {
		case strings.Contains(headline, gram):
			title++
		case strings.Contains(entry.Content, gram):
			content++
		}
	}
	return title, content
}

func cosine(a []float64, b []float64) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for index := range a {
		dot += a[index] * b[index]
		normA += a[index] * a[index]
		normB += b[index] * b[index]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// hashEmbedding 是离线向量器：字符 1~3 gram 做符号哈希投影。
type hashEmbedding struct {
	dim int
}

func (h hashEmbedding) Name() string {
	if h.dim <= 0 {
		return "hash-ngram"
	}
	return fmt.Sprintf("hash-ngram-%d", h.dim)
}

func (h hashEmbedding) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	dim := h.dim
	if dim <= 0 {
		dim = 192
	}
	out := make([][]float64, len(texts))
	for index, text := range texts {
		out[index] = hashVector(text, dim)
	}
	return out, nil
}

// hashVector 用 FNV-1a 把每个 gram 落到固定维度，长短 gram 加权不同：
// 单字权重最高（中文单字本身有信息量），三字词更有区分度但出现更少。
func hashVector(text string, dim int) []float64 {
	vector := make([]float64, dim)
	runes := []rune(strings.ToLower(strings.TrimSpace(text)))
	for n := 1; n <= 3; n++ {
		for start := 0; start+n <= len(runes); start++ {
			gram := string(runes[start : start+n])
			if strings.TrimSpace(gram) == "" {
				continue
			}
			hasher := fnv.New32a()
			_, _ = hasher.Write([]byte(gram))
			sum := hasher.Sum32()
			sign := 1.0
			if sum&1 == 1 {
				// 符号哈希：同一个 gram 永远同号，不同 gram 的碰撞会互相抵消而不是叠加。
				sign = -1
			}
			vector[int(sum%uint32(dim))] += sign / float64(n)
		}
	}
	var norm float64
	for _, value := range vector {
		norm += value * value
	}
	if norm > 0 {
		norm = math.Sqrt(norm)
		for index := range vector {
			vector[index] /= norm
		}
	}
	return vector
}

// apiEmbedding 用模型服务生成向量。
type apiEmbedding struct {
	client *llm.Client
	model  string
}

// NewLLMEmbedder 构造走模型的向量器；没有 Key 时返回 nil，调用方据此退回离线向量。
func NewLLMEmbedder(client *llm.Client, model string) Embedder {
	if client == nil || !client.Enabled() {
		return nil
	}
	if strings.TrimSpace(model) == "" {
		model = client.EmbeddingModel()
	}
	return apiEmbedding{client: client, model: model}
}

func (a apiEmbedding) Name() string { return "api:" + a.model }

func (a apiEmbedding) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	return a.client.Embed(ctx, a.model, texts)
}
