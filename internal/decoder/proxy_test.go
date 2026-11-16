package decoder

import (
	"context"
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// fakeReader 让代理识别的分支可以完全脱离网络测试。
type fakeReader struct {
	code       []byte
	slots      map[common.Hash]common.Hash
	callResult []byte
	callErr    error

	storageCalls int
	codeCalls    int
}

func (f *fakeReader) StorageAt(_ context.Context, _ common.Address, slot common.Hash) (common.Hash, error) {
	f.storageCalls++
	return f.slots[slot], nil
}

func (f *fakeReader) CodeAt(_ context.Context, _ common.Address) ([]byte, error) {
	f.codeCalls++
	return f.code, nil
}

func (f *fakeReader) Call(_ context.Context, _ common.Address, _ []byte) ([]byte, error) {
	if f.callErr != nil {
		return nil, f.callErr
	}
	return f.callResult, nil
}

var testAddress = common.HexToAddress("0x1234567890123456789012345678901234567890")

func TestDetectReportsEOAWithoutTouchingSlots(t *testing.T) {
	t.Parallel()

	reader := &fakeReader{code: nil}

	info, err := Detect(context.Background(), reader, testAddress)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if info.Kind != ProxyNone {
		t.Fatalf("kind = %q, want none", info.Kind)
	}
	// EOA 必须只读一次字节码，不浪费三次存储读取。
	if reader.storageCalls != 0 {
		t.Fatalf("storage calls = %d, want 0", reader.storageCalls)
	}
}

func TestDetectEIP1967(t *testing.T) {
	t.Parallel()

	implementation := common.HexToAddress("0xaaaa567890123456789012345678901234567890")
	reader := &fakeReader{
		code:  []byte{0x60, 0x80},
		slots: map[common.Hash]common.Hash{slotEIP1967Implementation: common.BytesToHash(implementation.Bytes())},
	}

	info, err := Detect(context.Background(), reader, testAddress)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if info.Kind != ProxyEIP1967 {
		t.Fatalf("kind = %q, want %q", info.Kind, ProxyEIP1967)
	}
	if info.Implementation != implementation {
		t.Fatalf("implementation = %s, want %s", info.Implementation.Hex(), implementation.Hex())
	}
}

func TestDetectEIP1967Beacon(t *testing.T) {
	t.Parallel()

	beacon := common.HexToAddress("0xbbbb567890123456789012345678901234567890")
	implementation := common.HexToAddress("0xcccc567890123456789012345678901234567890")
	reader := &fakeReader{
		code:       []byte{0x60, 0x80},
		slots:      map[common.Hash]common.Hash{slotEIP1967Beacon: common.BytesToHash(beacon.Bytes())},
		callResult: common.LeftPadBytes(implementation.Bytes(), common.HashLength),
	}

	info, err := Detect(context.Background(), reader, testAddress)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if info.Kind != ProxyEIP1967Beacon {
		t.Fatalf("kind = %q, want %q", info.Kind, ProxyEIP1967Beacon)
	}
	if info.Beacon != beacon {
		t.Fatalf("beacon = %s, want %s", info.Beacon.Hex(), beacon.Hex())
	}
	if info.Implementation != implementation {
		t.Fatalf("implementation = %s, want %s", info.Implementation.Hex(), implementation.Hex())
	}
}

// USDC 的 FiatTokenProxy 就属于这一类：EIP-1967 槽全零，只认遗留槽。
func TestDetectZeppelinOSLegacy(t *testing.T) {
	t.Parallel()

	implementation := common.HexToAddress("0x43506849d7c04f9138d1a2050bbf3a0c054402dd")
	reader := &fakeReader{
		code:  []byte{0x60, 0x80},
		slots: map[common.Hash]common.Hash{slotZeppelinOS: common.BytesToHash(implementation.Bytes())},
	}

	info, err := Detect(context.Background(), reader, testAddress)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if info.Kind != ProxyZeppelinOSP {
		t.Fatalf("kind = %q, want %q", info.Kind, ProxyZeppelinOSP)
	}
	if info.Implementation != implementation {
		t.Fatalf("implementation = %s, want %s", info.Implementation.Hex(), implementation.Hex())
	}
}

func TestDetectEIP1167MinimalProxy(t *testing.T) {
	t.Parallel()

	implementation := common.HexToAddress("0xdddd567890123456789012345678901234567890")
	code := make([]byte, 0, 45)
	code = append(code, eip1167Prefix...)
	code = append(code, implementation.Bytes()...)
	code = append(code, eip1167Suffix...)

	reader := &fakeReader{code: code}

	info, err := Detect(context.Background(), reader, testAddress)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if info.Kind != ProxyEIP1167 {
		t.Fatalf("kind = %q, want %q", info.Kind, ProxyEIP1167)
	}
	if info.Implementation != implementation {
		t.Fatalf("implementation = %s, want %s", info.Implementation.Hex(), implementation.Hex())
	}
}

// 普通合约（有字节码、三个槽全零、字节码不匹配最小代理模板）不应被误判。
func TestDetectPlainContract(t *testing.T) {
	t.Parallel()

	reader := &fakeReader{code: []byte{0x60, 0x80, 0x60, 0x40, 0x52}}

	info, err := Detect(context.Background(), reader, testAddress)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if info.Kind != ProxyNone {
		t.Fatalf("kind = %q, want none", info.Kind)
	}
	if info.IsProxy() {
		t.Fatalf("IsProxy() = true, want false")
	}
	// 三个槽都要探测过，才能下「不是代理」的结论。
	if reader.storageCalls != 3 {
		t.Fatalf("storage calls = %d, want 3", reader.storageCalls)
	}
}

func TestDetectBeaconCallFailureSurfaces(t *testing.T) {
	t.Parallel()

	beacon := common.HexToAddress("0xeeee567890123456789012345678901234567890")
	reader := &fakeReader{
		code:    []byte{0x60, 0x80},
		slots:   map[common.Hash]common.Hash{slotEIP1967Beacon: common.BytesToHash(beacon.Bytes())},
		callErr: errors.New("execution reverted"),
	}

	if _, err := Detect(context.Background(), reader, testAddress); err == nil {
		t.Fatal("Detect succeeded, want error from beacon call")
	}
}
