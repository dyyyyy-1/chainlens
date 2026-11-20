package api

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/dyyyyy-1/chainlens/internal/profile"
)

// fakeTokenReader 记录调用次数，用于验证缓存确实挡住了重复的 RPC。
type fakeTokenReader struct {
	balance       *big.Int
	decimals      uint8
	balanceErr    error
	decimalsErr   error
	balanceCalls  atomic.Int32
	decimalsCalls atomic.Int32
}

func (f *fakeTokenReader) BalanceOf(context.Context, common.Address, common.Address) (*big.Int, error) {
	f.balanceCalls.Add(1)
	if f.balanceErr != nil {
		return nil, f.balanceErr
	}
	return new(big.Int).Set(f.balance), nil
}

func (f *fakeTokenReader) Decimals(context.Context, common.Address) (uint8, error) {
	f.decimalsCalls.Add(1)
	if f.decimalsErr != nil {
		return 0, f.decimalsErr
	}
	return f.decimals, nil
}

var balanceSampleToken = common.HexToAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48")

func balanceSampleHoldings() []profile.Holding {
	return []profile.Holding{{
		Contract: balanceSampleToken,
		Net:      big.NewInt(-1558844000),
		LogCount: 42,
	}}
}

func newBalanceRouter(tokens TokenReader, holdings []profile.Holding) http.Handler {
	return NewRouter(Deps{
		Events:  &fakeEvents{coverage: testCoverage()},
		Profile: &fakeProfile{holdings: holdings},
		Tokens:  tokens,
	})
}

type balancesBody struct {
	Data    []holdingDTO `json:"data"`
	Onchain onchainDTO   `json:"onchain"`
}

func TestBalancesOnchainDisabledWithoutReader(t *testing.T) {
	handler := newBalanceRouter(nil, balanceSampleHoldings())

	body := decodeBody[balancesBody](t, get(t, handler, "/api/address/"+sampleAddress+"/balances"))
	if body.Onchain.Enabled {
		t.Fatal("未配置 RPC 时 onchain.enabled 必须为 false")
	}
	// 主字段必须照常返回：余额是可选能力，不能因为没配 RPC 就让持仓消失。
	if len(body.Data) != 1 || body.Data[0].Net != "-1558844000" {
		t.Fatalf("持仓字段受影响：%+v", body.Data)
	}
}

func TestBalancesIncludeOnchainBalance(t *testing.T) {
	reader := &fakeTokenReader{balance: big.NewInt(1234567890), decimals: 6}
	handler := newBalanceRouter(reader, balanceSampleHoldings())

	body := decodeBody[balancesBody](t, get(t, handler, "/api/address/"+sampleAddress+"/balances"))
	if !body.Onchain.Enabled {
		t.Fatal("配置了 RPC 时 onchain.enabled 必须为 true")
	}

	entry, ok := body.Onchain.Balances[hexAddress(balanceSampleToken)]
	if !ok {
		t.Fatalf("balances 缺少合约 %s：%+v", hexAddress(balanceSampleToken), body.Onchain.Balances)
	}
	if entry.Status != "ok" {
		t.Fatalf("status = %q, want ok", entry.Status)
	}
	if entry.Value != "1234567890" {
		t.Fatalf("value = %q, want 1234567890（必须是未换算的原始整数）", entry.Value)
	}
	if entry.Decimals == nil || *entry.Decimals != 6 {
		t.Fatalf("decimals = %v, want 6", entry.Decimals)
	}
	if entry.Cached {
		t.Fatal("首次查询不应标记为缓存命中")
	}
	if entry.FetchedAt == "" {
		t.Fatal("fetched_at 不应为空")
	}
}

func TestBalancesOnchainFailureKeepsHoldings(t *testing.T) {
	reader := &fakeTokenReader{balanceErr: errors.New("rpc: rate limited")}
	handler := newBalanceRouter(reader, balanceSampleHoldings())

	recorder := get(t, handler, "/api/address/"+sampleAddress+"/balances")
	if recorder.Code != http.StatusOK {
		t.Fatalf("RPC 失败不应改变状态码，得到 %d", recorder.Code)
	}

	body := decodeBody[balancesBody](t, recorder)
	if len(body.Data) != 1 {
		t.Fatalf("持仓必须照常返回，data 长度 = %d", len(body.Data))
	}

	entry := body.Onchain.Balances[hexAddress(balanceSampleToken)]
	if entry.Status != "unavailable" {
		t.Fatalf("status = %q, want unavailable", entry.Status)
	}
	if entry.Value != "" {
		t.Fatalf("失败时不应给 value，得到 %q", entry.Value)
	}
	// 全部代币失败时给出顶层错误码，调用方不必逐条检查。
	if body.Onchain.Error != "rpc_unavailable" {
		t.Fatalf("onchain.error = %q, want rpc_unavailable", body.Onchain.Error)
	}
	// 原始错误只进日志，不能出现在响应里。
	if raw := recorder.Body.String(); strings.Contains(raw, "rate limited") {
		t.Fatalf("响应泄漏了 RPC 细节：%s", raw)
	}
}

func TestBalancesOnchainCache(t *testing.T) {
	reader := &fakeTokenReader{balance: big.NewInt(1), decimals: 6}
	handler := newBalanceRouter(reader, balanceSampleHoldings())

	first := decodeBody[balancesBody](t, get(t, handler, "/api/address/"+sampleAddress+"/balances"))
	second := decodeBody[balancesBody](t, get(t, handler, "/api/address/"+sampleAddress+"/balances"))

	if got := reader.balanceCalls.Load(); got != 1 {
		t.Fatalf("balanceOf 调用 %d 次，缓存生效时应为 1 次", got)
	}
	if got := reader.decimalsCalls.Load(); got != 1 {
		t.Fatalf("decimals 调用 %d 次，缓存生效时应为 1 次", got)
	}

	if !second.Onchain.Balances[hexAddress(balanceSampleToken)].Cached {
		t.Fatal("第二次请求应标记 cached=true")
	}
	if first.Onchain.Balances[hexAddress(balanceSampleToken)].Cached {
		t.Fatal("第一次请求不应标记 cached=true")
	}
}

func TestBalancesUnknownDecimalsStaysNull(t *testing.T) {
	reader := &fakeTokenReader{balance: big.NewInt(5), decimalsErr: errors.New("no decimals()")}
	handler := newBalanceRouter(reader, balanceSampleHoldings())

	body := decodeBody[balancesBody](t, get(t, handler, "/api/address/"+sampleAddress+"/balances"))
	entry := body.Onchain.Balances[hexAddress(balanceSampleToken)]

	if entry.Status != "ok" {
		t.Fatalf("decimals 失败不应让余额一起失败，status = %q", entry.Status)
	}
	if entry.Decimals != nil {
		t.Fatalf("decimals 未知时必须为 null，得到 %v", *entry.Decimals)
	}
}

func TestBalanceCacheExpires(t *testing.T) {
	reader := &fakeTokenReader{balance: big.NewInt(7), decimals: 6}
	service := newBalanceService(reader)
	holder := common.HexToAddress(sampleAddress)

	if got := service.lookup(context.Background(), balanceSampleToken, holder); got.Err != "" {
		t.Fatalf("首次查询失败：%v", got.cause)
	}

	// 把缓存项直接改成已过期，避免测试里真的等 10 秒。
	service.mu.Lock()
	key := balanceKey{token: balanceSampleToken, holder: holder}
	entry := service.balances[key]
	entry.expiresAt = time.Now().Add(-time.Second)
	service.balances[key] = entry
	service.mu.Unlock()

	service.lookup(context.Background(), balanceSampleToken, holder)
	if got := reader.balanceCalls.Load(); got != 2 {
		t.Fatalf("缓存过期后应重新查询，balanceOf 调用 %d 次", got)
	}
}

func TestCapTokensKeepsOrderAndDropsDuplicates(t *testing.T) {
	a := common.HexToAddress("0x01")
	b := common.HexToAddress("0x02")
	c := common.HexToAddress("0x03")

	capped := capTokens([]common.Address{a, b, a, c}, 2)
	if len(capped) != 2 {
		t.Fatalf("长度 = %d, want 2", len(capped))
	}
	if capped[0] != a || capped[1] != b {
		t.Fatalf("顺序或去重不对：%v", capped)
	}
}
