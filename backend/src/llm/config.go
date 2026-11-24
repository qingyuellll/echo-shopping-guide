// Package llm 封装与 OpenAI 兼容接口的大模型调用。
//
// 导购站只有两处需要模型：把已经算好的事实说成人话（Complete / Stream），
// 以及把运营粘贴的演出描述整理成草稿。模型不参与任何数据计算，
// 未配置 API Key 时整个服务照常工作，只是少了这层润色。
package llm

import (
	"os"
	"strings"
)

const (
	// defaultBaseURL 是未显式配置时的默认入口（通义千问兼容模式）。
	defaultBaseURL   = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	defaultSmall     = "qwen3.5-flash"
	defaultLarge     = "qwen3.7-plus"
	defaultEmbedding = "text-embedding-v3"
)

// Config 是一次模型调用的全部参数，全部来自环境变量。
type Config struct {
	BaseURL        string
	APIKey         string
	SmallModel     string
	LargeModel     string
	EmbeddingModel string
	EnableThinking bool
}

// ConfigFromEnv 读取 AI_* / DASHSCOPE_* 环境变量，缺省值保证本地能直接跑。
func ConfigFromEnv() Config {
	return Config{
		BaseURL:        env("AI_BASE_URL", env("DASHSCOPE_BASE_URL", defaultBaseURL)),
		APIKey:         env("DASHSCOPE_API_KEY", env("AI_API_KEY", "")),
		SmallModel:     env("AI_SMALL_MODEL", defaultSmall),
		LargeModel:     env("AI_LARGE_MODEL", defaultLarge),
		EmbeddingModel: env("AI_EMBEDDING_MODEL", defaultEmbedding),
		EnableThinking: parseBool(env("AI_ENABLE_THINKING", "false"), false),
	}.withDefaults()
}

func (c Config) withDefaults() Config {
	if strings.TrimSpace(c.BaseURL) == "" {
		c.BaseURL = defaultBaseURL
	}
	if strings.TrimSpace(c.SmallModel) == "" {
		c.SmallModel = defaultSmall
	}
	if strings.TrimSpace(c.LargeModel) == "" {
		c.LargeModel = defaultLarge
	}
	if strings.TrimSpace(c.EmbeddingModel) == "" {
		c.EmbeddingModel = defaultEmbedding
	}
	return c
}

func env(key string, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func parseBool(value string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes", "on", "enabled":
		return true
	case "false", "0", "no", "off", "disabled":
		return false
	default:
		return fallback
	}
}
