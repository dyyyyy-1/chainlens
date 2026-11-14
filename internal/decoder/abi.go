// Package decoder 把 chain_log 中的原始日志解码为结构化事件。
//
// 解码是派生数据，原始日志才是事实：任何解码失败都只会让该项以 undecodable 状态入库，
// 绝不中断索引（HANDOFF.md 8.3）。
package decoder

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// 状态值，与 decoded_event.status 的 CHECK 约束保持一致。
const (
	StatusDecoded     = "decoded"
	StatusUndecodable = "undecodable"
)

// unknownEventName 用于 topic0 既不在 ABI 也不在注册表中的日志。
const unknownEventName = "unknown"

// 未解码的原因。仅用于日志分级，不入库：库里的区分依据是 status 列。
const (
	// ReasonNoABI 表示 topic0 在 ABI 中找不到对应事件，通常是没有该合约的 ABI。
	ReasonNoABI = "no_abi"
	// ReasonAnonymous 表示匹配到 anonymous 事件，其 topic0 不是签名，无法按 topic 解码。
	ReasonAnonymous = "anonymous"
	// ReasonTooManyIndexed 表示 indexed 参数超过 3 个，超出部分已被哈希进 data。
	ReasonTooManyIndexed = "too_many_indexed"
	// ReasonMalformed 表示 ABI 命中但 topics 或 data 长度不合法。
	ReasonMalformed = "malformed"
)

// maxIndexedArgs 是单个日志能承载的最大 indexed 参数数。
// EVM 一个日志最多 4 个 topic，topic0 固定为签名，剩下 3 个给 indexed 参数。
const maxIndexedArgs = 3

// ERC20ABI 是内置的 ERC20 事件 ABI，作为没有外部 ABI 时的默认值。
const ERC20ABI = `[
  {"type":"event","name":"Transfer","anonymous":false,"inputs":[
    {"name":"from","type":"address","indexed":true},
    {"name":"to","type":"address","indexed":true},
    {"name":"value","type":"uint256","indexed":false}]},
  {"type":"event","name":"Approval","anonymous":false,"inputs":[
    {"name":"owner","type":"address","indexed":true},
    {"name":"spender","type":"address","indexed":true},
    {"name":"value","type":"uint256","indexed":false}]}
]`

// Event 是一条日志的解码结果，对应 decoded_event 一行。
type Event struct {
	TxHash   common.Hash
	LogIndex uint32
	Name     string
	Status   string
	Payload  map[string]any
	// Reason 仅在 Status 为 undecodable 时有值，说明未解码的原因，用于日志分级。
	Reason string
}

// IsDecoded 表示参数是否成功解出。
func (e Event) IsDecoded() bool {
	return e.Status == StatusDecoded
}

// Decoder 按 ABI 把日志解为结构化事件。
type Decoder struct {
	abi       *abi.ABI
	byTopic   map[common.Hash]*abi.Event
	anonymous map[common.Hash]struct{}
}

// New 从 ABI JSON 构造解码器。
//
// anonymous 事件不会进入 topic0 索引：它们的 topic0 不是签名，按 topic 匹配必然是错的，
// 这类事件统一落为 undecodable（HANDOFF.md 8.3）。但仍记录其 ID，
// 以便把「确实是 anonymous 事件」与「压根没有这份 ABI」区分开。
func New(abiJSON []byte) (*Decoder, error) {
	parsed, err := abi.JSON(bytes.NewReader(abiJSON))
	if err != nil {
		return nil, fmt.Errorf("decoder: parse abi: %w", err)
	}

	byTopic := make(map[common.Hash]*abi.Event, len(parsed.Events))
	anonymous := make(map[common.Hash]struct{})
	for _, event := range parsed.Events {
		if event.Anonymous {
			anonymous[event.ID] = struct{}{}
			continue
		}
		byTopic[event.ID] = &event
	}

	return &Decoder{abi: &parsed, byTopic: byTopic, anonymous: anonymous}, nil
}

// NewFromFile 从磁盘上的 ABI JSON 文件构造解码器。
//
// 预留给 W3：Etherscan 拉到的 ABI 落盘后由这里加载，本阶段不做自动拉取。
func NewFromFile(path string) (*Decoder, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("decoder: read abi %s: %w", path, err)
	}
	decoder, err := New(content)
	if err != nil {
		return nil, fmt.Errorf("decoder: abi %s: %w", path, err)
	}
	return decoder, nil
}

// Decode 解码单条日志。
//
// 返回值恒非 nil，调用方用 Status 判断是否成功。刻意不返回 error：解码失败属于
// 预期内的情况，需要连同 undecodable 状态一起入库，而不是让整批中断。
func (d *Decoder) Decode(entry types.Log) Event {
	topic0 := topic0Of(entry)

	result := Event{
		TxHash:   entry.TxHash,
		LogIndex: uint32(entry.Index),
		Name:     unknownEventName,
		Status:   StatusUndecodable,
		Payload:  map[string]any{},
	}

	// 先尝试用内置注册表给出可读的事件名，即使没有 ABI 也有部分信息。
	if spec, isKnown := Lookup(topic0); isKnown {
		result.Name = spec.Name
	}

	event, hasEvent := d.byTopic[topic0]
	if !hasEvent {
		if _, isAnonymous := d.anonymous[topic0]; isAnonymous {
			result.Reason = ReasonAnonymous
		} else {
			result.Reason = ReasonNoABI
		}
		return result
	}
	if countIndexed(event.Inputs) > maxIndexedArgs {
		// 超出部分的 indexed 参数会被哈希进 data，不可逆，无法还原。
		result.Reason = ReasonTooManyIndexed
		return result
	}

	payload, err := d.unpack(event, entry)
	if err != nil {
		result.Reason = ReasonMalformed
		return result
	}

	result.Name = event.Name
	result.Status = StatusDecoded
	result.Payload = payload
	return result
}

// unpack 分别从 topics 与 data 还原参数。
func (d *Decoder) unpack(event *abi.Event, entry types.Log) (payload map[string]any, err error) {
	// abi 包在畸形输入上存在 panic 路径，这里兜底以满足「不 panic」的要求。
	defer func() {
		if recovered := recover(); recovered != nil {
			payload = nil
			err = fmt.Errorf("decoder: unpack %s panicked: %v", event.Name, recovered)
		}
	}()

	payload = make(map[string]any, len(event.Inputs))

	indexed := make(abi.Arguments, 0, len(event.Inputs))
	nonIndexed := 0
	for _, input := range event.Inputs {
		if input.Indexed {
			indexed = append(indexed, input)
			continue
		}
		nonIndexed++
	}

	if len(indexed) > 0 {
		// topics[0] 是事件签名，indexed 参数从 topics[1] 开始。
		if err := abi.ParseTopicsIntoMap(payload, indexed, entry.Topics[1:]); err != nil {
			return nil, fmt.Errorf("decoder: parse indexed topics of %s: %w", event.Name, err)
		}
	}

	if nonIndexed > 0 {
		if err := d.abi.UnpackIntoMap(payload, event.Name, entry.Data); err != nil {
			return nil, fmt.Errorf("decoder: unpack data of %s: %w", event.Name, err)
		}
	}

	for key, value := range payload {
		payload[key] = normalize(value)
	}

	return payload, nil
}

// normalize 把 ABI 解出的 Go 值转成可 JSON 序列化的形态。
//
// big.Int 必须转字符串：直接交给 encoding/json 会输出数字字面量，消费端在
// JavaScript 里读 uint256 会丢精度。地址与哈希统一小写 0x 十六进制，遵循 ADR-4
// 「对外输出统一小写 hex」的约定，避免 EIP-55 校验和带来的大小写不一致。
func normalize(value any) any {
	switch typed := value.(type) {
	case *big.Int:
		return typed.String()
	case common.Address:
		return strings.ToLower(typed.Hex())
	case common.Hash:
		return strings.ToLower(typed.Hex())
	case [32]byte:
		return "0x" + hex.EncodeToString(typed[:])
	case []byte:
		return "0x" + hex.EncodeToString(typed)
	case []any:
		items := make([]any, len(typed))
		for i, item := range typed {
			items[i] = normalize(item)
		}
		return items
	default:
		return value
	}
}

func countIndexed(inputs abi.Arguments) int {
	count := 0
	for _, input := range inputs {
		if input.Indexed {
			count++
		}
	}
	return count
}

func topic0Of(entry types.Log) common.Hash {
	if len(entry.Topics) == 0 {
		return common.Hash{}
	}
	return entry.Topics[0]
}
