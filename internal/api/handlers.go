package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/dyyyyy-1/chainlens/internal/profile"
	"github.com/dyyyyy-1/chainlens/internal/store"
)

const (
	// defaultPageSize 与 maxPageSize 收敛分页大小。
	//
	// 上限存在的意义与画像查询一致：不让一次请求把整段区间的事件拉走。
	defaultPageSize = 50
	maxPageSize     = 200
)

// responseDTO 是所有成功响应的外层结构。
//
// coverage 随每个响应返回，而不是只在 /api/stats 里给：单看某个地址的空结果，
// 无法区分「该地址确实没有记录」与「这段历史还没索引」。
type responseDTO struct {
	Coverage coverageDTO `json:"coverage"`
	Data     any         `json:"data"`
}

// eventsResponseDTO 多带一个 next_cursor。
//
// 游标只在「这一页填满了」时给出：返回不足 limit 说明已经到底，客户端不该再请求。
type eventsResponseDTO struct {
	Coverage   coverageDTO `json:"coverage"`
	Data       any         `json:"data"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

type coverageDTO struct {
	FromBlock       uint64 `json:"from_block"`
	ToBlock         uint64 `json:"to_block"`
	FinalizedBlock  uint64 `json:"finalized_block"`
	NextBlock       uint64 `json:"next_block"`
	LatestBlockTime string `json:"latest_block_time,omitempty"`
}

type errorDTO struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type errorResponseDTO struct {
	Error errorDTO `json:"error"`
}

type eventDTO struct {
	BlockNumber uint64          `json:"block_number"`
	BlockTime   string          `json:"block_time"`
	TxHash      string          `json:"tx_hash"`
	LogIndex    uint32          `json:"log_index"`
	Contract    string          `json:"contract"`
	Event       string          `json:"event,omitempty"`
	Status      string          `json:"status"`
	Payload     json.RawMessage `json:"payload,omitempty"`
}

type holdingDTO struct {
	Contract string `json:"contract"`
	Net      string `json:"net"`
	LogCount int64  `json:"log_count"`
}

// balancesResponseDTO 在通用信封之外多给一个 onchain 块。
//
// 余额是**可选**能力：查询服务不一定配置了 RPC。用 enabled=false 明确表达
// 「没有这个能力」，而不是回 0 让人误读成「余额为零」。
type balancesResponseDTO struct {
	Coverage coverageDTO  `json:"coverage"`
	Data     []holdingDTO `json:"data"`
	Onchain  onchainDTO   `json:"onchain"`
}

type onchainDTO struct {
	Enabled  bool                         `json:"enabled"`
	Balances map[string]onchainBalanceDTO `json:"balances,omitempty"`
	// Error 只在所有代币都失败时给出，避免调用方逐条翻 balances 才知道 RPC 挂了。
	Error string `json:"error,omitempty"`
}

type onchainBalanceDTO struct {
	// Value 是原始整数（未按 decimals 换算），失败时为空。
	Value string `json:"value,omitempty"`
	// Decimals 为 null 表示未知：调用方不得拿默认值猜，否则金额会差 10^N。
	Decimals  *int   `json:"decimals"`
	FetchedAt string `json:"fetched_at,omitempty"`
	Cached    bool   `json:"cached"`
	Status    string `json:"status"`
}

type approvalDTO struct {
	Contract    string `json:"contract"`
	Spender     string `json:"spender"`
	Value       string `json:"value"`
	IsUnlimited bool   `json:"is_unlimited"`
	BlockNumber uint64 `json:"block_number"`
	BlockTime   string `json:"block_time"`
	TxHash      string `json:"tx_hash"`
}

type interactionDTO struct {
	Contract   string `json:"contract"`
	EventCount int64  `json:"event_count"`
	LastBlock  uint64 `json:"last_block"`
	LastTime   string `json:"last_time"`
}

type statsDTO struct {
	TotalLogs        int64  `json:"total_logs"`
	DecodedCount     int64  `json:"decoded_count"`
	UndecodableCount int64  `json:"undecodable_count"`
	StaleSeconds     *int64 `json:"stale_seconds"`
}

func (d Deps) handleEvents(w http.ResponseWriter, r *http.Request) {
	query, ok := parseEventQuery(w, r)
	if !ok {
		return
	}

	events, err := d.Events.ListEvents(r.Context(), query)
	if err != nil {
		d.fail(w, r, err)
		return
	}

	items := make([]eventDTO, 0, len(events))
	for _, event := range events {
		items = append(items, eventDTO{
			BlockNumber: event.BlockNumber,
			BlockTime:   formatTime(event.BlockTime),
			TxHash:      hexBytes(event.TxHash.Bytes()),
			LogIndex:    event.LogIndex,
			Contract:    hexAddress(event.Contract),
			Event:       event.EventName,
			Status:      event.Status,
			Payload:     json.RawMessage(event.Payload),
		})
	}

	coverage, err := d.Events.Coverage(r.Context())
	if err != nil {
		d.fail(w, r, err)
		return
	}

	body := eventsResponseDTO{Coverage: toCoverageDTO(coverage), Data: items}
	// 只有填满一页才给下一页游标：不足 limit 说明确实到底了。
	if query.Limit > 0 && len(events) == query.Limit {
		last := events[len(events)-1]
		body.NextCursor = fmt.Sprintf("%d:%d", last.BlockNumber, last.LogIndex)
	}

	writeJSON(w, http.StatusOK, body)
}

func (d Deps) handleBalances(w http.ResponseWriter, r *http.Request) {
	addr, ok := addressFromPath(w, r)
	if !ok {
		return
	}
	limit, ok := limitFromQuery(w, r)
	if !ok {
		return
	}

	holdings, err := d.Profile.Holdings(r.Context(), addr, limit)
	if err != nil {
		d.fail(w, r, err)
		return
	}

	items := make([]holdingDTO, 0, len(holdings))
	for _, holding := range holdings {
		items = append(items, holdingDTO{
			Contract: hexAddress(holding.Contract),
			Net:      holding.Net.String(),
			LogCount: holding.LogCount,
		})
	}

	coverage, err := d.Events.Coverage(r.Context())
	if err != nil {
		d.fail(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, balancesResponseDTO{
		Coverage: toCoverageDTO(coverage),
		Data:     items,
		Onchain:  d.onchainSnapshot(r.Context(), addr, holdings),
	})
}

// onchainSnapshot 组装链上余额块；未配置 RPC 时只给 enabled=false。
func (d Deps) onchainSnapshot(ctx context.Context, holder common.Address, holdings []profile.Holding) onchainDTO {
	if d.balance == nil {
		return onchainDTO{Enabled: false}
	}

	snapshot := d.balance.snapshot(ctx, holder, holdingContracts(holdings))
	balances := make(map[string]onchainBalanceDTO, len(snapshot))
	failed := 0

	for contract, entry := range snapshot {
		dto := onchainBalanceDTO{
			Status:    "ok",
			FetchedAt: formatTime(entry.FetchedAt),
			Cached:    entry.Cached,
			Decimals:  entry.Decimals,
		}

		if entry.Err != "" {
			dto.Status = entry.Err
			failed++
			// 原始错误只进日志：它可能含 RPC 端点与内部提示，不该出现在响应里。
			d.logger().Warn("onchain balance unavailable",
				"holder", hexAddress(holder),
				"token", contract,
				"err", entry.cause,
			)
		} else {
			dto.Value = entry.Value.String()
		}

		balances[contract] = dto
	}

	result := onchainDTO{Enabled: true, Balances: balances}
	if len(snapshot) > 0 && failed == len(snapshot) {
		result.Error = "rpc_unavailable"
	}
	return result
}

func (d Deps) handleApprovals(w http.ResponseWriter, r *http.Request) {
	addr, ok := addressFromPath(w, r)
	if !ok {
		return
	}
	limit, ok := limitFromQuery(w, r)
	if !ok {
		return
	}

	approvals, err := d.Profile.Approvals(r.Context(), addr, limit)
	if err != nil {
		d.fail(w, r, err)
		return
	}

	items := make([]approvalDTO, 0, len(approvals))
	for _, approval := range approvals {
		items = append(items, approvalDTO{
			Contract:    hexAddress(approval.Contract),
			Spender:     hexAddress(approval.Spender),
			Value:       approval.Value.String(),
			IsUnlimited: approval.IsUnlimited,
			BlockNumber: approval.BlockNumber,
			BlockTime:   formatTime(approval.BlockTime),
			TxHash:      hexBytes(approval.TxHash.Bytes()),
		})
	}

	d.writeData(w, r, items)
}

func (d Deps) handleInteractions(w http.ResponseWriter, r *http.Request) {
	addr, ok := addressFromPath(w, r)
	if !ok {
		return
	}
	limit, ok := limitFromQuery(w, r)
	if !ok {
		return
	}

	interactions, err := d.Profile.Interactions(r.Context(), addr, limit)
	if err != nil {
		d.fail(w, r, err)
		return
	}

	items := make([]interactionDTO, 0, len(interactions))
	for _, interaction := range interactions {
		items = append(items, interactionDTO{
			Contract:   hexAddress(interaction.Contract),
			EventCount: interaction.EventCount,
			LastBlock:  interaction.LastBlock,
			LastTime:   formatTime(interaction.LastTime),
		})
	}

	d.writeData(w, r, items)
}

func (d Deps) handleStats(w http.ResponseWriter, r *http.Request) {
	stats, err := d.Events.Stats(r.Context())
	if err != nil {
		d.fail(w, r, err)
		return
	}

	body := statsDTO{
		TotalLogs:        stats.TotalLogs,
		DecodedCount:     stats.DecodedCount,
		UndecodableCount: stats.UndecodableCount,
	}
	// 数据为空时没有「落后多久」可言，保持 null 而不是伪造 0。
	if !stats.LatestBlockTime.IsZero() {
		stale := int64(time.Since(stats.LatestBlockTime).Seconds())
		body.StaleSeconds = &stale
	}

	writeResponse(w, stats.Coverage, body)
}

// parseEventQuery 校验 /api/events 的查询参数。
func parseEventQuery(w http.ResponseWriter, r *http.Request) (store.EventQuery, bool) {
	values := r.URL.Query()
	query := store.EventQuery{
		Limit:  defaultPageSize,
		Offset: 0,
	}

	if raw := values.Get("contract"); raw != "" {
		if !common.IsHexAddress(raw) {
			writeError(w, http.StatusBadRequest, "invalid_contract", "contract must be a 0x-prefixed 20-byte hex address")
			return store.EventQuery{}, false
		}
		contract := common.HexToAddress(raw)
		query.Contract = &contract
	}

	from, ok := parseUintParam(w, values.Get("from"), "from")
	if !ok {
		return store.EventQuery{}, false
	}
	to, ok := parseUintParam(w, values.Get("to"), "to")
	if !ok {
		return store.EventQuery{}, false
	}
	if from != nil && to != nil && *from > *to {
		writeError(w, http.StatusBadRequest, "invalid_range", "from must not be greater than to")
		return store.EventQuery{}, false
	}
	query.FromBlock = from
	query.ToBlock = to

	limit, ok := limitFromQuery(w, r)
	if !ok {
		return store.EventQuery{}, false
	}
	query.Limit = limit

	// 游标与 offset 互斥：同时给出说明调用方没想清楚要哪种翻页语义，
	// 静默取其一会让分页在客户端悄悄错位。
	cursorRaw := values.Get("cursor")
	offsetRaw := values.Get("offset")

	switch {
	case cursorRaw != "" && offsetRaw != "":
		writeError(w, http.StatusBadRequest, "conflicting_pagination",
			"cursor and offset are mutually exclusive")
		return store.EventQuery{}, false
	case cursorRaw != "":
		cursor, ok := parseCursor(w, cursorRaw)
		if !ok {
			return store.EventQuery{}, false
		}
		query.Cursor = cursor
	case offsetRaw != "":
		offset, err := strconv.Atoi(offsetRaw)
		if err != nil || offset < 0 {
			writeError(w, http.StatusBadRequest, "invalid_offset", "offset must be a non-negative integer")
			return store.EventQuery{}, false
		}
		query.Offset = offset
	}

	return query, true
}

// parseCursor 解析 "<block>:<log_index>" 形式的键集游标。
func parseCursor(w http.ResponseWriter, raw string) (*store.EventCursor, bool) {
	blockPart, indexPart, found := strings.Cut(raw, ":")
	if !found {
		writeError(w, http.StatusBadRequest, "invalid_cursor", `cursor must look like "<block>:<log_index>"`)
		return nil, false
	}

	block, err := strconv.ParseUint(blockPart, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_cursor", `cursor must look like "<block>:<log_index>"`)
		return nil, false
	}
	index, err := strconv.ParseUint(indexPart, 10, 32)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_cursor", `cursor must look like "<block>:<log_index>"`)
		return nil, false
	}

	return &store.EventCursor{BlockNumber: block, LogIndex: uint32(index)}, true
}

// parseUintParam 把可选的十进制参数解析成 uint64，空串返回 nil。
func parseUintParam(w http.ResponseWriter, raw, name string) (*uint64, bool) {
	if raw == "" {
		return nil, true
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_"+name, name+" must be a non-negative integer block number")
		return nil, false
	}
	return &value, true
}

// addressFromPath 读取并校验路径里的地址。
func addressFromPath(w http.ResponseWriter, r *http.Request) (common.Address, bool) {
	raw := r.PathValue("addr")
	if !common.IsHexAddress(raw) {
		writeError(w, http.StatusBadRequest, "invalid_address", "address must be a 0x-prefixed 20-byte hex address")
		return common.Address{}, false
	}
	return common.HexToAddress(raw), true
}

// limitFromQuery 读取分页大小，缺省 50，超过 200 按 200 处理。
//
// 超限采用收敛而不是报错：调用方多是脚本，截断比 400 更省事，
// 真正的保护在上限本身。
func limitFromQuery(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return defaultPageSize, true
	}

	limit, err := strconv.Atoi(raw)
	if err != nil || limit <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be a positive integer")
		return 0, false
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}
	return limit, true
}

// writeData 先取覆盖区间再写出响应。
func (d Deps) writeData(w http.ResponseWriter, r *http.Request, data any) {
	coverage, err := d.Events.Coverage(r.Context())
	if err != nil {
		d.fail(w, r, err)
		return
	}
	writeResponse(w, coverage, data)
}

func writeResponse(w http.ResponseWriter, coverage store.Coverage, data any) {
	writeJSON(w, http.StatusOK, responseDTO{
		Coverage: toCoverageDTO(coverage),
		Data:     data,
	})
}

func toCoverageDTO(coverage store.Coverage) coverageDTO {
	return coverageDTO{
		FromBlock:       coverage.FromBlock,
		ToBlock:         coverage.ToBlock,
		FinalizedBlock:  coverage.FinalizedBlock,
		NextBlock:       coverage.NextBlock,
		LatestBlockTime: formatTime(coverage.LatestBlockTime),
	}
}

// fail 把内部错误映射成 HTTP 状态码。
//
// 区分两种失败：查询超出预算（503，客户端可缩小范围重试）与真正的内部错误
// （500，细节只进日志）。把 SQL 报错原样回给客户端会泄露表结构。
func (d Deps) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, profile.ErrQueryTimeout), errors.Is(err, context.DeadlineExceeded):
		d.logger().Warn("query exceeded budget", "path", r.URL.Path, "err", err)
		writeError(w, http.StatusServiceUnavailable, "query_timeout",
			"query exceeded the time budget, narrow the address or block range")
	case errors.Is(err, context.Canceled):
		// 客户端已断开，回写没有意义。
		return
	default:
		d.logger().Error("request failed", "path", r.URL.Path, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponseDTO{Error: errorDTO{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// 响应头已发出，无法再改状态码，只能记录。
		slog.Error("write response failed", "err", err)
	}
}

// hexAddress 按 ADR-4 统一输出小写 hex。
//
// common.Address.Hex() 返回 EIP-55 校验和大小写混合，直接用会让同一个地址
// 在不同接口里出现两种写法。
func hexAddress(addr common.Address) string {
	return strings.ToLower(addr.Hex())
}

// hexBytes 把小写化的 0x hex 输出统一到同一约定。
func hexBytes(raw []byte) string {
	return "0x" + strings.ToLower(common.Bytes2Hex(raw))
}

// formatTime 统一输出 UTC 的 RFC3339，零值输出空串（配合 omitempty）。
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
