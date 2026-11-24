package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Client 是唯一的模型出入口。零值不可用，必须用 New 构造。
type Client struct {
	mu     sync.RWMutex
	config Config
	http   *http.Client
}

// Message 是最小对话结构，只保留角色和内容，够导购场景用。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// New 构建客户端；没有 API Key 时 Enabled 返回 false，调用方据此跳过润色。
func New(config Config) *Client {
	return &Client{
		config: config.withDefaults(),
		http:   &http.Client{Timeout: 45 * time.Second},
	}
}

// Enabled 表示是否配置了可用的密钥。
func (c *Client) Enabled() bool {
	return c != nil && strings.TrimSpace(c.Config().APIKey) != ""
}

// SmallModel 用于短任务：草稿整理、数据缺失说明。
func (c *Client) SmallModel() string { return c.Config().SmallModel }

// LargeModel 用于生成面向用户的回答。
func (c *Client) LargeModel() string { return c.Config().LargeModel }

// EmbeddingModel 用于把文本转成向量（常识库检索）。
func (c *Client) EmbeddingModel() string { return c.Config().EmbeddingModel }

// Config 返回当前配置副本（管理端只展示模型名与思考开关）。
func (c *Client) Config() Config {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.config
}

// Complete 一次性要回完整回答，用于草稿整理这类不需要逐字展示的场景。
func (c *Client) Complete(ctx context.Context, model string, messages []Message, temperature float64) (string, error) {
	config := c.Config()
	if strings.TrimSpace(config.APIKey) == "" {
		return "", errors.New("llm disabled: missing api key")
	}
	if model == "" {
		model = config.SmallModel
	}
	payload, err := json.Marshal(map[string]any{
		"model":           model,
		"messages":        messages,
		"temperature":     temperature,
		"enable_thinking": config.EnableThinking,
	})
	if err != nil {
		return "", fmt.Errorf("marshal llm request: %w", err)
	}
	body, err := c.post(ctx, payload)
	if err != nil {
		return "", err
	}
	var decoded struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", fmt.Errorf("decode llm response: %w", err)
	}
	if decoded.Error != nil && decoded.Error.Message != "" {
		return "", errors.New(decoded.Error.Message)
	}
	if len(decoded.Choices) == 0 {
		return "", errors.New("llm response has no choices")
	}
	content := strings.TrimSpace(decoded.Choices[0].Message.Content)
	if content == "" {
		return "", errors.New("llm response is empty")
	}
	return content, nil
}

// Embed 把一批文本转成向量。批量传入而不是逐条调用：索引构建时
// 一次请求就能覆盖整个常识库，省掉十几轮往返。
func (c *Client) Embed(ctx context.Context, model string, texts []string) ([][]float64, error) {
	config := c.Config()
	if strings.TrimSpace(config.APIKey) == "" {
		return nil, errors.New("llm disabled: missing api key")
	}
	if len(texts) == 0 {
		return nil, nil
	}
	if model == "" {
		model = config.EmbeddingModel
	}
	payload, err := json.Marshal(map[string]any{
		"model": model, "input": texts, "encoding_format": "float",
	})
	if err != nil {
		return nil, fmt.Errorf("marshal embedding request: %w", err)
	}
	body, err := c.postTo(ctx, "/embeddings", payload)
	if err != nil {
		return nil, err
	}
	var decoded struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("decode embedding response: %w", err)
	}
	if decoded.Error != nil && decoded.Error.Message != "" {
		return nil, errors.New(decoded.Error.Message)
	}
	// 按 index 回填：服务端不保证返回顺序与输入一致。
	vectors := make([][]float64, len(texts))
	for _, item := range decoded.Data {
		if item.Index >= 0 && item.Index < len(vectors) && len(item.Embedding) > 0 {
			vectors[item.Index] = item.Embedding
		}
	}
	for index, vector := range vectors {
		if len(vector) == 0 {
			return nil, fmt.Errorf("embedding %d missing in response", index)
		}
	}
	return vectors, nil
}

// Stream 逐字回调，用于 SSE 咨询：用户先看到数据版答案，再看到模型润色。
func (c *Client) Stream(ctx context.Context, model string, messages []Message, temperature float64, onDelta func(string) error) error {
	config := c.Config()
	if strings.TrimSpace(config.APIKey) == "" {
		return errors.New("llm disabled: missing api key")
	}
	if model == "" {
		model = config.SmallModel
	}
	payload, err := json.Marshal(map[string]any{
		"model":           model,
		"messages":        messages,
		"temperature":     temperature,
		"stream":          true,
		"enable_thinking": config.EnableThinking,
	})
	if err != nil {
		return fmt.Errorf("marshal llm stream request: %w", err)
	}
	request, err := c.request(ctx, "/chat/completions", payload)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "text/event-stream")
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("call llm stream: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		return fmt.Errorf("llm stream status %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 2<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			return nil
		}
		delta, err := decodeStreamDelta(data)
		if err != nil {
			return err
		}
		if delta == "" {
			continue
		}
		if err := onDelta(delta); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read llm stream: %w", err)
	}
	return nil
}

func (c *Client) post(ctx context.Context, payload []byte) ([]byte, error) {
	return c.postTo(ctx, "/chat/completions", payload)
}

// postTo 让同一套重试/错误处理覆盖不同端点（对话与向量）。
func (c *Client) postTo(ctx context.Context, path string, payload []byte) ([]byte, error) {
	request, err := c.request(ctx, path, payload)
	if err != nil {
		return nil, err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call llm: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return nil, fmt.Errorf("read llm response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("llm status %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func (c *Client) request(ctx context.Context, path string, payload []byte) (*http.Request, error) {
	config := c.Config()
	url := strings.TrimRight(config.BaseURL, "/") + path
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create llm request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+config.APIKey)
	request.Header.Set("Content-Type", "application/json")
	return request, nil
}

func decodeStreamDelta(data string) (string, error) {
	var decoded struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.Unmarshal([]byte(data), &decoded); err != nil {
		return "", fmt.Errorf("decode llm stream event: %w", err)
	}
	if decoded.Error != nil && decoded.Error.Message != "" {
		return "", errors.New(decoded.Error.Message)
	}
	if len(decoded.Choices) == 0 {
		return "", nil
	}
	if decoded.Choices[0].Delta.Content != "" {
		return decoded.Choices[0].Delta.Content, nil
	}
	return decoded.Choices[0].Message.Content, nil
}
