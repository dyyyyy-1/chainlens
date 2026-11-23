package api

import (
	"cmp"
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/dyyyyy-1/chainlens/internal/store"
)

// fakeStreamer 记录最后一次查询条件，便于断言导出与分页用的是同一套过滤。
type fakeStreamer struct {
	events    []store.Event
	count     int64
	countErr  error
	streamErr error
	lastQuery store.EventQuery
}

func (f *fakeStreamer) StreamEvents(_ context.Context, q store.EventQuery, fn func(store.Event) error) error {
	f.lastQuery = q
	if f.streamErr != nil {
		return f.streamErr
	}

	// 模拟 store 的 ORDER BY block_number DESC, log_index DESC：
	// 导出的顺序必须与分页一致，否则同一个过滤条件在页面和文件里顺序不同。
	events := slices.Clone(f.events)
	slices.SortFunc(events, func(a, b store.Event) int {
		if a.BlockNumber != b.BlockNumber {
			return cmp.Compare(b.BlockNumber, a.BlockNumber)
		}
		return cmp.Compare(b.LogIndex, a.LogIndex)
	})

	for _, event := range events {
		if err := fn(event); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeStreamer) CountEvents(_ context.Context, q store.EventQuery) (int64, error) {
	f.lastQuery = q
	if f.countErr != nil {
		return 0, f.countErr
	}
	if f.count > 0 {
		return f.count, nil
	}
	return int64(len(f.events)), nil
}

func sampleEvent(block uint64, txByte byte) store.Event {
	txHash := common.Hash{}
	txHash[31] = txByte

	return store.Event{
		BlockNumber: block,
		BlockTime:   time.Date(2026, 11, 21, 5, 0, 0, 0, time.UTC),
		TxHash:      txHash,
		LogIndex:    3,
		Contract:    common.HexToAddress(sampleAddress),
		EventName:   "Transfer",
		Status:      "decoded",
		Payload:     json.RawMessage(`{"from":"0xaa","to":"0xbb","value":"1000000"}`),
	}
}

func newExportRouter(streamer EventStreamer) http.Handler {
	return NewRouter(Deps{
		Events:   &fakeEvents{coverage: testCoverage()},
		Profile:  &fakeProfile{},
		Streamer: streamer,
	})
}

func TestExportCSV(t *testing.T) {
	streamer := &fakeStreamer{events: []store.Event{sampleEvent(100, 1), sampleEvent(101, 2)}}
	recorder := get(t, newExportRouter(streamer), "/api/events/export?format=csv&from=100&to=200")

	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/csv") {
		t.Fatalf("Content-Type = %q, want text/csv", contentType)
	}
	if disposition := recorder.Header().Get("Content-Disposition"); !strings.Contains(disposition, ".csv") {
		t.Fatalf("Content-Disposition = %q, want 文件名带 .csv", disposition)
	}

	records, err := csv.NewReader(strings.NewReader(recorder.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("解析 CSV 失败：%v", err)
	}
	if len(records) != 3 {
		t.Fatalf("CSV 行数 = %d, want 3（表头 + 2 行）", len(records))
	}
	if records[0][0] != "block_number" || records[0][7] != "payload" {
		t.Fatalf("表头不对：%v", records[0])
	}
	if records[1][0] != "101" {
		t.Fatalf("第一行数据块号 = %q, want 101（倒序）", records[1][0])
	}
	// payload 是紧凑 JSON，必须原样成为一列，而不是被拆成多列。
	if !strings.Contains(records[1][7], `"value":"1000000"`) {
		t.Fatalf("payload 列 = %q", records[1][7])
	}

	if streamer.lastQuery.FromBlock == nil || *streamer.lastQuery.FromBlock != 100 {
		t.Fatalf("过滤条件没有传到 store：%+v", streamer.lastQuery)
	}
}

func TestExportJSON(t *testing.T) {
	streamer := &fakeStreamer{events: []store.Event{sampleEvent(100, 1)}}
	recorder := get(t, newExportRouter(streamer), "/api/events/export?format=json")

	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("Content-Type = %q", contentType)
	}

	var items []eventDTO
	if err := json.Unmarshal(recorder.Body.Bytes(), &items); err != nil {
		t.Fatalf("响应不是合法 JSON 数组：%v (body=%s)", err, recorder.Body.String())
	}
	if len(items) != 1 {
		t.Fatalf("条数 = %d, want 1", len(items))
	}
	if items[0].TxHash == "" || items[0].Event != "Transfer" {
		t.Fatalf("导出字段不完整：%+v", items[0])
	}
}

func TestExportDefaultsToCSV(t *testing.T) {
	streamer := &fakeStreamer{events: []store.Event{sampleEvent(100, 1)}}
	recorder := get(t, newExportRouter(streamer), "/api/events/export")

	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/csv") {
		t.Fatalf("缺省格式应为 CSV，得到 %q", contentType)
	}
}

func TestExportRejectsPaginationParams(t *testing.T) {
	// 导出是「整个过滤范围」；带上 limit/cursor/offset 说明调用方理解错了语义，
	// 静默忽略会让他以为导出的是某一页。
	for _, suffix := range []string{"limit=10", "cursor=100:1", "offset=50"} {
		recorder := get(t, newExportRouter(&fakeStreamer{}), "/api/events/export?"+suffix)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s 状态码 = %d, want 400", suffix, recorder.Code)
		}
		if code := decodeBody[errorResponseDTO](t, recorder).Error.Code; code != "invalid_export_query" {
			t.Fatalf("%s 错误码 = %q", suffix, code)
		}
	}
}

func TestExportRejectsInvalidFormat(t *testing.T) {
	recorder := get(t, newExportRouter(&fakeStreamer{}), "/api/events/export?format=xml")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d, want 400", recorder.Code)
	}
	if code := decodeBody[errorResponseDTO](t, recorder).Error.Code; code != "invalid_format" {
		t.Fatalf("错误码 = %q, want invalid_format", code)
	}
}

func TestExportRejectsTooManyRows(t *testing.T) {
	streamer := &fakeStreamer{count: exportMaxRows + 1}
	recorder := get(t, newExportRouter(streamer), "/api/events/export?format=csv")

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d, want 400", recorder.Code)
	}
	if code := decodeBody[errorResponseDTO](t, recorder).Error.Code; code != "export_too_large" {
		t.Fatalf("错误码 = %q, want export_too_large", code)
	}
	// 必须在写响应体之前拒绝：否则客户端会拿到一个悄悄截断的文件。
	if recorder.Body.Len() > 512 {
		t.Fatalf("拒绝时不应写入大段内容，body=%s", recorder.Body.String())
	}
}

func TestExportCountFailure(t *testing.T) {
	streamer := &fakeStreamer{countErr: context.DeadlineExceeded}
	recorder := get(t, newExportRouter(streamer), "/api/events/export?format=csv")

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d, want 503", recorder.Code)
	}
	if code := decodeBody[errorResponseDTO](t, recorder).Error.Code; code != "query_timeout" {
		t.Fatalf("错误码 = %q, want query_timeout", code)
	}
}

func TestExportWithoutStreamer(t *testing.T) {
	handler := NewRouter(Deps{Events: &fakeEvents{coverage: testCoverage()}, Profile: &fakeProfile{}})
	recorder := get(t, handler, "/api/events/export?format=csv")

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d, want 500", recorder.Code)
	}
}
