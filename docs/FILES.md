# 文件职责清单

> **维护规则**：新增、删除或重命名任何文件，必须同步更新本表。评审时本表与目录结构不一致视为缺陷。

路径全部相对于仓库根目录。

---

## 入口（`cmd/`）

| 文件 | 职责 | 阶段 |
|---|---|---|
| `cmd/indexer/main.go` | 索引器进程入口。装载配置 → 建连接池与 RPC 客户端 → 识别目标合约的代理实现并写入 `contract_meta` → 构造解码器 → 启动 `indexer.Scanner`，响应 Ctrl-C 优雅退出。含 `validate()` 强制校验 `DATABASE_URL` 与 `TARGET_CONTRACT`。代理识别失败只告警不阻塞索引。 | W1D2、W2D1 |
| `cmd/probe/main.go` | 临时探测工具。拉指定合约一段区间的日志并原样打印（块号、tx、log_index、topic0~3、data 长度），用于确认 topic 布局。**索引器稳定后可删除**。 | W1D1 |
| `cmd/api/main.go` | 查询服务入口。装载配置 → **只建一个连接池**（`store.New`）→ `profile.New(st.Pool())` 复用同一个池 → 启动 `http.Server` 并处理优雅退出。含 `ReadHeaderTimeout` / `IdleTimeout` / `Shutdown` 三项超时。 | W3D3 |
| `cmd/verify/main.go` | 回放对拍工具。对同一区间重新 `eth_getLogs`，与 `chain_log` 按 `(tx_hash, log_index)` 逐字段比对（含 `block_time`），报告缺失/多余/不一致。**运行时二分探测归档边界**，只核对可回放部分，不把「超出窗口」误报成「数据缺失」。退出码 0/1/2。 | W4 |
| `cmd/verify/main_test.go` | 证明比对函数**能发现差异**（缺失 / 多余 / data 改动 / 块时间漂移），以及区间收敛的边界。没有这组用例，「0 差异」的结论不可信。 | W4 |
| `cmd/verify/etherscan.go` | `-source etherscan`：把 Etherscan V2 的 `getLogs` 作为**独立数据源**做抽样对拍，复用 `diffBatch`。数值字段兼容 hex 与十进制；3 次/秒限速；单次触到 1000 条上限直接报错（免费档会静默截断）。`sampleRanges` 负责均匀抽样。`reconcileIndexless` 处理第三方返回空 `logIndex` 的条目：按 `(tx, topics, data)` 唯一匹配，零候选计 missing，多候选报错而不是猜。 | 交接后（2026-11-20） |
| `cmd/verify/etherscan_test.go` | `parseQuantity` / `isBlankQuantity` / `sampleRanges` / `reconcileIndexless` 的表驱动测试，含「多候选必须报错」「空 logIndex 不得退化成 0」两条边界。 | 交接后（2026-11-20） |
| `cmd/redecode/main.go` | **离线重解**存量 `undecodable` 日志：从 `chain_log` 读原始日志、用 `contract_meta` 里的 ABI 重解、按块区间分批覆盖写 `decoded_event`。**不访问 RPC、不碰 `index_cursor`、不改 `chain_log`**，因此与索引器可并存。默认 dry-run，`-apply` 才写库。 | 交接后（2026-11-20） |

---

## 领域层（`internal/`）

### `internal/chain` —— JSON-RPC 访问

| 文件 | 职责 | 阶段 |
|---|---|---|
| `client.go` | 连接管理、超时、重试、链 ID 校验。导出 `New` / `HeaderByNumber` / `LatestBlockNumber` / `HeadersByRange` / `Close`。 | W1D1、W1D2 |
| `logs.go` | `Fetch`：按区块区间走 `eth_getLogs` 拉日志（ADR-2），带区间合法性校验与 2000 块上限。 | W1D1 |
| `token.go` | ERC-20 只读调用：`BalanceOf` / `Decimals`。选择器手写（`0x70a08231` / `0x313ce567`），不解析整份 ABI。**刻意不重试**：它服务在线查询，调用方只有几秒预算，重试会把「快速失败并降级」变成「拖垮整个响应」。 | 交接后（2026-11-20） |

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
| `decoded.go` | `InsertDecoded`（索引器用，冲突 `DO NOTHING`）、`UpsertDecoded`（重解用，冲突 `DO UPDATE`）、`UndecodableSummary`、`UndecodableLogs`（区间内未解码的原始日志）。两种冲突动作的差别就是「重扫不应改写」与「重解必须改写」。 | W1D3、交接后 |
| `meta.go` | `UpsertContractMeta` / `LoadContractMeta`：代理识别结果的读写。`abi` 用 `COALESCE` 更新，避免重复解析时把已获取的 ABI 覆盖成空。 | W2D1 |
| `cursor.go` | `EnsureCursor` / `LoadCursor` / `Advance`。游标推进必须由调用方传入事务。 | W1D2 |
| `query.go` | 只读查询面：`ListEvents`（按合约/区间过滤，支持键集游标，条件按需拼接）、`Coverage`（已索引区间）、`Stats`（水位 + 事件计数）、`LogsByRange`（区间原始日志，供 `cmd/verify` 对拍与导出）。与写路径分开：写路径服务于索引器的事务语义，读路径服务于查询服务。行扫描逻辑由 `scanLogRecord` 共用，避免两条查询的列顺序各写一遍。 | W3D3、W4 |

**`logs.go` 为什么这么绕（临时表 → COPY → INSERT ... ON CONFLICT）**：
PostgreSQL 的 `COPY` 协议不支持 `ON CONFLICT`，而 §8.4 同时要求「COPY 批量写」与「冲突时静默跳过」。解法是事务内建 `TEMP TABLE ... (LIKE chain_log) ON COMMIT DROP`（`LIKE` 默认不带主键约束，灌入不会因重复中断），COPY 进临时表后再用 `INSERT ... SELECT ... ON CONFLICT (tx_hash, log_index) DO NOTHING` 合并。返回值是 `RowsAffected()`，即**实际新增行数**——重扫时恒为 0，这就是幂等的判定依据。

**`headers.go` / `decoded.go` 为什么超出 §5 的「models / logs / cursor」三文件**：§5 是骨架示意，`block_header` 与 `decoded_event` 的写入需要落点。塞进 `logs.go` 会把两种写职责混在一起，故各起一文件。这是已知且经确认的偏离。

**`Store` 结构与构造器为什么放在 `models.go`**：同样是为了不再增加文件数（没有独立的 `store.go`）。

**`Store.Pool()` 为什么存在**：`internal/profile` 需要连接池，而一个进程只能有一套池。若让 profile 自建，数据库侧看到的总连接数是两套上限之和，两边超时与回收互不知情，排查连接数异常时会误导判断。`cmd/api` 因此只调一次 `store.New`，把同一个池交给 `profile.New`。该方法只供同进程只读组件复用，写路径仍在 Store 上。

### `internal/profile` —— 地址画像查询

| 文件 | 职责 | 阶段 |
|---|---|---|
| `balance.go` | `Profile` 类型与 `New`、`Querier` 窄接口（只声明 `BeginTx`）、`runReadOnly`（只读事务 + `statement_timeout` + `work_mem`）、`clampLimit`、`topic`（地址左填充 32 字节）、`u256Expr`（`data` 前 32 字节转 `numeric`）、`Holdings`。 | W3D2 |
| `approval.go` | `Approvals`：`DISTINCT ON (contract, topic2)` 取区间内最新一条授权，`2^256-1` 标记 `IsUnlimited`。 | W3D2 |
| `interaction.go` | `Interactions`：按合约汇总事件数与最近活动块，不按 `topic0` 过滤。 | W3D2 |
| `profile_test.go` | 纯函数单测（limit 收敛、topic 填充、十进制解析、`2^256-1`）+ 受 `CHAINLENS_TEST_DSN` 门控的集成测试（默认 skip），后者含两条路径的净额对账。 | W3D2 |

画像的三个方法都**只读本地 `chain_log`**，不查链（`docs/SESSION-HANDOFF.md` §5.1 第 4 条）。三条约定：

1. **必须带过滤**：`LIMIT` 由 `clampLimit` 收敛到 200，`statement_timeout` 由数据库中断慢查询，超时翻译成 `ErrQueryTimeout` 供 API 层映射 503。
2. **地址必须左填充**：`chain_log.topic1/topic2` 是 32 字节，用 20 字节地址查询会退化成全表扫描。
3. **不建预计算表、缓存表或物化视图**：查询即计算。

**`u256Expr` 为什么要拆成 8 段**：PostgreSQL 17 不支持 `bit(256)::numeric`，无法直接把 32 字节转数值。拆成 8 个 32 位分肢（每段必在 `bigint` 范围内）再乘 2 的幂，全程精确。该表达式已与 `decoded_event` 的十进制 `payload.value` 在 24 万条 Transfer 上逐条对账，零差异。

**`Profile` 为什么只依赖窄接口而不是 `*pgxpool.Pool`**：§11 要求接口在使用方声明，`profile` 不 import `store`，也不绑死具体连接池实现。`*pgxpool.Pool` 恰好满足 `BeginTx`，直接传入即可。

### `internal/api` —— 查询服务 HTTP 层

| 文件 | 职责 | 阶段 |
|---|---|---|
| `router.go` | 路由（5 个端点）、`EventStore` / `ProfileReader` / `TokenReader` 三个窄接口、请求中间件（超时预算、panic 恢复、访问日志、状态码记录）、按 IP 限流的接线。`withBalance` 在配置了 `Tokens` 时注入余额服务，未配置则保持 nil。 | W3D3、加固、交接后 |
| `handlers.go` | 参数解析与校验（含键集游标）、错误映射（超时 → 503、内部错误 → 500 且不泄露 SQL）、响应 DTO 与 `coverage` 封装、hex 与时间格式化。`/balances` 用专用 DTO：`data` 是区间净变化，`onchain` 是可选的链上余额，两者语义不同故不混在同一层。 | W3D3、加固、交接后 |
| `balance.go` | 链上余额的编排：`TokenReader` 窄接口、并发查询（每个代币一个 goroutine，上限 5 个）、两级缓存（余额 10s / decimals 1h）、过期清理。单代币失败只影响该单元格，并回 `status=unavailable` + 顶层 `onchain.error`。 | 交接后（2026-11-20） |
| `static.go` | 托管前端构建产物并做 SPA 回退。`/api/*` 的未匹配路径**不回退**到 index.html，而是返回 JSON 404——否则客户端会拿到 200 + HTML，把「接口不存在」误读成「接口返回了奇怪的东西」。路径先 `path.Clean` 再拼接，防止穿越。 | 前端 |
| `auth.go` | 可选的 Bearer token 校验：`API_TOKEN` 为空时完全不介入。该变量支持**逗号分隔多口令**以便轮换（新旧并存，不必让调用方同一刻切换），逐个定长比较且**不短路**（短路会通过响应时间泄漏命中顺序）。`tokenFingerprint` 给访问日志提供 SHA-256 前 4 字节指纹，能区分调用方而不落明文。**这仍是静态共享密钥，不是身份体系。** | 加固、交接后 |
| `stats_cache.go` | 给 `/api/stats` 的计数加 10 秒进程内缓存（只包 `Stats`，不包事件检索与覆盖区间）。持有互斥锁完成查询，顺带做单飞，避免并发打进来时多个请求同时做全表聚合。错误不进缓存。 | 加固 |
| `ratelimit.go` | 按来源 IP 的固定窗口限流，缺省 120 次/分钟，`0` 关闭。限流表设容量上限并清理过期条目。**刻意不读 `X-Forwarded-For`**：本服务没有可信代理，该头可被伪造。 | 加固 |
| `handlers_test.go` | 全离线的假实现测试：路由匹配、地址/区间/limit 校验、超时映射、错误不泄露内部细节、大写地址统一转小写、`2^256-1` 精确输出。 | W3D3 |

**为什么用标准库 `ServeMux` 而不是 chi**：`HANDOFF.md` 第 5 节写的是 chi，但 Go 1.22+ 的 `ServeMux` 已支持 `GET /api/address/{addr}/balances` 这种「方法 + 路径参数」模式，本服务五条静态路由完全覆盖。为一个 router 增加一条直接依赖不划算，差异已登记在 `docs/SESSION-HANDOFF.md` §5.1。

**为什么 `coverage` 随每个响应返回，而不是只在 `/api/stats`**：单看某个地址的空结果，无法区分「该地址确实没有记录」与「这段历史还没索引」。前者是数据结论，后者是数据缺口，混为一谈会得出错误判断。

**为什么 `handlers.go` 里没有 SQL**：查询与聚合在 `store`（事件、统计、区间）和 `profile`（画像）里，api 只做校验、编排与序列化。把 SQL 放进 handler 会让「换传输层」变成重写。

### `internal/config`

| 文件 | 职责 | 阶段 |
|---|---|---|
| `config.go` | 用标准库解析 `.env`（约 40 行，不引 godotenv），再映射到 `Config`。真实环境变量优先于文件；`.env` 不存在不报错。`RPC_HTTP_URL` 与 `CHAIN_ID` 必填——**仅对 `Load`（索引器）**；`LoadServer`（查询服务 / `cmd/redecode`）只要求 `DATABASE_URL`，并把 `RPC_HTTP_URL` / `CHAIN_ID` 当作**可选**（缺一即关闭链上余额）。`API_TOKEN` 经 `splitTokens` 支持逗号分隔多值。 | W1D1、W3D3、交接后 |

---

## 数据定义（`migrations/`）

| 文件 | 职责 | 阶段 |
|---|---|---|
| `0001_init.sql` | §6 的 5 张表 + `logs_hex` 调试视图，逐字转写。整文件包在 `BEGIN/COMMIT` 内。 | W1D1 |
| `0002_decoded_event_status.sql` | 给 `decoded_event` 补 `status` 列（`decoded` / `undecodable`）。起因：§8.3 要求标记 undecodable，而 §6 没有状态列。状态独立成列而非塞进 `payload`。 | W1D3 |

**migration 不可重复执行**：重跑 `0001` 会因表已存在而失败。本地重建用 `make db-reset`。版本追踪表留到 W2 再定。

---

## 前端（`web/`）

| 文件 | 职责 |
|---|---|
| `web/package.json` | 依赖与脚本。React 19 + Vite 8 + TypeScript，无 UI 框架、无状态库。 |
| `web/vite.config.ts` | 开发期把 `/api` 代理到 `127.0.0.1:8080`；生产由 `cmd/api` 同源托管，不需要代理。 |
| `web/src/api.ts` | 与后端 DTO 一一对应的类型与请求封装。金额字段一律 `string`，禁止转 `number`。`ApiError` 保留后端错误码，便于界面区分参数错与超时。 |
| `web/src/format.ts` | 金额与地址格式化。**全程字符串运算**：uint256 超出 `Number.MAX_SAFE_INTEGER`，转 number 是不可逆的精度丢失。 |
| `web/src/App.tsx` | 顶栏、三个标签页、token 输入（存 localStorage）。改 token 时以 token 为 key 重新挂载，自动按新凭据重取。 |
| `web/src/components/StatsPanel.tsx` | 索引统计。指标卡 + 覆盖区间 + 数据新鲜度。 |
| `web/src/components/AddressProfile.tsx` | 地址画像。三个表（余额与净变化 / 授权 / 交互），各表下方写明语义边界。链上余额列只在 `onchain.enabled` 时出现；取不到就写「获取失败」，`decimals` 为 null 就写「精度未知」——不猜、不回 0。 |
| `web/src/components/EventExplorer.tsx` | 事件检索。过滤 + 游标翻页；参数按事件类型（Transfer / Approval / 其他）压成一行。 |
| `web/src/components/CoverageBar.tsx` | 覆盖区间条。空结果必须能区分「确实没有记录」与「这段历史还没索引」。 |
| `web/src/styles.css` | 暗色设计系统（CSS 变量、卡片、表格、语义标签）。无第三方样式库。 |

**为什么不用 UI 框架**：三个视图、两张表、若干输入框，手写 CSS 足以覆盖；引入组件库会带来版本跟进与主题覆盖成本，且页面风格要贴合「终端工具」而非「后台管理系统」。

**已知短板**：`net`（区间净变化）仍按 USDC 6 位换算——它是按合约聚合的，而 `Holdings` 还没有返回每个合约的 decimals。链上余额已用接口返回的 `decimals`，不再写死（F4 完成后 `net` 也一并解决）。

## 工程配置

| 文件 | 职责 | 阶段 |
|---|---|---|
| `Makefile` | `build` / `vet` / `fmt` / `fmt-check` / `migrate` / `db-reset` / `probe`。**本机没有 make，实际入口是 `scripts/check.sh`**；Makefile 保留给装了 make 的环境。 | W1D1 |
| `scripts/check.sh` | 本地质量门禁：`build` / `vet` / `test` / `gofumpt` + 前端 `lint` / `build`。与 CI 保持一致，避免「本地绿、CI 红」。`SKIP_WEB=1` 可只查 Go。 | 交接后（2026-11-20） |
| `.gitattributes` | 统一 LF（`* text=auto eol=lf`），二进制文件不做转换。消除 Windows 上持续的 CRLF 警告，也防止 shell 脚本被 CRLF 破坏。 | 交接后（2026-11-20） |
| `.github/workflows/ci.yml` | GitHub Actions：Go 侧 `gofumpt` / `build` / `vet` / `test`，前端侧 `npm ci` / `lint` / `build`。**不启动数据库**：`internal/profile` 的集成测试由 `CHAINLENS_TEST_DSN` 门控，未设置时按设计跳过。 | 交接后（2026-11-20） |
| `LICENSE` | MIT。仓库是 Public，未声明许可等于默认保留所有权利，会挡住复用。 | 交接后（2026-11-20） |
| `.env.example` | §10 的环境变量模板，入库。真实 `.env` 在 `.gitignore` 内。W3D3 追加 `HTTP_ADDR`（仅查询服务读取）。 | W1D1、W3D3 |
| `.gitignore` | 排除 `.env`、`.workbuddy/`（本地工作记录，含本机口令）、Go 构建产物。 | W1D1 |
| `go.mod` / `go.sum` | 依赖锁定。仍只有 `go-ethereum` 与 `pgx/v5` 两个直接依赖。查询服务的路由用标准库 `ServeMux`，没有引入 chi（见 `internal/api` 一节）。 | W1D1、W3D3 |

---

## 脚本（`scripts/`）

| 文件 | 职责 | 阶段 |
|---|---|---|
| `scripts/reorg-test/run.sh` | reorg 端到端验证（§8.1 验收）。fork 主网 → 索引到链头 → 冒充 USDC 持有者转一笔真实转账 → 再挖两块 → `anvil_reorg` 深度 5 → 断言：分叉点识别、陈旧日志被删、块头换成新哈希、游标回退。使用独立库 `chainlens_anvil`，不触碰开发库。 | W2D2 |

---

## 文档

| 文件 | 职责 | 阶段 |
|---|---|---|
| `README.md` | 项目门面：定位、快速开始、架构、数据模型、关键设计的取舍、实测性能、API 与错误码、**局限（部分区间 / 无第三方交叉 / 无鉴权）**。 | W4 |
| `HANDOFF.md` | 项目设计与决策的唯一依据（定位、边界、ADR、数据模型、里程碑、规范）。**不随意改动**。 | 项目起始 |
| `docs/FILES.md` | 本文件。 | W1D3 |
| `docs/BENCHMARK.md` | W1 压测记录：`eth_getLogs` 容量、归档限制、批量对吞吐的影响、内存与存储占用、幂等结论、全量索引时间估算。W4 写 README 时从这里取数。 | W1 收尾 |
| `docs/SESSION-HANDOFF.md` | **新会话的第一个入口**。记录当前进度、本机环境与口令位置、数据快照、已踩过的坑、待决策清单、遗留问题、常用命令、协作约定。与 `HANDOFF.md` 的分工：那份是设计依据（几乎不变），这份是运行状态（随时更新）。 | W3D1 结束 |

---

## 规划中、尚未创建

按 §5 的目录结构，以下文件在对应里程碑之前不创建（避免空目录）：

| 文件 | 里程碑 / 状态 |
|---|---|
| （暂无） | `cmd/redecode/main.go` 已于 2026-11-20 以「离线重解」形态创建，不再需要「删数据 + 拨游标 + 重扫」的路径。 |
