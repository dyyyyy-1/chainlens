package decoder

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// 以下用例逐条覆盖 HANDOFF.md 8.3 要求的四种解码边界。

const (
	anonymousABI = `[{"type":"event","name":"Anon","anonymous":true,"inputs":[
		{"name":"who","type":"address","indexed":true}]}]`

	fourIndexedABI = `[{"type":"event","name":"TooManyIndexed","anonymous":false,"inputs":[
		{"name":"a","type":"address","indexed":true},
		{"name":"b","type":"address","indexed":true},
		{"name":"c","type":"address","indexed":true},
		{"name":"d","type":"address","indexed":true}]}]`

	emptyABI = `[]`

	approvalABI = `[{"type":"event","name":"Approval","anonymous":false,"inputs":[
		{"name":"owner","type":"address","indexed":true},
		{"name":"spender","type":"address","indexed":true},
		{"name":"value","type":"uint256","indexed":false}]}]`
)

var (
	transferTopic = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))
	approvalTopic = crypto.Keccak256Hash([]byte("Approval(address,address,uint256)"))
	anonTopic     = crypto.Keccak256Hash([]byte("Anon(address)"))
	tooManyTopic  = crypto.Keccak256Hash([]byte("TooManyIndexed(address,address,address,address)"))
)

func TestDecodeTransfer(t *testing.T) {
	t.Parallel()

	decoder := mustDecoder(t, ERC20ABI)
	from := common.HexToAddress("0x1111111111111111111111111111111111111111")
	to := common.HexToAddress("0x2222222222222222222222222222222222222222")
	amount := big.NewInt(1_234_567_890)

	event := decoder.Decode(types.Log{
		TxHash: common.HexToHash("0xabc1"),
		Index:  7,
		Topics: []common.Hash{
			transferTopic,
			common.BytesToHash(from.Bytes()),
			common.BytesToHash(to.Bytes()),
		},
		Data: common.LeftPadBytes(amount.Bytes(), 32),
	})

	if !event.IsDecoded() {
		t.Fatalf("status = %q, want %q", event.Status, StatusDecoded)
	}
	if event.Name != "Transfer" {
		t.Fatalf("name = %q, want Transfer", event.Name)
	}
	if event.LogIndex != 7 {
		t.Fatalf("log index = %d, want 7", event.LogIndex)
	}
	if got := event.Payload["from"]; got != from.Hex() {
		t.Fatalf("from = %v, want %s", got, from.Hex())
	}
	if got := event.Payload["to"]; got != to.Hex() {
		t.Fatalf("to = %v, want %s", got, to.Hex())
	}
	// uint256 必须以十进制字符串出现，不能是数字字面量，否则 JS 侧会丢精度。
	if got, isString := event.Payload["value"].(string); !isString || got != "1234567890" {
		t.Fatalf("value = %#v, want string \"1234567890\"", event.Payload["value"])
	}
}

func TestDecodeApproval(t *testing.T) {
	t.Parallel()

	decoder := mustDecoder(t, approvalABI)
	owner := common.HexToAddress("0x3333333333333333333333333333333333333333")
	spender := common.HexToAddress("0x4444444444444444444444444444444444444444")

	event := decoder.Decode(types.Log{
		TxHash: common.HexToHash("0xabc2"),
		Topics: []common.Hash{
			approvalTopic,
			common.BytesToHash(owner.Bytes()),
			common.BytesToHash(spender.Bytes()),
		},
		Data: common.LeftPadBytes(big.NewInt(0).Bytes(), 32),
	})

	if !event.IsDecoded() {
		t.Fatalf("status = %q, want %q", event.Status, StatusDecoded)
	}
	if event.Name != "Approval" {
		t.Fatalf("name = %q, want Approval", event.Name)
	}
	if got := event.Payload["value"]; got != "0" {
		t.Fatalf("value = %#v, want \"0\"", got)
	}
}

// 边界一：anonymous 事件没有 topic0 签名，不能按 topic 匹配，只能落为 undecodable。
func TestDecodeAnonymousEventStaysUndecodable(t *testing.T) {
	t.Parallel()

	decoder := mustDecoder(t, anonymousABI)
	who := common.HexToAddress("0x5555555555555555555555555555555555555555")

	event := decoder.Decode(types.Log{
		TxHash: common.HexToHash("0xabc3"),
		Topics: []common.Hash{anonTopic, common.BytesToHash(who.Bytes())},
	})

	if event.IsDecoded() {
		t.Fatalf("anonymous event must not decode, got status %q", event.Status)
	}
	if event.Status != StatusUndecodable {
		t.Fatalf("status = %q, want %q", event.Status, StatusUndecodable)
	}
	if event.Reason != ReasonAnonymous {
		t.Fatalf("reason = %q, want %q", event.Reason, ReasonAnonymous)
	}
	if len(event.Payload) != 0 {
		t.Fatalf("payload = %v, want empty", event.Payload)
	}
}

// 边界二：indexed 参数超过 3 个，超出部分被哈希进 data，不可逆。
func TestDecodeTooManyIndexedArgs(t *testing.T) {
	t.Parallel()

	decoder := mustDecoder(t, fourIndexedABI)
	topic := common.HexToHash("0x01")

	event := decoder.Decode(types.Log{
		TxHash: common.HexToHash("0xabc4"),
		Topics: []common.Hash{
			tooManyTopic,
			topic, topic, topic, topic,
		},
	})

	if event.IsDecoded() {
		t.Fatalf("4 indexed args must not decode, got status %q", event.Status)
	}
	if event.Reason != ReasonTooManyIndexed {
		t.Fatalf("reason = %q, want %q", event.Reason, ReasonTooManyIndexed)
	}
}

// 边界三：ABI 缺失。事件名仍能靠内置注册表还原，但参数解不出。
func TestDecodeWithoutABIFallsBackToRegistry(t *testing.T) {
	t.Parallel()

	decoder := mustDecoder(t, emptyABI)
	from := common.HexToAddress("0x6666666666666666666666666666666666666666")

	event := decoder.Decode(types.Log{
		TxHash: common.HexToHash("0xabc5"),
		Topics: []common.Hash{
			transferTopic,
			common.BytesToHash(from.Bytes()),
			common.BytesToHash(from.Bytes()),
		},
		Data: common.LeftPadBytes(big.NewInt(1).Bytes(), 32),
	})

	if event.IsDecoded() {
		t.Fatalf("log without abi must not decode, got status %q", event.Status)
	}
	if event.Name != "Transfer" {
		t.Fatalf("name = %q, want Transfer from registry", event.Name)
	}
	if event.Reason != ReasonNoABI {
		t.Fatalf("reason = %q, want %q", event.Reason, ReasonNoABI)
	}
}

// 边界三补充：topic0 既不在 ABI 也不在注册表。
func TestDecodeUnknownEventName(t *testing.T) {
	t.Parallel()

	decoder := mustDecoder(t, ERC20ABI)

	event := decoder.Decode(types.Log{
		TxHash: common.HexToHash("0xabc6"),
		Topics: []common.Hash{crypto.Keccak256Hash([]byte("Mystery(uint256)"))},
	})

	if event.IsDecoded() {
		t.Fatalf("unknown event must not decode")
	}
	if event.Name != "unknown" {
		t.Fatalf("name = %q, want unknown", event.Name)
	}
}

// 边界四：data 长度不合法，必须返回 undecodable 而不是 panic。
func TestDecodeMalformedDataDoesNotPanic(t *testing.T) {
	t.Parallel()

	decoder := mustDecoder(t, ERC20ABI)
	from := common.HexToAddress("0x7777777777777777777777777777777777777777")

	event := decoder.Decode(types.Log{
		TxHash: common.HexToHash("0xabc7"),
		Topics: []common.Hash{
			transferTopic,
			common.BytesToHash(from.Bytes()),
			common.BytesToHash(from.Bytes()),
		},
		Data: nil, // Transfer 需要 32 字节的 value
	})

	if event.IsDecoded() {
		t.Fatalf("malformed data must not decode, got status %q", event.Status)
	}
	if event.Reason != ReasonMalformed {
		t.Fatalf("reason = %q, want %q", event.Reason, ReasonMalformed)
	}
}

func mustDecoder(t *testing.T, abiJSON string) *Decoder {
	t.Helper()

	decoder, err := New([]byte(abiJSON))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return decoder
}
