package profile

import (
	"context"
	"math/big"
	"os"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestClampLimit(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"未指定用默认值", 0, defaultLimit},
		{"负数用默认值", -5, defaultLimit},
		{"正常值原样返回", 1, 1},
		{"等于上限", maxLimit, maxLimit},
		{"超过上限被截断", maxLimit + 1, maxLimit},
		{"极大值被截断", 1 << 20, maxLimit},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampLimit(tc.in); got != tc.want {
				t.Fatalf("clampLimit(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestTopicPadding(t *testing.T) {
	// 小地址最容易暴露填充错误：前 12 字节必须补零，不能左移。
	addr := common.HexToAddress("0x000000000004444c5dc75cb358380d2e3de08a90")
	got := topic(addr)

	if len(got) != 32 {
		t.Fatalf("topic length = %d, want 32", len(got))
	}
	for i := range 12 {
		if got[i] != 0 {
			t.Fatalf("topic[%d] = %#x, want 0（地址必须右对齐）", i, got[i])
		}
	}
	if common.BytesToAddress(got[12:]) != addr {
		t.Fatalf("topic 末尾 20 字节 = %s, want %s", common.BytesToAddress(got[12:]).Hex(), addr.Hex())
	}
}

func TestParseAmount(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"零", "0", "0", false},
		{"负数", "-2817103140587", "-2817103140587", false},
		{"uint256 上界", maxUint256.String(), maxUint256.String(), false},
		{"带小数点应失败", "1.5", "", true},
		{"空串应失败", "", "", true},
		{"非数字应失败", "12a", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseAmount(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseAmount(%q) 期望报错，实际得到 %v", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseAmount(%q) 意外报错：%v", tc.in, err)
			}
			if got.String() != tc.want {
				t.Fatalf("parseAmount(%q) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestMaxUint256(t *testing.T) {
	// 2^256-1 的十进制有 78 位，这是 ERC20 无限额度的判定基准。
	want := "115792089237316195423570985008687907853269984665640564039457584007913129639935"
	if maxUint256.String() != want {
		t.Fatalf("maxUint256 = %s, want %s", maxUint256, want)
	}
	if got := new(big.Int).Add(maxUint256, big.NewInt(1)).BitLen(); got != 257 {
		t.Fatalf("maxUint256+1 的位宽 = %d, want 257", got)
	}
}

// TestProfileAgainstLiveDatabase 是集成测试，默认跳过。
//
// 它验证的是 SQL 本身：语法、列类型、占位符顺序。这些用纯函数测不出来，
// 而 SQL 拼错时单元测试全绿、线上却一条都查不到。
//
// 运行方式：
//
//	CHAINLENS_TEST_DSN='postgres://chainlens:chainlens_dev@localhost:5432/chainlens?sslmode=disable' \
//	    go test ./internal/profile -run TestProfileAgainstLiveDatabase -v
func TestProfileAgainstLiveDatabase(t *testing.T) {
	dsn := os.Getenv("CHAINLENS_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 CHAINLENS_TEST_DSN，跳过需要数据库的集成测试")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接测试库失败：%v", err)
	}
	defer pool.Close()

	// 取一条真实 Transfer 的收款方作为样本，避免把测试绑死在某个具体地址上。
	var raw []byte
	err = pool.QueryRow(ctx,
		`SELECT topic2 FROM chain_log WHERE topic0 = $1 AND topic2 IS NOT NULL LIMIT 1`,
		transferTopic.Bytes(),
	).Scan(&raw)
	if err != nil {
		t.Fatalf("采样地址失败（库里有数据吗？）：%v", err)
	}
	addr := common.BytesToAddress(raw)

	p := New(pool)

	holdings, err := p.Holdings(ctx, addr, 10)
	if err != nil {
		t.Fatalf("Holdings 失败：%v", err)
	}
	if len(holdings) == 0 {
		t.Fatalf("Holdings 对采样地址 %s 返回空，但该地址至少有一条 Transfer", addr.Hex())
	}
	if holdings[0].Net == nil || holdings[0].LogCount == 0 {
		t.Fatalf("Holdings 结果不完整：%+v", holdings[0])
	}

	approvals, err := p.Approvals(ctx, addr, 10)
	if err != nil {
		t.Fatalf("Approvals 失败：%v", err)
	}
	for _, a := range approvals {
		want := a.Value.Cmp(maxUint256) == 0
		if a.IsUnlimited != want {
			t.Fatalf("IsUnlimited 与 Value 不一致：value=%s unlimited=%v", a.Value, a.IsUnlimited)
		}
	}

	interactions, err := p.Interactions(ctx, addr, 10)
	if err != nil {
		t.Fatalf("Interactions 失败：%v", err)
	}
	if len(interactions) == 0 {
		t.Fatalf("Interactions 对采样地址 %s 返回空", addr.Hex())
	}

	// 冷地址必须返回空而不是报错。
	cold := common.HexToAddress("0x1111111111111111111111111111111111111111")
	if got, err := p.Holdings(ctx, cold, 10); err != nil || len(got) != 0 {
		t.Fatalf("冷地址 Holdings = %v, err = %v，期望空且无错", got, err)
	}
	if got, err := p.Interactions(ctx, cold, 10); err != nil || len(got) != 0 {
		t.Fatalf("冷地址 Interactions = %v, err = %v，期望空且无错", got, err)
	}

	// LIMIT 必须真的生效，这是「禁止全量扫描」的最后一道闸。
	if got, err := p.Holdings(ctx, addr, 1); err != nil || len(got) > 1 {
		t.Fatalf("Holdings(limit=1) 返回 %d 行, err = %v", len(got), err)
	}

	// 跨路径对账：Holdings 走 chain_log 的原始 topic + data，
	// 这里用 decoded_event 的十进制 payload 独立算一遍。
	// 两条路径共用同一批日志但换算方式不同，任何一侧出错都会暴露。
	// 仅在单合约索引下成立（decoded_event 不带 contract 列）。
	if len(holdings) == 1 {
		// payload 里的地址在解码时已统一转小写（ADR-4），
		// 而 common.Address.Hex() 返回 EIP-55 混合大小写，必须转换后再比较。
		lower := strings.ToLower(addr.Hex())

		var want string
		err := pool.QueryRow(ctx, `
            SELECT round(coalesce(sum(CASE
                     WHEN payload->>'from' = payload->>'to' THEN 0
                     WHEN payload->>'from' = $1 THEN -(payload->>'value')::numeric
                     ELSE (payload->>'value')::numeric
                   END), 0))::text
            FROM decoded_event
            WHERE event_name = 'Transfer' AND status = 'decoded'
              AND (payload->>'from' = $1 OR payload->>'to' = $1)`,
			lower,
		).Scan(&want)
		if err != nil {
			t.Fatalf("decoded_event 对账查询失败：%v", err)
		}
		if holdings[0].Net.String() != want {
			t.Fatalf("跨路径净额不一致：chain_log=%s, decoded_event=%s", holdings[0].Net, want)
		}
	}
}
