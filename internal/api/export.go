package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/dyyyyy-1/chainlens/internal/store"
)

const (
	// exportMaxRows 是单次导出的行数上限。
	//
	// 200k 行 CSV 约 40 MB，前端用 fetch 接 Blob 也吃得下；再大就该用命令行工具
	// 直连数据库，而不是让浏览器做 ETL。
	exportMaxRows = 200_000

	// exportFlushEvery 控制每写多少行 flush 一次。
	// 不 flush 的话响应会攒在 buffer 里，用户看到「卡住」直到全部写完。
	exportFlushEvery = 1000
)

// EventStreamer 是导出所需的流式读取能力，由 internal/store 满足。
//
// 与 EventStore 分开：分页接口不需要流式，把 StreamEvents 塞进 EventStore
// 会让所有 mock 都被迫实现一个用不到的方法。
type EventStreamer interface {
	StreamEvents(ctx context.Context, q store.EventQuery, fn func(store.Event) error) error
	CountEvents(ctx context.Context, q store.EventQuery) (int64, error)
}

// handleExport 把符合过滤条件的事件导出为 CSV 或 JSON。
//
// 设计取舍：
//   - 先 count 再流式写。一旦开始写响应体就无法改状态码，先数一遍才能在
//     「范围过大」时返回 400，而不是给用户一个悄悄截断的文件。
//   - 不接受 limit/cursor/offset。导出的语义是「整个过滤范围」，
//     带上分页参数会让调用方误以为导出的是某一页。
//   - CSV 的 payload 用紧凑 JSON 字符串，不把字段拍平成动态列——
//     不同事件的字段不同，拍平会产生大量空列。
func (d Deps) handleExport(w http.ResponseWriter, r *http.Request) {
	if d.Streamer == nil {
		writeError(w, http.StatusInternalServerError, "internal", "export is not configured")
		return
	}

	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "csv"
	}
	if format != "csv" && format != "json" {
		writeError(w, http.StatusBadRequest, "invalid_format", `format must be "csv" or "json"`)
		return
	}

	query, ok := parseExportQuery(w, r)
	if !ok {
		return
	}

	count, err := d.Streamer.CountEvents(r.Context(), query)
	if err != nil {
		d.fail(w, r, err)
		return
	}
	if count > exportMaxRows {
		writeError(w, http.StatusBadRequest, "export_too_large",
			fmt.Sprintf("matched %d rows, the limit is %d; narrow the block range", count, exportMaxRows))
		return
	}

	filename := fmt.Sprintf("chainlens-events-%s.%s", time.Now().UTC().Format("20060102-150405"), format)
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)

	var writeErr error
	if format == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		writeErr = d.writeEventsCSV(w, r, query)
	} else {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		writeErr = d.writeEventsJSON(w, r, query)
	}

	if writeErr != nil {
		// 响应头已发出，无法再改状态码；只能记录，让客户端看到被截断的流。
		d.logger().Warn("export interrupted", "format", format, "err", writeErr)
	}
}

// parseExportQuery 校验导出参数。分页参数一律拒绝，避免语义歧义。
func parseExportQuery(w http.ResponseWriter, r *http.Request) (store.EventQuery, bool) {
	values := r.URL.Query()
	for _, name := range []string{"cursor", "offset", "limit"} {
		if values.Get(name) != "" {
			writeError(w, http.StatusBadRequest, "invalid_export_query",
				name+" is not supported by export; narrow the block range instead")
			return store.EventQuery{}, false
		}
	}

	query := store.EventQuery{Limit: exportMaxRows}

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

	return query, true
}

func (d Deps) writeEventsCSV(w io.Writer, r *http.Request, query store.EventQuery) error {
	writer := csv.NewWriter(w)
	if err := writer.Write([]string{
		"block_number", "block_time", "tx_hash", "log_index", "contract", "event", "status", "payload",
	}); err != nil {
		return err
	}

	rows := 0
	err := d.Streamer.StreamEvents(r.Context(), query, func(event store.Event) error {
		if err := writer.Write([]string{
			strconv.FormatUint(event.BlockNumber, 10),
			formatTime(event.BlockTime),
			hexBytes(event.TxHash.Bytes()),
			strconv.FormatUint(uint64(event.LogIndex), 10),
			hexAddress(event.Contract),
			event.EventName,
			event.Status,
			string(event.Payload),
		}); err != nil {
			return err
		}

		rows++
		if rows%exportFlushEvery == 0 {
			writer.Flush()
			return writer.Error()
		}
		return nil
	})

	writer.Flush()
	if err != nil {
		return err
	}
	return writer.Error()
}

func (d Deps) writeEventsJSON(w io.Writer, r *http.Request, query store.EventQuery) error {
	if _, err := io.WriteString(w, "["); err != nil {
		return err
	}

	rows := 0
	first := true
	err := d.Streamer.StreamEvents(r.Context(), query, func(event store.Event) error {
		if !first {
			if _, err := io.WriteString(w, ","); err != nil {
				return err
			}
		}
		first = false

		encoded, err := json.Marshal(toEventDTO(event))
		if err != nil {
			return err
		}
		if _, err := w.Write(encoded); err != nil {
			return err
		}

		rows++
		if rows%exportFlushEvery == 0 {
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	_, err = io.WriteString(w, "]")
	return err
}
