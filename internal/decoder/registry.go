package decoder

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// EventSpec 描述一个已知事件的可读信息。
type EventSpec struct {
	Name      string
	Signature string
}

// knownEvents 是本索引器内置的事件签名表。
//
// 存在理由：没有 ABI 时，topic0 仍然可以反查事件名，使 decoded_event.event_name
// 保持可读而不是一律 unknown（HANDOFF.md 8.3 的「退化到原始日志层」）。
// 哈希在包初始化时由签名字符串计算，避免手抄 32 字节常量出错。
var knownEvents = buildKnownEvents([]EventSpec{
	{Name: "Transfer", Signature: "Transfer(address,address,uint256)"},
	{Name: "Approval", Signature: "Approval(address,address,uint256)"},
	{Name: "ApprovalForAll", Signature: "ApprovalForAll(address,address,bool)"},
	{Name: "Deposit", Signature: "Deposit(address,uint256)"},
	{Name: "Withdrawal", Signature: "Withdrawal(address,uint256)"},
	{Name: "TransferSingle", Signature: "TransferSingle(address,address,address,uint256,uint256)"},
	{Name: "TransferBatch", Signature: "TransferBatch(address,address,address,uint256[],uint256[])"},
})

func buildKnownEvents(specs []EventSpec) map[common.Hash]EventSpec {
	events := make(map[common.Hash]EventSpec, len(specs))
	for _, spec := range specs {
		events[crypto.Keccak256Hash([]byte(spec.Signature))] = spec
	}
	return events
}

// Lookup 按 topic0 查询内置事件签名。
func Lookup(topic0 common.Hash) (EventSpec, bool) {
	spec, isKnown := knownEvents[topic0]
	return spec, isKnown
}
