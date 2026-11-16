package decoder

import (
	"bytes"
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
)

// ChainReader 是代理识别所需的最小链上读能力，由 internal/chain 实现。
// 接口定义在使用方（HANDOFF.md 第 11 节）。
type ChainReader interface {
	StorageAt(ctx context.Context, addr common.Address, slot common.Hash) (common.Hash, error)
	CodeAt(ctx context.Context, addr common.Address) ([]byte, error)
	Call(ctx context.Context, to common.Address, data []byte) ([]byte, error)
}

// ProxyKind 是代理合约的识别结果类型。
type ProxyKind string

const (
	// ProxyNone 表示该地址不是合约，或是未匹配任何已知模式。
	ProxyNone ProxyKind = "none"
	// ProxyEIP1967 是 EIP-1967 标准实现槽，OpenZeppelin 系代理广泛使用。
	ProxyEIP1967 ProxyKind = "eip1967"
	// ProxyEIP1967Beacon 是 EIP-1967 信标代理，实现地址由信标合约动态返回。
	ProxyEIP1967Beacon ProxyKind = "eip1967_beacon"
	// ProxyZeppelinOSP 是 ZeppelinOS 遗留槽。USDC 的 FiatTokenProxy 用的是它，
	// EIP-1967 槽位读出来是全零，必须单独支持。
	ProxyZeppelinOSP ProxyKind = "zeppelin_os"
	// ProxyEIP1167 是字节码层面硬编码实现地址的最小代理（克隆合约）。
	ProxyEIP1167 ProxyKind = "eip1167"
)

// 存储槽常量。三者取值互不相同，声明顺序即探测优先级。
var (
	slotEIP1967Implementation = common.HexToHash("0x360894a13ba1a3210667c828492db98dca3e2076cc3735a920a3ca505d382bbc")
	slotEIP1967Beacon         = common.HexToHash("0xa3f0ad74e5423aebfd80d3ef4346578335a9a72aeaee59ff6cb3582b35133d50")
	slotZeppelinOS            = common.HexToHash("0x7050c9e0f4ca769c69bd3a8ef740bc37934f8e2c036e5a723fd8ee048ed3f8c3")
)

// beaconImplementationSelector 是 implementation() 的函数选择器（0x5c60da1b）。
var beaconImplementationSelector = []byte{0x5c, 0x60, 0xda, 0x1b}

// EIP-1167 最小代理的字节码模板：10 字节前缀 + 20 字节实现地址 + 15 字节后缀。
var (
	eip1167Prefix = []byte{0x36, 0x3d, 0x3d, 0x37, 0x3d, 0x3d, 0x3d, 0x36, 0x3d, 0x73}
	eip1167Suffix = []byte{0x5a, 0xf4, 0x3d, 0x82, 0x80, 0x3e, 0x90, 0x3d, 0x91, 0x60, 0x2b, 0x57, 0xfd, 0x5b, 0xf3}
)

// ProxyInfo 是代理识别的结果。
type ProxyInfo struct {
	Kind           ProxyKind
	Implementation common.Address
	// Beacon 仅在 ProxyEIP1967Beacon 时非零。
	Beacon common.Address
}

// IsProxy 表示是否识别出代理模式。
func (p ProxyInfo) IsProxy() bool {
	return p.Kind != ProxyNone
}

// Detect 依次探测四种代理模式：EIP-1967 实现槽、EIP-1967 信标槽、ZeppelinOS 遗留槽、
// EIP-1167 最小代理字节码。
//
// 先读字节码再读槽位：EOA 的任意槽位都是零，先查槽会把大量普通地址白白消耗三次请求，
// 也分不清「未识别的代理」与「根本不是合约」。
func Detect(ctx context.Context, reader ChainReader, addr common.Address) (ProxyInfo, error) {
	code, err := reader.CodeAt(ctx, addr)
	if err != nil {
		return ProxyInfo{}, fmt.Errorf("decoder: read code of %s: %w", addr.Hex(), err)
	}
	if len(code) == 0 {
		return ProxyInfo{Kind: ProxyNone}, nil
	}

	info, hasFound, err := detectBySlots(ctx, reader, addr)
	if err != nil {
		return ProxyInfo{}, err
	}
	if hasFound {
		return info, nil
	}

	return detectMinimalProxy(code), nil
}

// detectBySlots 按优先级读取三个存储槽。
func detectBySlots(ctx context.Context, reader ChainReader, addr common.Address) (ProxyInfo, bool, error) {
	implementation, err := readSlotAddress(ctx, reader, addr, slotEIP1967Implementation)
	if err != nil {
		return ProxyInfo{}, false, err
	}
	if implementation != (common.Address{}) {
		return ProxyInfo{Kind: ProxyEIP1967, Implementation: implementation}, true, nil
	}

	beacon, err := readSlotAddress(ctx, reader, addr, slotEIP1967Beacon)
	if err != nil {
		return ProxyInfo{}, false, err
	}
	if beacon != (common.Address{}) {
		resolved, err := callBeaconImplementation(ctx, reader, beacon)
		if err != nil {
			return ProxyInfo{}, false, err
		}
		return ProxyInfo{Kind: ProxyEIP1967Beacon, Implementation: resolved, Beacon: beacon}, true, nil
	}

	legacy, err := readSlotAddress(ctx, reader, addr, slotZeppelinOS)
	if err != nil {
		return ProxyInfo{}, false, err
	}
	if legacy != (common.Address{}) {
		return ProxyInfo{Kind: ProxyZeppelinOSP, Implementation: legacy}, true, nil
	}

	return ProxyInfo{}, false, nil
}

// readSlotAddress 读取槽位并取低 20 字节作为地址；零值表示该槽未被使用。
func readSlotAddress(ctx context.Context, reader ChainReader, addr common.Address, slot common.Hash) (common.Address, error) {
	raw, err := reader.StorageAt(ctx, addr, slot)
	if err != nil {
		return common.Address{}, fmt.Errorf("decoder: read slot %s of %s: %w", slot.Hex(), addr.Hex(), err)
	}
	return common.BytesToAddress(raw.Bytes()), nil
}

// callBeaconImplementation 向信标合约请求实现地址。
func callBeaconImplementation(ctx context.Context, reader ChainReader, beacon common.Address) (common.Address, error) {
	result, err := reader.Call(ctx, beacon, beaconImplementationSelector)
	if err != nil {
		return common.Address{}, fmt.Errorf("decoder: call implementation() on beacon %s: %w", beacon.Hex(), err)
	}
	if len(result) < common.HashLength {
		return common.Address{}, fmt.Errorf("decoder: beacon %s returned %d bytes, want %d", beacon.Hex(), len(result), common.HashLength)
	}
	return common.BytesToAddress(result), nil
}

// detectMinimalProxy 从字节码识别 EIP-1167 最小代理。
//
// 这类代理没有存储槽，实现地址直接编码在字节码中段，只能靠前后缀匹配定位。
func detectMinimalProxy(code []byte) ProxyInfo {
	minLength := len(eip1167Prefix) + common.AddressLength + len(eip1167Suffix)
	if len(code) < minLength {
		return ProxyInfo{Kind: ProxyNone}
	}
	if !bytes.Equal(code[:len(eip1167Prefix)], eip1167Prefix) {
		return ProxyInfo{Kind: ProxyNone}
	}

	end := len(code) - len(eip1167Suffix)
	if !bytes.Equal(code[end:], eip1167Suffix) {
		return ProxyInfo{Kind: ProxyNone}
	}

	return ProxyInfo{
		Kind:           ProxyEIP1167,
		Implementation: common.BytesToAddress(code[end-common.AddressLength : end]),
	}
}
