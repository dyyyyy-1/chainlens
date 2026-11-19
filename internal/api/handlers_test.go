package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/dyyyyy-1/chainlens/internal/profile"
	"github.com/dyyyyy-1/chainlens/internal/store"
)

// 假实现：本包测试不需要数据库，路由与校验不该依赖真实存储。
type fakeEvents struct {
	events    []store.Event
	stats     store.Stats
	coverage  store.Coverage
	eventsErr error
	statsErr  error
	lastQuery store.EventQuery
}

func (f *fakeEvents) ListEvents(_ context.Context, q store.EventQuery) ([]store.Event, error) {
	f.lastQuery = q
	return f.events, f.eventsErr
}

func (f *fakeEvents) Stats(context.Context) (store.Stats, error) {
	return f.stats, f.statsErr
}

func (f *fakeEvents) Coverage(context.Context) (store.Coverage, error) {
	return f.coverage, nil
}

type fakeProfile struct {
	holdings     []profile.Holding
	approvals    []profile.Approval
	interactions []profile.Interaction
	err          error
	lastLimit    int
}

func (f *fakeProfile) Holdings(context.Context, common.Address, int) ([]profile.Holding, error) {
	return f.holdings, f.err
}

func (f *fakeProfile) Approvals(_ context.Context, _ common.Address, limit int) ([]profile.Approval, error) {
	f.lastLimit = limit
	return f.approvals, f.err
}

func (f *fakeProfile) Interactions(context.Context, common.Address, int) ([]profile.Interaction, error) {
	return f.interactions, f.err
}

func testCoverage() store.Coverage {
	return store.Coverage{
		FromBlock:       26422714,
		ToBlock:         26432909,
		FinalizedBlock:  26432897,
		NextBlock:       26432910,
		LatestBlockTime: time.Date(2026, 11, 17, 6, 30, 0, 0, time.UTC),
	}
}

func newTestRouter(events *fakeEvents, profiles *fakeProfile) http.Handler {
	return NewRouter(Deps{Events: events, Profile: profiles})
}

func get(t *testing.T, handler http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	return recorder
}

func decodeBody[T any](t *testing.T, recorder *httptest.ResponseRecorder) T {
	t.Helper()
	var body T
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON：%v，原文 %s", err, recorder.Body.String())
	}
	return body
}

const sampleAddress = "0x000000000004444c5dc75cb358380d2e3de08a90"

func TestEventsEndpoint(t *testing.T) {
	events := &fakeEvents{
		coverage: testCoverage(),
		events: []store.Event{{
			BlockNumber: 26432909,
			BlockTime:   time.Date(2026, 11, 17, 6, 29, 47, 0, time.UTC),
			TxHash:      common.HexToHash("0xABCDEF0000000000000000000000000000000000000000000000000000000001"),
			LogIndex:    3,
			Contract:    common.HexToAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"),
			EventName:   "Transfer",
			Status:      "decoded",
			Payload:     []byte(`{"from":"0xaa","to":"0xbb","value":"100"}`),
		}},
	}
	handler := newTestRouter(events, &fakeProfile{})

	recorder := get(t, handler, "/api/events?contract="+sampleAddress+"&from=26422714&to=26432909&limit=5&offset=2")
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200，body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q", got)
	}

	body := decodeBody[struct {
		Coverage coverageDTO `json:"coverage"`
		Data     []eventDTO  `json:"data"`
	}](t, recorder)

	if body.Coverage.FromBlock != 26422714 || body.Coverage.ToBlock != 26432909 {
		t.Fatalf("coverage 未随响应返回：%+v", body.Coverage)
	}
	if len(body.Data) != 1 {
		t.Fatalf("data 长度 = %d, want 1", len(body.Data))
	}

	event := body.Data[0]
	if event.Contract != strings.ToLower("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48") {
		t.Fatalf("contract 必须小写输出，得到 %q", event.Contract)
	}
	if event.TxHash != "0xabcdef0000000000000000000000000000000000000000000000000000000001" {
		t.Fatalf("tx_hash 必须小写输出，得到 %q", event.TxHash)
	}
	if event.BlockTime != "2026-11-17T06:29:47Z" {
		t.Fatalf("block_time = %q, want UTC RFC3339", event.BlockTime)
	}
	if string(event.Payload) != `{"from":"0xaa","to":"0xbb","value":"100"}` {
		t.Fatalf("payload 透传失败：%s", event.Payload)
	}

	if events.lastQuery.Limit != 5 || events.lastQuery.Offset != 2 {
		t.Fatalf("分页参数未透传：%+v", events.lastQuery)
	}
	if events.lastQuery.Contract == nil || *events.lastQuery.Contract != common.HexToAddress(sampleAddress) {
		t.Fatalf("contract 过滤未透传：%+v", events.lastQuery.Contract)
	}
}

func TestBalancesPrecision(t *testing.T) {
	// 用 2^256-1 这种超出 float64 与 int64 的量级，验证 JSON 里是精确的十进制字符串。
	max := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	profiles := &fakeProfile{
		holdings: []profile.Holding{{
			Contract: common.HexToAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"),
			Net:      max,
			LogCount: 56488,
		}},
	}
	handler := newTestRouter(&fakeEvents{coverage: testCoverage()}, profiles)

	recorder := get(t, handler, "/api/address/"+sampleAddress+"/balances")
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", recorder.Code, recorder.Body.String())
	}

	body := decodeBody[struct {
		Data []holdingDTO `json:"data"`
	}](t, recorder)
	if len(body.Data) != 1 {
		t.Fatalf("data 长度 = %d, want 1", len(body.Data))
	}
	if body.Data[0].Net != max.String() {
		t.Fatalf("net = %q, want %q", body.Data[0].Net, max.String())
	}
	if body.Data[0].LogCount != 56488 {
		t.Fatalf("log_count = %d", body.Data[0].LogCount)
	}
}

func TestApprovalsUnlimitedFlag(t *testing.T) {
	max := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	profiles := &fakeProfile{
		approvals: []profile.Approval{{
			Contract:    common.HexToAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"),
			Spender:     common.HexToAddress("0x0729e2a4716d9f7dd17858d5d504426d228697f7"),
			Value:       max,
			IsUnlimited: true,
			BlockNumber: 26432000,
			BlockTime:   time.Date(2026, 11, 17, 5, 0, 0, 0, time.UTC),
			TxHash:      common.HexToHash("0x01"),
		}},
	}
	handler := newTestRouter(&fakeEvents{coverage: testCoverage()}, profiles)

	recorder := get(t, handler, "/api/address/"+sampleAddress+"/approvals")
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", recorder.Code, recorder.Body.String())
	}

	body := decodeBody[struct {
		Data []approvalDTO `json:"data"`
	}](t, recorder)
	if len(body.Data) != 1 || !body.Data[0].IsUnlimited {
		t.Fatalf("无限额度标记丢失：%+v", body.Data)
	}
}

func TestInvalidAddress(t *testing.T) {
	handler := newTestRouter(&fakeEvents{}, &fakeProfile{})

	recorder := get(t, handler, "/api/address/0x1234/balances")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d, want 400", recorder.Code)
	}

	body := decodeBody[errorResponseDTO](t, recorder)
	if body.Error.Code != "invalid_address" {
		t.Fatalf("错误码 = %q, want invalid_address", body.Error.Code)
	}
}

func TestInvalidBlockRange(t *testing.T) {
	handler := newTestRouter(&fakeEvents{}, &fakeProfile{})

	recorder := get(t, handler, "/api/events?from=100&to=50")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d, want 400", recorder.Code)
	}
	if code := decodeBody[errorResponseDTO](t, recorder).Error.Code; code != "invalid_range" {
		t.Fatalf("错误码 = %q, want invalid_range", code)
	}
}

func TestLimitIsClampedNotRejected(t *testing.T) {
	events := &fakeEvents{coverage: testCoverage()}
	handler := newTestRouter(events, &fakeProfile{})

	recorder := get(t, handler, "/api/events?limit=100000")
	if recorder.Code != http.StatusOK {
		t.Fatalf("超限 limit 应被收敛而不是报错，状态码 = %d", recorder.Code)
	}
	if events.lastQuery.Limit != maxPageSize {
		t.Fatalf("limit = %d, want %d", events.lastQuery.Limit, maxPageSize)
	}
}

func TestQueryTimeoutMapsTo503(t *testing.T) {
	profiles := &fakeProfile{err: fmt.Errorf("profile: holdings: %w", profile.ErrQueryTimeout)}
	handler := newTestRouter(&fakeEvents{coverage: testCoverage()}, profiles)

	recorder := get(t, handler, "/api/address/"+sampleAddress+"/balances")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d, want 503", recorder.Code)
	}

	body := decodeBody[errorResponseDTO](t, recorder)
	if body.Error.Code != "query_timeout" {
		t.Fatalf("错误码 = %q, want query_timeout", body.Error.Code)
	}
}

func TestInternalErrorDoesNotLeakDetails(t *testing.T) {
	events := &fakeEvents{
		coverage:  testCoverage(),
		eventsErr: errors.New(`ERROR: relation "chain_log" does not exist (SQLSTATE 42P01)`),
	}
	handler := newTestRouter(events, &fakeProfile{})

	recorder := get(t, handler, "/api/events")
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d, want 500", recorder.Code)
	}
	if body := recorder.Body.String(); strings.Contains(body, "chain_log") || strings.Contains(body, "SQLSTATE") {
		t.Fatalf("响应泄露了内部细节：%s", body)
	}
}

func TestStatsEmptyDatabase(t *testing.T) {
	handler := newTestRouter(&fakeEvents{stats: store.Stats{}}, &fakeProfile{})

	recorder := get(t, handler, "/api/stats")
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", recorder.Code, recorder.Body.String())
	}

	body := decodeBody[struct {
		Data statsDTO `json:"data"`
	}](t, recorder)
	if body.Data.StaleSeconds != nil {
		t.Fatalf("空库时 stale_seconds 应为 null，得到 %d", *body.Data.StaleSeconds)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	handler := newTestRouter(&fakeEvents{}, &fakeProfile{})

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/stats", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("状态码 = %d, want 405", recorder.Code)
	}
}

func TestEventsCursorPagination(t *testing.T) {
	// 返回条数等于 limit 才能判断「可能还有下一页」，因此造满一页。
	events := &fakeEvents{coverage: testCoverage()}
	for i := range 2 {
		events.events = append(events.events, store.Event{
			BlockNumber: 26433029,
			BlockTime:   time.Date(2026, 11, 17, 6, 29, 47, 0, time.UTC),
			TxHash:      common.Hash{31: byte(i + 1)},
			LogIndex:    uint32(588 - i),
			Contract:    common.HexToAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"),
			EventName:   "Transfer",
			Status:      "decoded",
			Payload:     []byte(`{}`),
		})
	}
	handler := newTestRouter(events, &fakeProfile{})

	recorder := get(t, handler, "/api/events?limit=2")
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", recorder.Code, recorder.Body.String())
	}

	body := decodeBody[struct {
		NextCursor string `json:"next_cursor"`
	}](t, recorder)
	if body.NextCursor != "26433029:587" {
		t.Fatalf("next_cursor = %q, want 26433029:587（最后一行的 block:index）", body.NextCursor)
	}
}

func TestEventsNoCursorOnLastPage(t *testing.T) {
	// 只返回 1 条而 limit=2，说明到底了，不应再给游标。
	events := &fakeEvents{
		coverage: testCoverage(),
		events:   []store.Event{{BlockNumber: 26433029, LogIndex: 588, Payload: []byte(`{}`)}},
	}
	handler := newTestRouter(events, &fakeProfile{})

	body := decodeBody[struct {
		NextCursor string `json:"next_cursor"`
	}](t, get(t, handler, "/api/events?limit=2"))
	if body.NextCursor != "" {
		t.Fatalf("最后一页不应给 next_cursor，得到 %q", body.NextCursor)
	}
}

func TestEventsCursorIsPropagated(t *testing.T) {
	events := &fakeEvents{coverage: testCoverage()}
	handler := newTestRouter(events, &fakeProfile{})

	recorder := get(t, handler, "/api/events?cursor=26433029:588&limit=10")
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if events.lastQuery.Cursor == nil {
		t.Fatal("cursor 未透传到查询")
	}
	if events.lastQuery.Cursor.BlockNumber != 26433029 || events.lastQuery.Cursor.LogIndex != 588 {
		t.Fatalf("cursor = %+v", events.lastQuery.Cursor)
	}
	if events.lastQuery.Offset != 0 {
		t.Fatalf("使用游标时 offset 必须为 0，得到 %d", events.lastQuery.Offset)
	}
}

func TestInvalidCursor(t *testing.T) {
	handler := newTestRouter(&fakeEvents{}, &fakeProfile{})

	for _, raw := range []string{"abc", "26433029", "26433029:abc", ":5"} {
		recorder := get(t, handler, "/api/events?cursor="+raw)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("cursor=%q 状态码 = %d, want 400", raw, recorder.Code)
		}
		if code := decodeBody[errorResponseDTO](t, recorder).Error.Code; code != "invalid_cursor" {
			t.Fatalf("cursor=%q 错误码 = %q, want invalid_cursor", raw, code)
		}
	}
}

func TestCursorAndOffsetConflict(t *testing.T) {
	handler := newTestRouter(&fakeEvents{}, &fakeProfile{})

	recorder := get(t, handler, "/api/events?cursor=26433029:588&offset=10")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d, want 400", recorder.Code)
	}
	if code := decodeBody[errorResponseDTO](t, recorder).Error.Code; code != "conflicting_pagination" {
		t.Fatalf("错误码 = %q, want conflicting_pagination", code)
	}
}

func TestRateLimit(t *testing.T) {
	handler := NewRouter(Deps{
		Events:             &fakeEvents{coverage: testCoverage()},
		Profile:            &fakeProfile{},
		RateLimitPerMinute: 2,
	})

	for i := range 2 {
		if code := get(t, handler, "/api/stats").Code; code != http.StatusOK {
			t.Fatalf("第 %d 次请求状态码 = %d, want 200", i+1, code)
		}
	}

	recorder := get(t, handler, "/api/stats")
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("超限后状态码 = %d, want 429", recorder.Code)
	}
	if recorder.Header().Get("Retry-After") == "" {
		t.Fatal("429 响应必须带 Retry-After")
	}
	if code := decodeBody[errorResponseDTO](t, recorder).Error.Code; code != "rate_limited" {
		t.Fatalf("错误码 = %q, want rate_limited", code)
	}
}

func TestRateLimitDisabled(t *testing.T) {
	handler := NewRouter(Deps{Events: &fakeEvents{coverage: testCoverage()}, Profile: &fakeProfile{}})

	for i := range 5 {
		if code := get(t, handler, "/api/stats").Code; code != http.StatusOK {
			t.Fatalf("未配置限流时第 %d 次请求被拒：%d", i+1, code)
		}
	}
}
