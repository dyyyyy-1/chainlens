package chain

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
)

// ERC-20 只读方法的 4 字节选择器。
//
// 手写选择器而不是解析 ABI JSON：这里只用到两个固定的无参/单参方法，
// 引入 abi 包解析整份 USDC ABI 反而是更大的依赖面。
var (
	selectorBalanceOf = [4]byte{0x70, 0xa0, 0x82, 0x31} // balanceOf(address)
	selectorDecimals  = [4]byte{0x31, 0x3c, 0xe5, 0x67} // decimals()
)

// wordLen 是 ABI 编码中一个 32 字节字长。
const wordLen = 32

// BalanceOf 调用 ERC-20 合约的 balanceOf(address)，返回原始整数（未按 decimals 换算）。
//
// 与 Client.Call 的区别是**不重试**：本方法服务于在线查询，调用方给的上下文
// 只有几秒预算，重试一次就会把「快速失败并降级」变成「拖垮整个响应」。
// 需要重试的是索引器的批量拉取，不是单个用户的余额读取。
func (c *Client) BalanceOf(ctx context.Context, token, holder common.Address) (*big.Int, error) {
	data := make([]byte, 4+wordLen)
	copy(data, selectorBalanceOf[:])
	// 地址左补 12 字节零，占满 32 字节字。
	copy(data[4+wordLen-len(holder.Bytes()):], holder.Bytes())

	output, err := c.callOnce(ctx, token, data)
	if err != nil {
		return nil, fmt.Errorf("chain: balanceOf %s on %s: %w", holder.Hex(), token.Hex(), err)
	}
	if len(output) < wordLen {
		return nil, fmt.Errorf("chain: balanceOf %s on %s: got %d bytes, want >= %d", holder.Hex(), token.Hex(), len(output), wordLen)
	}

	return new(big.Int).SetBytes(output[:wordLen]), nil
}

// Decimals 调用 ERC-20 合约的 decimals()。
//
// 返回值用于前端换算：不同代币的小数位不同，写死 6 位会让金额差 10^N。
func (c *Client) Decimals(ctx context.Context, token common.Address) (uint8, error) {
	output, err := c.callOnce(ctx, token, selectorDecimals[:])
	if err != nil {
		return 0, fmt.Errorf("chain: decimals of %s: %w", token.Hex(), err)
	}
	if len(output) < wordLen {
		return 0, fmt.Errorf("chain: decimals of %s: got %d bytes, want >= %d", token.Hex(), len(output), wordLen)
	}

	value := new(big.Int).SetBytes(output[:wordLen])
	// uint8 的合法范围是 0..255。超出说明返回的不是 decimals()，宁可报错也不截断。
	if !value.IsUint64() || value.Uint64() > 255 {
		return 0, fmt.Errorf("chain: decimals of %s: out of range: %s", token.Hex(), value)
	}

	return uint8(value.Uint64()), nil
}

// callOnce 执行一次 eth_call，不重试、不派生超时（由调用方的 ctx 决定预算）。
func (c *Client) callOnce(ctx context.Context, to common.Address, data []byte) ([]byte, error) {
	return c.eth.CallContract(ctx, ethereum.CallMsg{To: &to, Data: data}, nil)
}
