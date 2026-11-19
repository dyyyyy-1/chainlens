# ChainLens

EVM 日志索引器 + 地址画像查询服务。把一个合约的事件流变成结构化数据，再据此回答「这个地址持有多少、授权给谁、和谁交互过」。

当前实证对象：以太坊主网 USDC（`0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48`，链 ID 1）。

---

## 它解决什么问题

链上数据只有两种拿法：要么实时查 `eth_getLogs`（免费端点有几千到一万块的跨度上限），要么自己索引。前者做不了"历史"，后者要处理幂等、重组、编码歧义。这个项目做后者，并把常见的坑用测试和对拍钉住。

两条能力：

- **索引**：区间扫描 → 解码 → 幂等落库 → 最终性水位 → reorg 回滚
- **查询**：5 个只读 HTTP 端点，覆盖事件检索与地址画像

---

## 快速开始

依赖：Go 1.27+、PostgreSQL 17+。

```bash
# 1. 建库与表
createdb chainlens
psql "postgres://chainlens@localhost:5432/chainlens" -f migrations/0001_init.sql
psql "postgres://chainlens@localhost:5432/chainlens" -f migrations/0002_decoded_event_status.sql

# 2. 配置（.env 已被 .gitignore 排除）
cp .env.example .env
# 至少填 DATABASE_URL 与 TARGET_CONTRACT

# 3. 索引（前台运行，Ctrl-C 优雅退出）
go run ./cmd/indexer

# 4. 查询服务（另开一个终端）
go run ./cmd/api
```

```bash
curl -s localhost:8080/api/stats
curl -s "localhost:8080/api/address/0x000000000004444c5dc75cb358380d2e3de08a90/balances"
```

`.env` 关键项：

| 变量 | 说明 |
|---|---|
| `DATABASE_URL` | 必填 |
| `RPC_HTTP_URL` | 必填（索引器与对拍工具用） |
| `CHAIN_ID` | 必填，主网为 `1`。不填会被拒绝，防止主网与测试网数据混写 |
| `TARGET_CONTRACT` | 必填，被索引的合约 |
| `START_BLOCK` | 起始区块 |
| `CONFIRMATIONS` | 最终性窗口，默认 12 |
| `BATCH_BLOCK_RANGE` | 单批区块数，默认 2000。换端点补历史时要按端点上限调小 |
| `RPC_HEADER_BATCH` | 单次批量请求的块头数，默认 100。publicnode 允许 100，drpc 免费档只允许 3 |
| `HTTP_ADDR` | 查询服务监听地址，默认 `:8080` |
| `HTTP_RATE_LIMIT` | 每 IP 每分钟请求上限，默认 120；0 关闭 |

---

## 架构

```
        ┌─────────────────────────────┐
        │  cmd/api   :8080 只读        │
        │   ├ internal/api    路由/校验 │
        │   ├ internal/profile 画像聚合 │
        │   └ internal/store   读查询   │
        └──────────┬──────────────────┘
                   │
       ┌───────────▼───────────┐
       │     PostgreSQL 17     │
       │  chain_log / decoded  │
       │  block_header / cursor│
       └───────────▲───────────┘
                   │
        ┌──────────┴──────────────────┐
        │  cmd/indexer   扫描 + 解码   │
        │   ├ internal/indexer 调度    │
        │   ├ internal/chain  JSON-RPC │
        │   └ internal/decoder ABI/代理 │
        └─────────────────────────────┘
```

查询服务与索引器是两个进程：索引器停机时查询照常服务，只是数据停在最后一次水位。

逐文件的职责与「为什么这样拆」见 [`docs/FILES.md`](docs/FILES.md)。

---

## 数据模型

五张表，三个约束是正确性的最后防线：

| 表 | 作用 | 关键约束 |
|---|---|---|
| `chain_log` | 原始日志（`topic0~3` 存完整 32 字节） | 主键 `(tx_hash, log_index)`，重扫不产生重复 |
| `decoded_event` | 解码结果，`payload` 为 jsonb | 与 `chain_log` 1:1，`status` 区分 `decoded` / `undecodable` |
| `block_header` | 块头（含 `is_finalized`） | 主键 `number`，reorg 检测靠 `parent_hash` 连链 |
| `index_cursor` | 单行游标 | `CHECK (id = 1)`，数据库层面不可能出现两个游标 |
| `contract_meta` | 合约元信息与 ABI | 主键 `address`，代理识别结果不重复解析 |

---

## 关键设计

| 问题 | 做法 | 代价 / 取舍 |
|---|---|---|
| 崩溃时游标前进但数据丢失 | 区块数据与游标在同一事务提交（ADR-1） | 批量提交粒度不能太细，否则事务开销吃掉吞吐 |
| 重扫产生重复 | `COPY` 进临时表 + `INSERT ... ON CONFLICT DO NOTHING`，返回实际新增行数作为幂等判据 | 多一次临时表写入 |
| 链重组 | 比对本地 tip 与链上同高度哈希，回溯共同祖先，事务内删数据并回退游标 | 分叉检测每轮都跑，追平后也有固定开销 |
| 合约是代理，ABI 取错 | 四槽位探测（EIP-1967 实现/信标、ZeppelinOS 遗留槽、EIP-1167 字节码）。USDC 用的是第三种 | 多 3~4 次 RPC |
| 解码失败中断整批 | `Decode` 不返回 error，失败记为 `undecodable` 入库 | 需要 `status` 列（偏离原设计，已登记） |
| 画像查询扫全表 | `LIMIT` 收敛 + 语句级 `statement_timeout` + 地址左填充 32 字节 | 地址历史过大时宁可超时也不占连接 |

设计依据与全部偏离登记见 [`HANDOFF.md`](HANDOFF.md) 与 [`docs/SESSION-HANDOFF.md`](docs/SESSION-HANDOFF.md) §5.1。

---

## 实测性能

索引器，主网 USDC，publicnode 免费端点，2000 块一批：

| 指标 | 值 |
|---|---|
| 单批 2000 块 | 99.0 ~ 112.7 s（含 15.6~18.0 万条日志） |
| 单块平均 | 约 53 ms |
| 追平能力 | 约 19 块/秒（出块速度的 150 倍以上） |
| 存储 | 38.3 万条日志 → `chain_log` 147 MB + `decoded_event` 113 MB（约 680 B/条） |
| 新数据未解码率 | 0%（Sourcify ABI） |

查询延迟（库内 210 万条日志、24,459 块，最热地址 5.7 万条 Transfer）：

| 端点 | 耗时 |
|---|---|
| `/api/events?limit=50` | 3 ms |
| `/api/address/{a}/approvals` | 156 ms |
| `/api/address/{a}/interactions` | 154 ms |
| `/api/address/{a}/balances` | 296 ms |
| `/api/stats` | 353 ms（首次）/ <1 ms（10 秒缓存内） |

比对记录与完整基准见 [`docs/BENCHMARK.md`](docs/BENCHMARK.md)。

---

## API

所有响应统一带 `coverage`（已索引区间）。金额一律十进制字符串——`uint256` 超出 JSON 数字的安全范围，用数字会静默丢精度。

### `GET /api/stats`

```json
{
  "coverage": {
    "from_block": 26422714,
    "to_block": 26433029,
    "finalized_block": 26433017,
    "next_block": 26433030,
    "latest_block_time": "2026-11-17T06:39:35Z"
  },
  "data": {
    "total_logs": 848735,
    "decoded_count": 844757,
    "undecodable_count": 3978,
    "stale_seconds": 358
  }
}
```

`stale_seconds` 是「最新索引区块的时间」与当前时刻之差，用来判断数据新鲜度。

### `GET /api/events`

参数：`contract`（地址）、`from` / `to`（区块区间）、`limit`（默认 50，上限 200）、
翻页二选一——`cursor`（推荐）或 `offset`（兼容旧调用，深翻页会退化）。

```json
{
  "coverage": { "...": "..." },
  "data": [{
    "block_number": 26433029,
    "block_time": "2026-11-17T06:39:35Z",
    "tx_hash": "0x051922938abd438cc65dc5197db8e4a78fe36fde94cc727195c2c6631ba25dcc",
    "log_index": 588,
    "contract": "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48",
    "event": "Transfer",
    "status": "decoded",
    "payload": {"from": "0x8f0d...", "to": "0x82b2...", "value": "1000000000"}
  }],
  "next_cursor": "26433029:588"
}
```

翻页用键集（keyset）而不是 `OFFSET`：`OFFSET` 让数据库先数过前面所有行，翻到很深时
前面每一行都要被扫出来；键集从上一页最后一行继续沿索引倒序扫描，代价恒定，也不会
因为并发写入而跳行。`next_cursor` 只在**本页填满**时返回——不足 `limit` 说明已经到底。

`cursor` 与 `offset` 互斥，同时给出会返回 400，避免两种翻页语义在客户端悄悄错位。

### `GET /api/address/{addr}/balances`

```json
{"data": [{"contract": "0xa0b8...eb48", "net": "-3258700130159", "log_count": 57210}]}
```

`net` 是**已索引区间内的净变化**，不是余额。区间外的历史流水不可见，活跃地址完全可能为负。要看真实余额必须查链上 `balanceOf`。

### `GET /api/address/{addr}/approvals`

```json
{"data": [{
  "contract": "0xa0b8...eb48",
  "spender": "0xa188...f98c",
  "value": "115792089237316195423570985008687907853269984665640564039457584007913129639935",
  "is_unlimited": true,
  "block_number": 26433002,
  "block_time": "2026-11-17T06:34:11Z",
  "tx_hash": "0x4d81...b393"
}]}
```

`value = 2^256-1` 时 `is_unlimited` 为 true。它同样是**区间内最新一条授权日志**，不代表链上当前 allowance。

### `GET /api/address/{addr}/interactions`

按合约汇总事件数与最近活动块。当前只索引一个合约，因此只返回一行。

### 错误码

| 状态 | `code` | 含义 |
|---|---|---|
| 400 | `invalid_address` / `invalid_contract` / `invalid_range` / `invalid_limit` / `invalid_offset` / `invalid_cursor` | 参数不合法 |
| 400 | `conflicting_pagination` | `cursor` 与 `offset` 同时给出 |
| 405 | — | 非 GET 请求 |
| 429 | `rate_limited` | 超出按 IP 的请求速率，响应带 `Retry-After` |
| 503 | `query_timeout` | 查询超出时间预算，缩小地址或区间后重试 |
| 500 | `internal` | 内部错误，细节只进日志 |

限流按来源 IP 固定窗口计数，缺省 120 次/分钟（`HTTP_RATE_LIMIT`，设 0 关闭）。
刻意不读 `X-Forwarded-For`：本服务没有可信反向代理，该头可被任意伪造。

---

## 局限（请先读这一段）

1. **只覆盖一段区间。** 当前库内约 2.45 万块（26,422,714..26,447,172）。因此「持仓」是净变化、「授权」是区间内快照，**都不能直接当链上当前状态用**。
2. **没有第三方交叉验证。** Etherscan API 需要 key（本项目不接受代注册），Blockscout 在开发机上不可达。现有的对拍是**同源回放**：用 `cmd/verify` 重新拉同一区间的 `eth_getLogs`，与库内逐字段比对（19 批 / 783,105 条 / 0 差异）。它能证明索引器忠实记录，**不能**证明我们对合约语义的理解与第三方一致。
3. **免费端点的归档窗口约 10,000 块且滚动。** 索引器停机过久，`next_block` 会滑出窗口，`eth_getLogs` 报 `Archive requests require a personal token`，需要把 `START_BLOCK` 与游标回拨到窗口内。
4. **`/api/stats` 的计数是全表聚合**，随数据量线性变慢（当前 84.9 万行约 176 ms）。已加 10 秒进程内缓存挡住重复请求，但没有解决复杂度；全量索引 USDC 后仍需计数表或物化视图。
5. **无鉴权**，只适合本机或内网。有限流（按 IP 固定窗口），但限流不等于鉴权。
6. `/api/events` 已支持键集分页；为兼容旧调用保留了 `offset`，深翻页会退化，建议改用 `cursor`。

---

## 验证工具

```bash
# 区间回放对拍：链上日志 vs 库内日志，逐字段（含 block_time）
# 退出码 0 无差异 / 1 有差异 / 2 运行失败
go run ./cmd/verify

# 只核对最近 500 块
go run ./cmd/verify -from 26432530 -to 26433029 -batch 500
```

工具会自动二分探测归档边界，只核对可回放的部分，不会把「超出窗口」误报成「数据缺失」。

---

## 开发

```bash
go build ./... && go vet ./... && go test ./... && gofumpt -l .
```

测试分两层：纯函数单测（限流、地址填充、十进制解析、参数校验）与受环境变量门控的集成测试。

```bash
# 画像层与真实库对账（默认跳过）
CHAINLENS_TEST_DSN='postgres://chainlens:chainlens_dev@localhost:5432/chainlens?sslmode=disable' \
  go test ./internal/profile -v
```

---

## 文档

| 文件 | 内容 |
|---|---|
| [`HANDOFF.md`](HANDOFF.md) | 设计依据：定位、ADR、数据模型、里程碑 |
| [`docs/SESSION-HANDOFF.md`](docs/SESSION-HANDOFF.md) | 运行状态：进度、环境、踩坑、待决策、**偏离登记表** |
| [`docs/FILES.md`](docs/FILES.md) | 逐文件职责与拆分理由 |
| [`docs/BENCHMARK.md`](docs/BENCHMARK.md) | 压测与对拍记录 |
