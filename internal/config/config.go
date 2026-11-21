// Package config 从进程环境变量与 .env 文件装载运行期配置。
//
// 字段与 HANDOFF.md 第 10 节的 .env 模板一一对应，不额外引入环境变量。
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
)

// Config 是索引器与查询服务共用的配置集合。
type Config struct {
	DatabaseURL     string
	RPCHTTPURL      string
	EtherscanAPIKey string
	ChainID         uint64
	// TargetContracts 是索引目标，至少一个。
	//
	// TARGET_CONTRACTS（逗号分隔）优先；为空时回退到单值写法 TARGET_CONTRACT，
	// 保证旧 .env 与脚本不用改。
	TargetContracts []string
	StartBlock      uint64
	Confirmations   uint64
	BatchBlockRange uint64
	// RPCHeaderBatch 是单次批量请求容纳的块头数。
	// publicnode 允许 100，drpc 免费档只允许 3；换端点补历史时必须一起改。
	RPCHeaderBatch uint64
	LogLevel       slog.Level
	// HTTPAddr 是查询服务的监听地址，仅 cmd/api 使用。
	HTTPAddr string
	// HTTPRateLimit 是每个来源 IP 每分钟的请求上限，仅 cmd/api 使用。0 表示不限流。
	HTTPRateLimit uint64
	// APITokens 非空时查询服务要求 Bearer token，仅 cmd/api 使用。
	//
	// 支持逗号分隔多值（`API_TOKEN=old,new`）：轮换时新旧口令可并存，
	// 不必让调用方在同一时刻切换。口令本身不能包含逗号。
	APITokens []string
	// WebDir 是前端构建产物目录，仅 cmd/api 使用。为空表示不托管前端。
	WebDir string
}

// .env 模板中的缺省值，仅在对应环境变量为空时生效。
const (
	defaultConfirmations   = 12
	defaultBatchBlockRange = 2000
	defaultHeaderBatch     = 100
	defaultHTTPAddr        = ":8080"
	defaultRateLimit       = 120
	defaultWebDir          = "web/dist"
)

// Load 先读取 envFile（可为空串或不存在），再从进程环境变量解析配置。
//
// 真实环境变量优先于 .env 中的同名项，便于 CI 与临时覆盖。
func Load(envFile string) (Config, error) {
	if envFile != "" {
		if err := loadEnvFile(envFile); err != nil {
			return Config{}, err
		}
	}

	cfg := Config{
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		RPCHTTPURL:      strings.TrimSpace(os.Getenv("RPC_HTTP_URL")),
		EtherscanAPIKey: os.Getenv("ETHERSCAN_API_KEY"),
		TargetContracts: parseContracts(os.Getenv("TARGET_CONTRACTS"), os.Getenv("TARGET_CONTRACT")),
		HTTPAddr:        httpAddr(),
	}
	if cfg.RPCHTTPURL == "" {
		return Config{}, errors.New("config: RPC_HTTP_URL is required")
	}

	// CHAIN_ID 必填：主网与 Sepolia 混用会造成难以察觉的数据污染（ADR-5）。
	chainID, err := parseUint64("CHAIN_ID", 0)
	if err != nil {
		return Config{}, err
	}
	if chainID == 0 {
		return Config{}, errors.New("config: CHAIN_ID is required")
	}
	cfg.ChainID = chainID

	if cfg.StartBlock, err = parseUint64("START_BLOCK", 0); err != nil {
		return Config{}, err
	}
	if cfg.Confirmations, err = parseUint64("CONFIRMATIONS", defaultConfirmations); err != nil {
		return Config{}, err
	}
	if cfg.BatchBlockRange, err = parseUint64("BATCH_BLOCK_RANGE", defaultBatchBlockRange); err != nil {
		return Config{}, err
	}
	if cfg.RPCHeaderBatch, err = parseUint64("RPC_HEADER_BATCH", defaultHeaderBatch); err != nil {
		return Config{}, err
	}
	if cfg.LogLevel, err = parseLogLevel("LOG_LEVEL"); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// LoadServer 装载查询服务所需的配置。
//
// 查询服务只读数据库、不访问链，因此**不要求** RPC_HTTP_URL 与 CHAIN_ID。
// 这是它与索引器唯一的分歧：多要求一个用不到的变量，会让 api 在只缺 RPC 时
// 报出与真实原因无关的错误。
func LoadServer(envFile string) (Config, error) {
	if envFile != "" {
		if err := loadEnvFile(envFile); err != nil {
			return Config{}, err
		}
	}

	cfg := Config{
		DatabaseURL: os.Getenv("DATABASE_URL"),
		HTTPAddr:    httpAddr(),
		APITokens:   splitTokens(os.Getenv("API_TOKEN")),
		WebDir:      strings.TrimSpace(os.Getenv("WEB_DIR")),
		// cmd/redecode 用它按合约取 ABI；查询服务本身不读它。
		// 集中在这里解析，避免各命令各自 os.Getenv 出一套不一致的默认值。
		TargetContracts: parseContracts(os.Getenv("TARGET_CONTRACTS"), os.Getenv("TARGET_CONTRACT")),
		// 链上余额是可选能力：只配置其中一个变量不足以安全调用 RPC
		// （chain 层要靠 CHAIN_ID 拒绝主网与测试网混用），因此两者缺一即视为未启用。
		RPCHTTPURL: strings.TrimSpace(os.Getenv("RPC_HTTP_URL")),
	}
	if cfg.WebDir == "" {
		cfg.WebDir = defaultWebDir
	}
	// 目录不存在时关闭托管，避免每个请求都去 stat 一个不存在的路径。
	if info, statErr := os.Stat(cfg.WebDir); statErr != nil || !info.IsDir() {
		cfg.WebDir = ""
	}

	var err error
	if cfg.ChainID, err = parseUint64("CHAIN_ID", 0); err != nil {
		return Config{}, err
	}
	if cfg.LogLevel, err = parseLogLevel("LOG_LEVEL"); err != nil {
		return Config{}, err
	}
	if cfg.HTTPRateLimit, err = parseUint64("HTTP_RATE_LIMIT", defaultRateLimit); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// httpAddr 返回监听地址，未设置时用 :8080。
func httpAddr() string {
	if addr := strings.TrimSpace(os.Getenv("HTTP_ADDR")); addr != "" {
		return addr
	}
	return defaultHTTPAddr
}

// loadEnvFile 解析 KEY=VALUE 形式的 .env 文件，忽略空行与 # 注释。
//
// 文件不存在视为正常情况：配置可由真实环境变量完整提供。
func loadEnvFile(path string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("config: read %s: %w", path, err)
	}

	for i, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, hasSeparator := strings.Cut(line, "=")
		if !hasSeparator {
			return fmt.Errorf("config: %s:%d: missing '=' separator", path, i+1)
		}

		key = strings.TrimSpace(key)
		if _, hasKey := os.LookupEnv(key); hasKey {
			continue
		}
		if err := os.Setenv(key, unquote(strings.TrimSpace(value))); err != nil {
			return fmt.Errorf("config: %s:%d: set %s: %w", path, i+1, key, err)
		}
	}

	return nil
}

// unquote 去掉成对的首尾单引号或双引号，兼容 `KEY="value"` 写法。
func unquote(value string) string {
	if len(value) < 2 {
		return value
	}
	first, last := value[0], value[len(value)-1]
	if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
		return value[1 : len(value)-1]
	}
	return value
}

// parseContracts 解析合约列表：多值写法优先，回退到单值写法。
//
// 两种情况共用 splitTokens 的切分规则（去空白、去空项、保序去重）：
// 重复的合约会让索引器对同一份日志做两次解码，虽然结果相同，但白白浪费 ABI 解析与 RPC。
func parseContracts(multi, single string) []string {
	if strings.TrimSpace(multi) != "" {
		return splitTokens(multi)
	}
	return splitTokens(single)
}

// splitTokens 把逗号分隔的口令串切成列表，去掉空白与空项并按首次出现去重。
//
// 去重不是洁癖：重复项会让「有几个口令」这件事在日志与配置对账时对不上。
func splitTokens(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	parts := strings.Split(raw, ",")
	tokens := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		token := strings.TrimSpace(part)
		if token == "" {
			continue
		}
		if _, duplicate := seen[token]; duplicate {
			continue
		}
		seen[token] = struct{}{}
		tokens = append(tokens, token)
	}

	if len(tokens) == 0 {
		return nil
	}
	return tokens
}

// parseUint64 解析十进制无符号整数，空值回落到 fallback。
func parseUint64(key string, fallback uint64) (uint64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("config: parse %s=%q: %w", key, raw, err)
	}
	return value, nil
}

func parseLogLevel(key string) (slog.Level, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return slog.LevelInfo, nil
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(raw)); err != nil {
		return 0, fmt.Errorf("config: parse %s=%q: %w", key, raw, err)
	}
	return level, nil
}
