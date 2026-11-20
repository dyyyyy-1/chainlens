package api

import (
	"context"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/dyyyyy-1/chainlens/internal/profile"
)

const (
	// balanceTimeout 是单个代币余额查询的预算。
	//
	// 比请求总预算（5s）短得多：宁可这一格显示「获取失败」，
	// 也不该让一个可选字段拖垮整页画像。
	balanceTimeout = 3 * time.Second

	// balanceCacheTTL 是余额缓存时长。前端连续刷新或用多个标签页看同一地址时，
	// 不该每次都打免费 RPC 端点。
	balanceCacheTTL = 10 * time.Second

	// decimalsCacheTTL 是 decimals 的缓存时长。它几乎不随区块变化，可以缓存得久。
	decimalsCacheTTL = time.Hour

	// maxBalanceTokens 限制一次请求最多查几个合约的余额。
	//
	// holdings 按合约聚合，多合约索引后可能几十行；逐行打 RPC 会直接把
	// 免费端点的速率限制打满，而用户真正关心的是头几个。
	maxBalanceTokens = 5

	// balanceSweepEvery 是「每写多少次缓存清理一次过期项」。
	//
	// 缓存键含地址，是天然的无限基数；不清理就是缓慢的内存泄漏。
	// 每次写入都全表扫一遍太浪费，按固定次数触发即可。
	balanceSweepEvery = 256
)

// TokenReader 是 ERC-20 只读能力，由 internal/chain 实现（接口在使用方声明）。
type TokenReader interface {
	BalanceOf(ctx context.Context, token, holder common.Address) (*big.Int, error)
	Decimals(ctx context.Context, token common.Address) (uint8, error)
}

// onchainBalance 是单个 (合约, 地址) 的链上余额快照。
//
// Err 非空表示这次没取到；cause 保留原始错误供日志使用，绝不进响应。
type onchainBalance struct {
	Value     *big.Int
	Decimals  *int
	FetchedAt time.Time
	Cached    bool
	Err       string
	cause     error
}

// balanceService 给余额查询加缓存与并发。
type balanceService struct {
	reader TokenReader

	mu       sync.Mutex
	balances map[balanceKey]cachedBalance
	decimals map[common.Address]cachedDecimals
	writes   int
}

type balanceKey struct {
	token  common.Address
	holder common.Address
}

type cachedBalance struct {
	value     *big.Int
	fetchedAt time.Time
	expiresAt time.Time
}

type cachedDecimals struct {
	value     uint8
	expiresAt time.Time
}

func newBalanceService(reader TokenReader) *balanceService {
	return &balanceService{
		reader:   reader,
		balances: make(map[balanceKey]cachedBalance),
		decimals: make(map[common.Address]cachedDecimals),
	}
}

// snapshot 并发查询多个代币的余额，返回以合约地址为键的快照。
//
// 单个代币失败只影响它自己：其余代币照常返回，整体不会因为一个 RPC 抖动而变空。
func (s *balanceService) snapshot(ctx context.Context, holder common.Address, tokens []common.Address) map[string]onchainBalance {
	tokens = capTokens(tokens, maxBalanceTokens)
	if len(tokens) == 0 {
		return nil
	}

	results := make([]onchainBalance, len(tokens))
	var wg sync.WaitGroup
	for i, token := range tokens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = s.lookup(ctx, token, holder)
		}()
	}
	wg.Wait()

	balances := make(map[string]onchainBalance, len(tokens))
	for i, token := range tokens {
		balances[hexAddress(token)] = results[i]
	}
	return balances
}

// lookup 查单个余额，命中缓存则直接返回。
func (s *balanceService) lookup(parent context.Context, token, holder common.Address) onchainBalance {
	key := balanceKey{token: token, holder: holder}
	now := time.Now()

	s.mu.Lock()
	cached, hit := s.balances[key]
	s.mu.Unlock()

	if hit && now.Before(cached.expiresAt) {
		return onchainBalance{
			Value:     new(big.Int).Set(cached.value),
			Decimals:  s.decimalsOf(parent, token),
			FetchedAt: cached.fetchedAt,
			Cached:    true,
		}
	}

	ctx, cancel := context.WithTimeout(parent, balanceTimeout)
	defer cancel()

	value, err := s.reader.BalanceOf(ctx, token, holder)
	if err != nil {
		return onchainBalance{Err: "unavailable", cause: err}
	}

	fetchedAt := time.Now()
	s.store(balanceKey{token: token, holder: holder}, value, fetchedAt)

	return onchainBalance{
		Value:     value,
		Decimals:  s.decimalsOf(parent, token),
		FetchedAt: fetchedAt,
	}
}

// decimalsOf 返回代币小数位，未知时返回 nil（调用方不得猜测）。
func (s *balanceService) decimalsOf(parent context.Context, token common.Address) *int {
	now := time.Now()

	s.mu.Lock()
	cached, hit := s.decimals[token]
	s.mu.Unlock()

	if hit && now.Before(cached.expiresAt) {
		value := int(cached.value)
		return &value
	}

	ctx, cancel := context.WithTimeout(parent, balanceTimeout)
	defer cancel()

	value, err := s.reader.Decimals(ctx, token)
	if err != nil {
		return nil
	}

	s.mu.Lock()
	s.decimals[token] = cachedDecimals{value: value, expiresAt: now.Add(decimalsCacheTTL)}
	s.mu.Unlock()

	result := int(value)
	return &result
}

// store 写入余额缓存，并按固定节奏清理过期项。
func (s *balanceService) store(key balanceKey, value *big.Int, fetchedAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.balances[key] = cachedBalance{
		value:     new(big.Int).Set(value),
		fetchedAt: fetchedAt,
		expiresAt: fetchedAt.Add(balanceCacheTTL),
	}

	s.writes++
	if s.writes%balanceSweepEvery == 0 {
		s.sweepLocked(fetchedAt)
	}
}

// sweepLocked 删除已过期的缓存项。调用方必须已持有 mu。
func (s *balanceService) sweepLocked(now time.Time) {
	for key, entry := range s.balances {
		if now.After(entry.expiresAt) {
			delete(s.balances, key)
		}
	}
	for token, entry := range s.decimals {
		if now.After(entry.expiresAt) {
			delete(s.decimals, token)
		}
	}
}

// capTokens 保持顺序去重，并截断到 limit 个。
func capTokens(tokens []common.Address, limit int) []common.Address {
	if len(tokens) == 0 {
		return nil
	}

	capped := make([]common.Address, 0, min(len(tokens), limit))
	seen := make(map[common.Address]struct{}, len(tokens))
	for _, token := range tokens {
		if len(capped) >= limit {
			break
		}
		if _, duplicate := seen[token]; duplicate {
			continue
		}
		seen[token] = struct{}{}
		capped = append(capped, token)
	}
	return capped
}

// holdingContracts 取持仓涉及的合约地址，顺序与 holdings 一致。
func holdingContracts(holdings []profile.Holding) []common.Address {
	contracts := make([]common.Address, 0, len(holdings))
	for _, holding := range holdings {
		contracts = append(contracts, holding.Contract)
	}
	return contracts
}
