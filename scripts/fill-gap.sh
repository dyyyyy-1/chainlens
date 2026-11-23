#!/usr/bin/env bash
#
# 补缺口：索引器停机过久、游标滑出免费端点的归档窗口时，用它把游标拉回窗口内。
#
# 背景（docs/SESSION-HANDOFF.md §8.5、§9「缺口补齐」）：
#   publicnode 只保留最近约 10,000 块。停机超过约 1.4 天，next_block 落到窗口之外，
#   eth_getLogs 直接报 `Archive requests require a personal token`。
#   唯一的办法是用归档端点（drpc）把中间那段补上，再切回 publicnode。
#
# 这个流程 2026-11-19 与 11-23 各手工走过一次，两次都要现场回想：
#   用哪个端点、批大小调到多少、块头批量降到几、补到哪算够。所以脚本化。
#
# 用法：
#   bash scripts/fill-gap.sh              # 只判断 + 补到窗口内，结束后打印下一步
#   bash scripts/fill-gap.sh --serve      # 补完直接用 publicnode 前台运行索引器
#   bash scripts/fill-gap.sh --dry-run    # 只判断，不动任何东西
#
# 环境变量：
#   DRPC_URL    归档端点，默认 https://eth.drpc.org
#   PUBLIC_URL  免费端点，默认取 .env 的 RPC_HTTP_URL
#   SAFETY      补到「publicnode 接受」后再多补的块数，默认 500
#   POLL        轮询间隔秒数，默认 15
#   TIMEOUT     最长补缺口时间（分钟），默认 180
#
# 需要：psql、curl、go，以及 .env 里的 DATABASE_URL / TARGET_CONTRACTS / CHAIN_ID。
# 本机需要代理时，先 export HTTPS_PROXY（脚本会透传给 curl 与索引器）。

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

DRPC_URL="${DRPC_URL:-https://eth.drpc.org}"
SAFETY="${SAFETY:-500}"
POLL="${POLL:-15}"
TIMEOUT="${TIMEOUT:-180}"

SERVE=0
DRY_RUN=0
for arg in "$@"; do
	case "$arg" in
	--serve) SERVE=1 ;;
	--dry-run) DRY_RUN=1 ;;
	-h | --help)
		sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*)
		echo "未知参数：$arg（支持 --serve / --dry-run）" >&2
		exit 2
		;;
	esac
done

log() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
fail() { printf '\033[31mFAIL: %s\033[0m\n' "$*" >&2; exit 1; }
ok() { printf '\033[32m  ok  %s\033[0m\n' "$*"; }

# ---------------------------------------------------------------------------
# 配置
# ---------------------------------------------------------------------------

env_value() {
	local key="$1"
	local from_env="${!key:-}"
	if [[ -n "$from_env" ]]; then
		printf '%s' "$from_env"
		return 0
	fi

	# .env 里没有这个 key 是正常情况（例如只配了 TARGET_CONTRACT 而不是 TARGET_CONTRACTS）。
	# 但 set -e + pipefail 下，grep 无匹配会让整个赋值失败并静默退出，
	# 所以这里必须显式吞掉失败，返回空串。
	local line
	line=$(grep -E "^${key}=" .env 2>/dev/null | tail -1 || true)
	[[ -n "$line" ]] || return 0
	printf '%s' "${line#*=}" | sed 's/^"//; s/"$//'
}

DATABASE_URL="${DATABASE_URL:-$(env_value DATABASE_URL)}"
PUBLIC_URL="${PUBLIC_URL:-$(env_value RPC_HTTP_URL)}"
CHAIN_ID="${CHAIN_ID:-$(env_value CHAIN_ID)}"
TARGETS="${TARGET_CONTRACTS:-$(env_value TARGET_CONTRACTS)}"
if [[ -z "$TARGETS" ]]; then
	TARGETS="$(env_value TARGET_CONTRACT)"
fi
# 探测窗口只需要一个合约：归档限制是端点属性，与查哪个合约无关。
PROBE_CONTRACT="${TARGETS%%,*}"

[[ -n "$DATABASE_URL" ]] || fail "缺少 DATABASE_URL（环境变量或 .env）"
[[ -n "$PUBLIC_URL" ]] || fail "缺少 RPC_HTTP_URL（环境变量或 .env）"
[[ -n "$CHAIN_ID" ]] || fail "缺少 CHAIN_ID（环境变量或 .env）"
[[ -n "$PROBE_CONTRACT" ]] || fail "缺少 TARGET_CONTRACTS / TARGET_CONTRACT"

for cmd in psql curl go; do
	command -v "$cmd" >/dev/null 2>&1 || fail "缺少命令：$cmd"
done

# ---------------------------------------------------------------------------
# 工具函数
# ---------------------------------------------------------------------------

cursor() {
	psql "$DATABASE_URL" -tAc "select next_block from index_cursor" 2>/dev/null | tr -d '[:space:]'
}

hex_of() { printf '0x%x' "$1"; }

json_result_hex() {
	grep -o '"result":"0x[0-9a-fA-F]*"' | head -1 | sed 's/.*"0x//; s/"//'
}

chain_head() {
	local response hex
	response=$(curl -s -m 20 -H 'Content-Type: application/json' \
		-d '{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}' "$PUBLIC_URL")
	hex=$(printf '%s' "$response" | json_result_hex)
	[[ -n "$hex" ]] || return 1
	printf '%d' "0x$hex"
}

# probe_serves 判断免费端点是否还能提供某个块。
#   返回 0 可用 / 1 明确被归档窗口拒绝 / 2 其他错误（限流、网络），调用方应重试
probe_serves() {
	local block="$1"
	local response
	response=$(curl -s -m 25 -H 'Content-Type: application/json' \
		-d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"eth_getLogs\",\"params\":[{\"fromBlock\":\"$(hex_of "$block")\",\"toBlock\":\"$(hex_of "$block")\",\"address\":\"$PROBE_CONTRACT\"}]}" \
		"$PUBLIC_URL") || return 2

	if printf '%s' "$response" | grep -q 'Archive requests'; then
		return 1
	fi
	if printf '%s' "$response" | grep -q '"error"'; then
		return 2
	fi
	return 0
}

# ---------------------------------------------------------------------------
# 前置检查：不能有第二个实例在跑
# ---------------------------------------------------------------------------

running_indexers() {
	case "$(uname -s)" in
	MINGW* | MSYS* | CYGWIN*)
		tasklist //FI "IMAGENAME eq indexer.exe" 2>/dev/null | grep -c "indexer.exe" || true
		;;
	*) pgrep -c -f "cmd/indexer" 2>/dev/null || true ;;
	esac
}

if [[ "$(running_indexers)" -gt 0 ]]; then
	fail "已有索引器在运行。两个实例会抢游标、烧 RPC 配额，先停掉再执行本脚本。"
fi

# ---------------------------------------------------------------------------
# 判断是否需要补
# ---------------------------------------------------------------------------

log "读取状态"
NEXT=$(cursor)
[[ -n "$NEXT" ]] || fail "读不到 index_cursor.next_block（库连上了吗？跑过索引器吗？）"
HEAD=$(chain_head) || fail "查不到链头（$PUBLIC_URL）。代理是否可用？"
BEHIND=$((HEAD - NEXT))
printf '游标 next_block=%s，链头 %s，落后 %s 块\n' "$NEXT" "$HEAD" "$BEHIND"

log "探测 $PUBLIC_URL 是否还能提供块 $NEXT"
set +e
probe_serves "$NEXT"
PROBE=$?
set -e

case "$PROBE" in
0)
	ok "免费端点可用，不需要补缺口"
	printf '\n直接启动即可：\n  go run ./cmd/indexer\n'
	exit 0
	;;
1)
	ok "确认落在归档窗口之外，需要补缺口"
	;;
*)
	fail "探测失败（既不是归档拒绝，也不是正常响应）。先确认网络/代理，再重试。"
	;;
esac

if [[ "$DRY_RUN" == "1" ]]; then
	printf '\n--dry-run：需要补缺口，但未做任何改动。\n'
	exit 0
fi

# ---------------------------------------------------------------------------
# 补缺口：归档端点 + 小批量
# ---------------------------------------------------------------------------

log "检查归档端点 $DRPC_URL"
DRPC_HEAD=$(curl -s -m 20 -H 'Content-Type: application/json' \
	-d '{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}' "$DRPC_URL" | json_result_hex)
[[ -n "$DRPC_HEAD" ]] || fail "归档端点不可用：$DRPC_URL"
ok "归档端点可用（head=0x$DRPC_HEAD）"

INDEXER_LOG="$(mktemp -t chainlens-gap-XXXXXX.log)"
IDX_PID=""

cleanup() {
	if [[ -n "$IDX_PID" ]]; then
		case "$(uname -s)" in
		MINGW* | MSYS* | CYGWIN*)
			MSYS_NO_PATHCONV=1 taskkill /PID "$IDX_PID" /T /F >/dev/null 2>&1 || true
			;;
		*)
			pkill -TERM -P "$IDX_PID" 2>/dev/null || true
			kill "$IDX_PID" 2>/dev/null || true
			;;
		esac
	fi
}
trap cleanup EXIT

log "用归档端点启动索引器（100 块/批，块头批量 3）"
# drpc 免费档的两个硬约束：eth_getLogs 跨度 <= 100 块、批量块头 <= 3。
# 用默认的 2000 / 100 会第一批就直接失败。
DATABASE_URL="$DATABASE_URL" \
	RPC_HTTP_URL="$DRPC_URL" \
	CHAIN_ID="$CHAIN_ID" \
	TARGET_CONTRACTS="${TARGETS}" \
	BATCH_BLOCK_RANGE=100 \
	RPC_HEADER_BATCH=3 \
	go run ./cmd/indexer >"$INDEXER_LOG" 2>&1 &
IDX_PID=$!

START_NEXT=$NEXT
START_TS=$(date +%s)
DEADLINE=$((START_TS + TIMEOUT * 60))

while true; do
	sleep "$POLL"

	if ! kill -0 "$IDX_PID" 2>/dev/null; then
		tail -20 "$INDEXER_LOG" >&2
		fail "索引器已退出，日志见 $INDEXER_LOG"
	fi

	CURRENT=$(cursor)
	[[ -n "$CURRENT" ]] || continue

	ELAPSED=$(( $(date +%s) - START_TS ))
	RATE=$(( (CURRENT - START_NEXT) / (ELAPSED > 0 ? ELAPSED : 1) ))
	printf '  补到 %s（+%s 块，约 %s 块/秒）\n' "$CURRENT" "$((CURRENT - START_NEXT))" "$RATE"

	set +e
	probe_serves "$CURRENT"
	PROBE=$?
	set -e

	if [[ "$PROBE" == "0" ]]; then
		# 命中窗口后继续多补一段：窗口是滚动的，补的过程里它也在上移。
		TARGET=$((CURRENT + SAFETY))
		printf '  免费端点已可服务块 %s，继续补 %s 块余量到 %s\n' "$CURRENT" "$SAFETY" "$TARGET"
		while [[ "$(cursor)" -lt "$TARGET" ]]; do
			sleep "$POLL"
			if ! kill -0 "$IDX_PID" 2>/dev/null; then
				tail -20 "$INDEXER_LOG" >&2
				fail "索引器已退出，日志见 $INDEXER_LOG"
			fi
			if [[ "$(date +%s)" -ge "$DEADLINE" ]]; then
				fail "超过 ${TIMEOUT} 分钟仍未补到 $TARGET"
			fi
		done
		break
	fi

	if [[ "$(date +%s)" -ge "$DEADLINE" ]]; then
		fail "超过 ${TIMEOUT} 分钟仍未回到窗口内（当前 $CURRENT）"
	fi
done

FINAL=$(cursor)
REST=$((HEAD - FINAL))
ok "补缺口完成：$START_NEXT → $FINAL（+$((FINAL - START_NEXT)) 块），距链头 $REST 块"
printf '日志：%s\n' "$INDEXER_LOG"

# 停掉归档端点上的实例；--serve 时马上用免费端点接上。
cleanup
IDX_PID=""
sleep 2

if [[ "$SERVE" == "1" ]]; then
	log "用 $PUBLIC_URL 前台运行索引器（Ctrl-C 退出）"
	exec env DATABASE_URL="$DATABASE_URL" RPC_HTTP_URL="$PUBLIC_URL" \
		CHAIN_ID="$CHAIN_ID" TARGET_CONTRACTS="$TARGETS" go run ./cmd/indexer
fi

printf '\n下一步（切回免费端点，速度约快 8 倍）：\n  go run ./cmd/indexer\n'
printf '注意 .env 里的 RPC_HTTP_URL 必须是 %s，不要留着 drpc。\n' "$PUBLIC_URL"
