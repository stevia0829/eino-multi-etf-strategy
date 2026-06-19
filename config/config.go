package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/multi-agents-etf-trade-strategy/llm"
)

type Config struct {
	LLM llm.MultiProviderConfig
}

// secretsFile 本地不入库的密钥文件结构。
// 路径默认 config/secrets.json，可通过环境变量 SECRETS_FILE 覆盖。
// 字段为空表示未配置，会回落到同名 OS 环境变量。
type secretsFile struct {
	DeepSeekAPIKey string `json:"deepseek_api_key"`
	MoonshotAPIKey string `json:"moonshot_api_key"`
	DoubaoAPIKey   string `json:"doubao_api_key"`
	QwenAPIKey     string `json:"qwen_api_key"`
	OpenAIAPIKey   string `json:"openai_api_key"`
	// CustomAPIKey 通用占位：用于自建网关 / 任意 OpenAI 兼容厂商。
	CustomAPIKey string `json:"custom_api_key"`
}

// loadSecrets 读取 config/secrets.json；文件不存在 / 解析失败时返回空结构（不算错误）。
func loadSecrets() secretsFile {
	path := os.Getenv("SECRETS_FILE")
	if path == "" {
		// 默认相对项目根目录的 config/secrets.json
		path = filepath.Join("config", "secrets.json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return secretsFile{}
	}
	var s secretsFile
	if err := json.Unmarshal(data, &s); err != nil {
		return secretsFile{}
	}
	return s
}

// pickKey 优先取 secrets 文件中的值，缺失则回落到 OS 环境变量。
func pickKey(fromFile, envName string) string {
	if fromFile != "" {
		return fromFile
	}
	return os.Getenv(envName)
}

// providerCatalog 各厂商默认配置（baseURL / 默认模型 / 取 key 的来源）。
// 任何 OpenAI Chat Completions 兼容的厂商都能直接进入这张表，主/备两端共用。
type providerCatalog struct {
	BaseURL    string
	Model      string
	URLEnv     string // 覆盖 baseURL 的 OS 环境变量名
	ModelEnv   string // 覆盖 model 的 OS 环境变量名
	GetAPIKey  func(secretsFile) string
}

var providerDefaults = map[string]providerCatalog{
	"deepseek": {
		BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-chat",
		URLEnv: "DEEPSEEK_BASE_URL", ModelEnv: "DEEPSEEK_MODEL",
		GetAPIKey: func(s secretsFile) string { return pickKey(s.DeepSeekAPIKey, "DEEPSEEK_API_KEY") },
	},
	"moonshot": {
		BaseURL: "https://api.moonshot.cn/v1", Model: "moonshot-v1-8k",
		URLEnv: "MOONSHOT_BASE_URL", ModelEnv: "MOONSHOT_MODEL",
		GetAPIKey: func(s secretsFile) string { return pickKey(s.MoonshotAPIKey, "MOONSHOT_API_KEY") },
	},
	"doubao": {
		BaseURL: "https://ark.cn-beijing.volces.com/api/v3", Model: "doubao-1-5-pro-32k",
		URLEnv: "DOUBAO_BASE_URL", ModelEnv: "DOUBAO_MODEL",
		GetAPIKey: func(s secretsFile) string { return pickKey(s.DoubaoAPIKey, "DOUBAO_API_KEY") },
	},
	"qwen": {
		BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", Model: "qwen-plus",
		URLEnv: "QWEN_BASE_URL", ModelEnv: "QWEN_MODEL",
		GetAPIKey: func(s secretsFile) string { return pickKey(s.QwenAPIKey, "QWEN_API_KEY") },
	},
	"openai": {
		BaseURL: "https://api.openai.com/v1", Model: "gpt-4o-mini",
		URLEnv: "OPENAI_BASE_URL", ModelEnv: "OPENAI_MODEL",
		GetAPIKey: func(s secretsFile) string { return pickKey(s.OpenAIAPIKey, "OPENAI_API_KEY") },
	},
	// custom：自建 / 公司网关 / 未在表中的厂商，全部通过这条通道接入。
	// 必填环境变量：CUSTOM_BASE_URL / CUSTOM_MODEL / CUSTOM_API_KEY。
	"custom": {
		BaseURL: "", Model: "",
		URLEnv: "CUSTOM_BASE_URL", ModelEnv: "CUSTOM_MODEL",
		GetAPIKey: func(s secretsFile) string { return pickKey(s.CustomAPIKey, "CUSTOM_API_KEY") },
	},
}

// buildProvider 把 catalog 表项装配成 ProviderConfig。
// name 不在表中时静默跳过（返回 zero ProviderConfig 由调用方过滤）。
func buildProvider(name string, sec secretsFile) llm.ProviderConfig {
	name = strings.ToLower(strings.TrimSpace(name))
	cat, ok := providerDefaults[name]
	if !ok {
		return llm.ProviderConfig{}
	}
	apiKey := cat.GetAPIKey(sec)
	baseURL := getEnv(cat.URLEnv, cat.BaseURL)
	model := getEnv(cat.ModelEnv, cat.Model)
	return llm.ProviderConfig{
		Name:    name,
		APIKey:  apiKey,
		BaseURL: baseURL,
		Model:   model,
		Timeout: getEnvDuration(strings.ToUpper(name)+"_TIMEOUT", 60*time.Second),
		Enabled: apiKey != "" && baseURL != "" && model != "",
	}
}

// resolveProviderChain 解析"主 + 备"链：
//   - 默认顺序：deepseek → moonshot → doubao → qwen
//   - 可由 LLM_PRIMARY 指定主厂商（取值见 providerDefaults 的 key）
//   - 可由 LLM_FALLBACKS 指定备链，逗号分隔，如 "moonshot,doubao,custom"
//   - 任何"无 key / 无 baseURL / 无 model"的 provider 自动跳过
func resolveProviderChain(sec secretsFile) (llm.ProviderConfig, []llm.ProviderConfig) {
	primaryName := strings.ToLower(getEnv("LLM_PRIMARY", "deepseek"))
	fbRaw := getEnv("LLM_FALLBACKS", "moonshot,doubao,qwen")

	// primary：若用户指定的主厂商无 key，仍构造（Build 时返回 error，给出明确报错）。
	primary := buildProvider(primaryName, sec)
	if primary.Name == "" {
		// 名字不在 catalog → 退回 deepseek，不静默吞错
		primary = buildProvider("deepseek", sec)
	}

	seen := map[string]bool{primary.Name: true}
	fallbacks := make([]llm.ProviderConfig, 0, 4)
	for _, raw := range strings.Split(fbRaw, ",") {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		p := buildProvider(name, sec)
		if p.Name == "" {
			continue
		}
		fallbacks = append(fallbacks, p)
	}
	return primary, fallbacks
}

func Load() *Config {
	sec := loadSecrets()
	primary, fallbacks := resolveProviderChain(sec)

	return &Config{
		LLM: llm.MultiProviderConfig{
			Primary:    primary,
			Fallbacks:  fallbacks,
			MaxRetries: getEnvInt("LLM_MAX_RETRIES", 2),
			BaseDelay:  getEnvDuration("LLM_BASE_DELAY", 500*time.Millisecond),
		},
	}
}

func getEnv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func getEnvInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getEnvDuration(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
