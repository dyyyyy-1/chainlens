# 文件职责清单

> **维护规则**：新增、删除或重命名任何文件，必须同步更新本表。评审时本表与目录结构不一致视为缺陷。

路径全部相对于仓库根目录。

---

## 入口（`cmd/`）

| 文件 | 职责 | 阶段 |
|---|---|---|
| `cmd/indexer/main.go` | 索引器进程入口。装载配置 → 建连接池与 RPC 客户端 → 识别目标合约的代理实现并写入 `contract_meta` → 构造解码器 → 启动 `indexer.Scanner`，响应 Ctrl-C 优雅退出。含 `validate()` 强制校验 `DATABASE_URL` 与 `TARGET_CONTRACT`。代理识别失败只告警不阻塞索引。 | W1D2、W2D1 |
| `cmd/probe/main.go` | 临时探测工具。拉指定合约一段区间的日志并原样打印（块号、tx、log_index、topic0~3、data 长度），用于确认 topic 布局。**索引器稳定后可删除**。 | W1D1 |

---

## 领域层（`internal/`）

### `internal/chain` —— JSON-RPC 访问

| 文件 | 职责 | 阶段 |
|---|---|---|
| `client.go` | 连接管理、超时、重试、链 ID 校验。导出 `New` / `HeaderByNumber` / `LatestBlockNumber` / `HeadersByRange` / `Close`。 | W1D1、W1D2 |
| `logs.go` | `Fetch`：按区块区间走 `eth_getLogs` 拉日志（ADR-2），带区间合法性校验与 2000 块上限。 | W1D1 |

**为什么重试写在 `client.go` 而不是独立文件**：重试是调用语义的一部分，只有本包的调用点使用；抽出去反而要暴露内部状态。

**为什么 `HeadersByRange` 用批量请求**：`chain_log.block_time` 是 `NOT NULL`，必须取块头。2000 块逐次请求等于 2000 次往返，批量后降到 20 次。

### `internal/decoder` —— 日志解码

| 文件 | 职责 | 阶段 |
|---|---|---|
| `registry.go` | 内置事件签名表（ERC20 / ERC721 / WETH / ERC1155 常见事件）。哈希在初始化时由签名串算出，不手抄常量。作用：没有 ABI 时仍能把 topic0 还原成可读事件名。 | W1D3 |
| `abi.go` | ABI 解析与解码。`New` / `NewFromFile` / `Decode`。覆盖 §8.3 四种边界：anonymous 事件、indexed 超 3 个、ABI 缺失、data 长度非法。`normalize` 负责把 `big.Int` 转十进制字符串、地址转小写 hex。 | W1D3 |
| `abi_test.go` | 上述四种边界 + Transfer/Approval 正常路径的表驱动测试。 | W1D3 |
| `proxy.go` | 代理识别。声明 `ChainReader` 接口（由 `internal/chain` 实现），按优先级探测四种模式：EIP-1967 实现槽 → EIP-1967 信标槽（`eth_call` 取实现） → ZeppelinOS 遗留槽 → EIP-1167 最小代理字节码。**USDC 属于第三种**，只做 EIP-1967 会漏判。 | W2D1 |
| `proxy_test.go` | 用 `fakeReader` 覆盖四种代理 + EOA + 普通合约 + 信标调用失败，全部脱离网络。 | W2D1 |
| `sourcify.go` | 从 Sourcify v2 拉取已验证合约的 ABI。**无需注册与 API key**，这是替代 Etherscan 的关键理由。未验证返回 `ErrNotVerified`，5xx 重试一次，4xx 直接失败。注意返回的 ABI 可能是代理合约自己的（只有 `AdminChanged`/`Upgraded`），必须按实现地址拉取。 | W3D1 |
| `sourcify_test.go` | 用 `httptest` 覆盖正常返回、未验证、ABI 为 null、5xx 重试成功、持续 5xx 放弃，全部离线。 | W3D1 |

**为什么 `Decode` 不返回 `error`**：解码失败是预期内情况，未解码的日志同样要入库（`status = undecodable`）。返回 error 会诱导调用方中断整批，与 §8.3「不中断索引」冲突。失败原因放在 `Event.Reason`，只用于日志分级。

**为什么用 `defer recover()` 包住解码**：`accounts/abi` 在畸形输入上存在 panic 路径，§8.3 明确要求「不 panic」。

### `internal/indexer` —— 扫描主循环

| 文件 | 职责 | 阶段 |
|---|---|---|
| `cursor.go` | `nextRange`：纯计算，无 IO。根据游标、链头、确认数、批量上限算出本批区间或「无活」。可独立单测。 | W1D2 |
| `cursor_test.go` | `nextRange` 的 6 个边界用例（批量受限、水位受限、追平、head 低于确认数、batch=0）。 | W1D2 |
| `scanner.go` | 主循环与单批流程：取区间 → 校验分叉 → 拉日志 → 补块头 → 解码 → 事务内写四张表 → 晋升确认水位 → 推进游标。块头按**整段区间**写入（`blockNumbers`），因为 reorg 检测要靠 `parent_hash` 把块头连成链，缺块就会断开。同时在此声明 `LogFetcher` / `HeaderFetcher` / `LogStore` / `EventDecoder` 四个接口。 | W1D2、W1D3、W2D1、W2D2 |
| `reorg.go` | 分叉检测与回滚。`checkReorg` 比对本地 tip 与链上同高度哈希；`findForkPoint` 在 `maxReorgDepth`（128）窗口内批量拉块头回溯共同祖先；`applyReorg` 在事务内删数据并把游标退回分叉点。**每轮都执行**，且早于「是否还有活」的判定——重组最容易发生在追平之后。 | W2D2 |

**为什么接口定义在这里**：§11 要求接口在使用方声明。`chain` 与 `store` 只提供具体类型，不反向依赖 indexer。

**游标语义**：`next_block` 是下一个待扫高度；`to = min(next_block + batch - 1, head - confirmations)`，因此凡写入的数据都已越过确认数门槛。

### `internal/store` —— 持久化

| 文件 | 职责 | 阶段 |
|---|---|---|
| `models.go` | `Store` 类型、连接池构造与 `Begin`/`Close`；四个记录类型 `LogRecord` / `HeaderRecord` / `DecodedRecord` / `Cursor`；`NewLogRecord` 负责 `types.Log` → 记录的转换。 | W1D2、W1D3 |
| `logs.go` | `InsertLogs`：临时表中转 + COPY + 幂等合并（见下）。 | W1D2 |
| `headers.go` | `InsertHeaders`：`unnest` 数组参数一次写入多行块头。 | W1D2 |
| `decoded.go` | `InsertDecoded`：`unnest` 写入解码结果，`payload` 以 JSON 文本传入后在库内转 `jsonb`。 | W1D3 |
| `meta.go` | `UpsertContractMeta` / `LoadContractMeta`：代理识别结果的读写。`abi` 用 `COALESCE` 更新，避免重复解析时把已获取的 ABI 覆盖成空。 | W2D1 |
| `cursor.go` | `EnsureCursor` / `LoadCursor` / `Advance`。游标推进必须由调用方传入事务。 | W1D2 |

**`logs.go` 为什么这么绕（临时表 → COPY → INSERT ... ON CONFLICT）**：
PostgreSQL 的 `COPY` 协议不支持 `ON CONFLICT`，而 §8.4 同时要求「COPY 批量写」与「冲突时静默跳过」。解法是事务内建 `TEMP TABLE ... (LIKE chain_log) ON COMMIT DROP`（`LIKE` 默认不带主键约束，灌入不会因重复中断），COPY 进临时表后再用 `INSERT ... SELECT ... ON CONFLICT (tx_hash, log_index) DO NOTHING` 合并。返回值是 `RowsAffected()`，即**实际新增行数**——重扫时恒为 0，这就是幂等的判定依据。

**`headers.go` / `decoded.go` 为什么超出 §5 的「models / logs / cursor」三文件**：§5 是骨架示意，`block_header` 与 `decoded_event` 的写入需要落点。塞进 `logs.go` 会把两种写职责混在一起，故各起一文件。这是已知且经确认的偏离。

**`Store` 结构与构造器为什么放在 `models.go`**：同样是为了不再增加文件数（没有独立的 `store.go`）。

### `internal/config`

| 文件 | 职责 | 阶段 |
|---|---|---|
| `config.go` | 用标准库解析 `.env`（约 40 行，不引 godotenv），再映射到 `Config`。真实环境变量优先于文件；`.env` 不存在不报错。`RPC_HTTP_URL` 与 `CHAIN_ID` 必填。 | W1D1 |

---

## 数据定义（`migrations/`）

| 文件 | 职责 | 阶段 |
|---|---|---|
| `0001_init.sql` | §6 的 5 张表 + `logs_hex` 调试视图，逐字转写。整文件包在 `BEGIN/COMMIT` 内。 | W1D1 |
| `0002_decoded_event_status.sql` | 给 `decoded_event` 补 `status` 列（`decoded` / `undecodable`）。起因：§8.3 要求标记 undecodable，而 §6 没有状态列。状态独立成列而非塞进 `payload`。 | W1D3 |

**migration 不可重复执行**：重跑 `0001` 会因表已存在而失败。本地重建用 `make db-reset`。版本追踪表留到 W2 再定。

---

## 工程配置

| 文件 | 职责 | 阶段 |
|---|---|---|
| `Makefile` | `build` / `vet` / `fmt` / `fmt-check` / `migrate` / `db-reset` / `probe`。 | W1D1 |
| `.env.example` | §10 的环境变量模板，入库。真实 `.env` 在 `.gitignore` 内。 | W1D1 |
| `.gitignore` | 排除 `.env`、`.workbuddy/`（本地工作记录，含本机口令）、Go 构建产物。 | W1D1 |
| `go.mod` / `go.sum` | 依赖锁定。仅 `go-ethereum` 与 `pgx/v5` 两个直接依赖，均来自 §3。 | W1D1 |

---

## 脚本（`scripts/`）

| 文件 | 职责 | 阶段 |
|---|---|---|
| `scripts/reorg-test/run.sh` | reorg 端到端验证（§8.1 验收）。fork 主网 → 索引到链头 → 冒充 USDC 持有者转一笔真实转账 → 再挖两块 → `anvil_reorg` 深度 5 → 断言：分叉点识别、陈旧日志被删、块头换成新哈希、游标回退。使用独立库 `chainlens_anvil`，不触碰开发库。 | W2D2 |

---

## 文档

| 文件 | 职责 | 阶段 |
|---|---|---|
| `HANDOFF.md` | 项目设计与决策的唯一依据（定位、边界、ADR、数据模型、里程碑、规范）。**不随意改动**。 | 项目起始 |
| `docs/FILES.md` | 本文件。 | W1D3 |
| `docs/BENCHMARK.md` | W1 压测记录：`eth_getLogs` 容量、归档限制、批量对吞吐的影响、内存与存储占用、幂等结论、全量索引时间估算。W4 写 README 时从这里取数。 | W1 收尾 |
| `docs/SESSION-HANDOFF.md` | **新会话的第一个入口**。记录当前进度、本机环境与口令位置、数据快照、已踩过的坑、待决策清单、遗留问题、常用命令、协作约定。与 `HANDOFF.md` 的分工：那份是设计依据（几乎不变），这份是运行状态（随时更新）。 | W3D1 结束 |

---

## 规划中、尚未创建

按 §5 的目录结构，以下文件在对应里程碑之前不创建（避免空目录）：

| 文件 | 里程碑 / 状态 |
|---|---|
| `internal/profile/balance.go`、`approval.go` | W3D2（地址画像：持仓聚合 / 授权解析）。**路径以 §5 为准，不要放进 `store`** |
| `internal/profile/interaction.go` | W3D2，`§5` 未列，属新增偏离（见 `docs/SESSION-HANDOFF.md` §5.1 第 5 条） |
| `internal/api/router.go`、`handlers.go`、`cmd/api/main.go` | W3D3（查询服务） |
| `web/` | W4（前端） |
| `cmd/redecode/main.go` | **用户明确暂缓**。用途：按区间删除 `decoded_event` 后回退游标重解，支持分批与断点。起因是切换 ABI 不会回溯重解，存量 3,978 条 `undecodable` 需要它才能安全修正。详见 `docs/SESSION-HANDOFF.md` §8。 |
