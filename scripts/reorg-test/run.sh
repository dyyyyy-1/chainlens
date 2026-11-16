#!/usr/bin/env bash
#
# reorg 端到端验证（HANDOFF.md 8.1）。
#
# 流程：
#   1. 用 anvil fork 主网到最近区块
#   2. 索引器扫到链头
#   3. 冒充 USDC 持有者转一笔真实转账，产生可验证的 Transfer 日志
#   4. 再挖两块，使转账块落入「最近 5 块」窗口
#   5. anvil_reorg 深度 5，改写这 5 个块
#   6. 断言：索引器回滚到分叉点、陈旧日志被删除、块头换成新哈希
#
# 用法：bash scripts/reorg-test/run.sh
# 依赖：anvil / cast（Foundry）、psql、go。使用独立数据库 chainlens_anvil，
#      不会触碰开发库 chainlens。

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

ANVIL_PORT="${ANVIL_PORT:-8545}"
RPC="http://127.0.0.1:${ANVIL_PORT}"
FORK_RPC="${FORK_RPC:-https://ethereum-rpc.publicnode.com}"
DB_NAME="${TEST_DB:-chainlens_anvil}"
PGURL="postgres://chainlens:chainlens_dev@localhost:5432/${DB_NAME}?sslmode=disable"

USDC="0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"
# Morpho Blue 在测试块高度持有约 1.41 亿 USDC，用来发起真实转账。
SENDER="0xbbbbbbbbbb9cc5e90e3b3af64bdaf62c37eeffcb"
RECIPIENT="0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
TRANSFER_AMOUNT="1000000" # 1 USDC（6 位小数）
REORG_DEPTH=5

WORK_DIR="$(mktemp -d)"
ANVIL_LOG="$WORK_DIR/anvil.log"
INDEXER_LOG="$WORK_DIR/indexer.log"
INDEXER_PID=""
ANVIL_PID=""

cleanup() {
	if [[ -n "$INDEXER_PID" ]]; then kill "$INDEXER_PID" 2>/dev/null || true; fi
	if [[ -n "$ANVIL_PID" ]]; then kill "$ANVIL_PID" 2>/dev/null || true; fi
}
trap cleanup EXIT

log() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
fail() { printf '\033[31mFAIL: %s\033[0m\n' "$*" >&2; exit 1; }
pass() { printf '\033[32m  ok  %s\033[0m\n' "$*"; }

psqlq() { PGPASSWORD=chainlens_dev psql "$PGURL" -tAc "$1"; }

require() {
	command -v "$1" >/dev/null 2>&1 || fail "缺少命令：$1（Foundry 提供 anvil 与 cast）"
}

wait_until() {
	local description="$1" timeout="$2"
	shift 2
	local deadline=$((SECONDS + timeout))
	while ((SECONDS < deadline)); do
		if "$@" >/dev/null 2>&1; then return 0; fi
		sleep 2
	done
	fail "等待超时：$description"
}

require anvil
require cast
require psql
require go

# ---------------------------------------------------------------------------
log "重置测试库 ${DB_NAME} 的 schema"
if ! psqlq "select 1" >/dev/null 2>&1; then
	fail "无法连接 ${DB_NAME}。请先创建：psql -U postgres -c \"CREATE DATABASE ${DB_NAME} OWNER chainlens;\""
fi
psqlq "DROP SCHEMA public CASCADE; CREATE SCHEMA public;" >/dev/null
for migration in migrations/*.sql; do
	PGPASSWORD=chainlens_dev psql "$PGURL" -v ON_ERROR_STOP=1 -q -f "$migration"
done
pass "migrations 已应用"

# ---------------------------------------------------------------------------
log "启动 anvil fork"
FORK_BLOCK=$(cast block-number --rpc-url "$FORK_RPC")
FORK_AT=$((FORK_BLOCK - 50))
anvil --fork-url "$FORK_RPC" --fork-block-number "$FORK_AT" --chain-id 31337 \
	--port "$ANVIL_PORT" --silent >"$ANVIL_LOG" 2>&1 &
ANVIL_PID=$!
wait_until "anvil 就绪" 60 cast block-number --rpc-url "$RPC"
pass "fork 于区块 ${FORK_AT}"

# ---------------------------------------------------------------------------
log "启动索引器"
export DATABASE_URL="$PGURL"
export RPC_HTTP_URL="$RPC"
export CHAIN_ID=31337
export TARGET_CONTRACT="$USDC"
export START_BLOCK=$((FORK_AT - 100))
export CONFIRMATIONS=12
export BATCH_BLOCK_RANGE=50
export LOG_LEVEL=info

go run ./cmd/indexer >"$INDEXER_LOG" 2>&1 &
INDEXER_PID=$!

caught_up() {
	local head next
	head=$(cast block-number --rpc-url "$RPC")
	next=$(psqlq "select next_block from index_cursor" 2>/dev/null || echo 0)
	[[ -n "$next" ]] && (( next > head ))
}
wait_until "索引器追平链头" 180 caught_up
pass "已追平，next_block=$(psqlq 'select next_block from index_cursor')"

# ---------------------------------------------------------------------------
log "制造带真实 USDC 日志的本地块"
cast rpc --rpc-url "$RPC" anvil_mine 5 >/dev/null
cast rpc --rpc-url "$RPC" anvil_impersonateAccount "$SENDER" >/dev/null
cast rpc --rpc-url "$RPC" anvil_setBalance "$SENDER" 0x56BC75E2D63100000 >/dev/null
cast send --rpc-url "$RPC" --unlocked --from "$SENDER" "$USDC" \
	"transfer(address,uint256)" "$RECIPIENT" "$TRANSFER_AMOUNT" >/dev/null
TRANSFER_BLOCK=$(cast block-number --rpc-url "$RPC")
cast rpc --rpc-url "$RPC" anvil_mine 2 >/dev/null
HEAD_AFTER=$(cast block-number --rpc-url "$RPC")
pass "转账落在区块 ${TRANSFER_BLOCK}，当前链头 ${HEAD_AFTER}"

has_transfer_log() {
	[[ "$(psqlq "select count(*) from chain_log where block_number = ${TRANSFER_BLOCK}")" == "1" ]]
}
wait_until "转账块被索引" 120 has_transfer_log
TOTAL_LOGS=$(psqlq "select count(*) from chain_log")
pass "转账块已索引，当前日志总数 ${TOTAL_LOGS}"

# ---------------------------------------------------------------------------
log "执行 anvil_reorg 深度 ${REORG_DEPTH}"
cast rpc --rpc-url "$RPC" anvil_reorg "$REORG_DEPTH" '[]' >/dev/null
FORK_POINT_MIN=$((HEAD_AFTER - REORG_DEPTH))
pass "分叉点应落在 ${FORK_POINT_MIN}..${HEAD_AFTER} 之间"

rolled_back() { grep -q "reorg rolled back" "$INDEXER_LOG"; }
wait_until "索引器检测到重组" 120 rolled_back

DETECTED_FORK=$(grep "reorg rolled back" "$INDEXER_LOG" | tail -1 | sed -n 's/.*fork_block=\([0-9]*\).*/\1/p')

# 不要求恰好等于 HEAD_AFTER - REORG_DEPTH：窗口内的空块若被按相同时间戳重建，
# 哈希不变，索引器会把它当作共同祖先，真正的分叉点因此可能更靠后。
(( DETECTED_FORK >= FORK_POINT_MIN && DETECTED_FORK <= HEAD_AFTER )) \
	|| fail "分叉点 ${DETECTED_FORK} 超出预期区间 ${FORK_POINT_MIN}..${HEAD_AFTER}"
pass "分叉点识别在预期区间内：${DETECTED_FORK}"

# 转账块必须落在分叉点之后，否则「陈旧日志被删除」这条断言失去意义。
(( TRANSFER_BLOCK > DETECTED_FORK )) \
	|| fail "转账块 ${TRANSFER_BLOCK} 不在分叉点 ${DETECTED_FORK} 之后，测试构造有误"
pass "转账块 ${TRANSFER_BLOCK} 位于分叉点之后，其日志应被清除"

# ---------------------------------------------------------------------------
log "校验回滚结果"
tail_logs() { [[ "$(psqlq "select count(*) from chain_log where block_number > ${DETECTED_FORK}")" == "0" ]]; }
wait_until "分叉点之后的陈旧日志被清除" 120 tail_logs
pass "分叉点之后日志数 = 0"

NEW_TOTAL=$(psqlq "select count(*) from chain_log")
[[ "$NEW_TOTAL" == "$((TOTAL_LOGS - 1))" ]] || fail "日志总数 ${NEW_TOTAL}，期望 $((TOTAL_LOGS - 1))"
pass "日志总数 ${TOTAL_LOGS} → ${NEW_TOTAL}（转账日志已被删除）"

LOCAL_HASH=$(psqlq "select '0x'||encode(hash,'hex') from block_header where number = ${TRANSFER_BLOCK}")
CHAIN_HASH=$(cast block "$TRANSFER_BLOCK" --field hash --rpc-url "$RPC")
[[ "${LOCAL_HASH,,}" == "${CHAIN_HASH,,}" ]] || fail "块头未更新：本地 ${LOCAL_HASH}，链上 ${CHAIN_HASH}"
pass "块头已换成新哈希 ${CHAIN_HASH}"

CURSOR_NEXT=$(psqlq "select next_block from index_cursor")
[[ "$CURSOR_NEXT" -ge "$((DETECTED_FORK + 1))" ]] || fail "游标未回退：${CURSOR_NEXT}"
pass "游标已回退至分叉点之后：${CURSOR_NEXT}"

printf '\n\033[32m全部断言通过：reorg 回滚 + 重扫结果与分叉后链一致。\033[0m\n'
