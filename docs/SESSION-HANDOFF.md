# 会话交接文档

> 目的：让一个全新的会话在读完本文件后能无缝接手。**先读这份，再读 `HANDOFF.md`（设计依据）与 `docs/FILES.md`（代码地图）。**
>
> **冲突处理规则**：本文件与 `HANDOFF.md` 不一致时，**以 `HANDOFF.md` 为准**，并必须把偏离登记到 §5.1。本文件只描述运行状态，不重新定义设计。
>
> 最后更新：2026-11-17

---

## 0. 一句话现状

W1（索引链路）、W2（代理识别 + 最终性 + reorg 回滚）、W3D1（Sourcify ABI 接入）已完成并推送到 GitHub。
**下一步是 W3D2：地址画像查询层。**

---

## 1. 仓库与远程

| 项 | 值 |
|---|---|
| 远程 | `https://github.com/dyyyyy-1/chainlens.git`（**Public**） |
| 分支 | `main`，本地与远程同步 |
| 提交数 | 以 `git log --oneline \| wc -l` 为准（硬编码必然过期，故不写死） |
| 推送凭据 | 系统级 `credential.helper=manager` + Windows 凭据库已有 `github.com` 条目，**`git push` 无需交互** |

推送偶发 `schannel: failed to receive handshake`。**这不是凭据问题**——同期 `curl https://github.com` 返回 200、`git ls-remote` 能通，是写方向的 TLS 抖动，直接重试即可。若持续失败，可试 `git -c http.sslBackend=openssl push`。

**TLS 报错可能根本不是 TLS 问题，先查代理。**（2026-11-17 与 11-19 各踩一次）

本机 `git config http.proxy` 指向 `http://127.0.0.1:10808`，而环境变量 `HTTPS_PROXY`
在两次会话里分别是 `55692` 和 `49252`。**这些端口会随代理客户端重启来回变化**，
所以「哪个是活的」必须现测，不能凭上次结论。

诊断（照这个走，不要先怀疑凭据或 TLS）：

```bash
git config --get-all http.proxy ; echo "env=$HTTPS_PROXY"
for p in 10808 10809 1080 7890 7891 8899 2080 $(echo "$HTTPS_PROXY" | sed 's#http://##; s#/$##'); do
  code=$(curl -s -o /dev/null -w "%{http_code}" -m 3 -x "http://127.0.0.1:$p" https://github.com)
  echo "port $p -> $code"
done
curl -s -o /dev/null -w "direct=%{http_code}\n" -m 10 --noproxy '*' https://github.com
```

修复用**单次覆盖**，不改全局配置：

```bash
git -c http.proxy=http://127.0.0.1:<探到的端口> push origin main
# 直连可用时：git -c http.proxy= push origin main
```

`schannel` 与 `openssl` 两种后端在同一代理下都会失败，所以「换后端」这个老办法治标不治本。
11-19 的现象更典型：10808 先死、几分钟后又活，直接重试就成功了——**先探活，再重试，再谈 TLS**。

---

## 2. 本机环境

| 组件 | 版本 / 位置 | 备注 |
|---|---|---|
| Go | 1.27.1 windows/amd64 | |
| PostgreSQL | 17.12，服务 `postgresql-x64-17`，端口 5432 | `C:\Program Files\PostgreSQL\17\bin` 已入用户 PATH |
| Foundry | 1.9.0（anvil/cast/forge/chisel/solar） | `~/.foundry/bin`，已入用户 PATH |
| gofumpt | v0.12.0 | 在 `~/go/bin/gofumpt`，**不在 PATH**，调用需全路径 |
| gh CLI | 2.104.0 | 已装但**未登录**；推送不依赖它 |
| make | **未安装** | `Makefile` 写好了但当前跑不了，直接用 `go` 命令 |

**口令（本机开发用）**：PostgreSQL 超级用户 `postgres` / `CL_local_pg17x9`；应用用户 `chainlens` / `chainlens_dev`。
数据库：`chainlens`（开发库）、`chainlens_anvil`（reorg 测试专用，可随意重置）。

**go 依赖拉取**必须加环境变量，否则 `GOSUMDB` 默认指向国内镜像会 TLS 超时：

```bash
GOSUMDB=sum.golang.org GOPROXY=https://goproxy.cn,direct go mod tidy
```

---

## 3. 进度总览

| 阶段 | 内容 | 状态 |
|---|---|---|
| W1 | 区间扫描 → `eth_getLogs` → COPY 幂等落库 → 解码 → 重扫行数不变 | ✅ 完成并压测 |
| W2 | 多槽位代理识别、最终性窗口（`is_finalized`）、reorg 回滚、anvil 验证 | ✅ 完成 |
| W3D1 | Sourcify ABI 拉取、按实现地址取 ABI、验证区间未解码率 1.48% → 0% | ✅ 完成 |
| W3D2 | 地址画像查询层（`Holdings` / `Approvals` / `Interactions`），`internal/profile` | ✅ 完成 |
| W3D3 | HTTP API（5 个端点）+ `cmd/api` | ✅ 完成 |
| W4 | 回放对拍（`cmd/verify`）、README、BENCHMARK 重测 | ✅ 完成 |
| 加固 | 事件键集分页、按 IP 限流、stats 计数缓存 | ✅ 完成 |
| W4+ | 前端 `web/`（`HANDOFF` §9 标为可选） | ⬜ **未开始，待决定** |

---

## 4. 当前数据状态

`chainlens` 库快照（2026-11-20 12:10，索引器**运行中**）：

| 指标 | 值 |
|---|---|
| `chain_log` 行数 | 2,793,986 |
| 已索引区间 | 26422714 .. 26453825（连续，无缺口） |
| `chain_log` 总大小 | 1020 MB |
| `decoded_event` | decoded 2,790,008 / undecodable 3,978（存量） |
| `index_cursor` | `next_block=26453826`，`finalized_block=26453813`，`chain_id=1` |
| `block_header` | 31,112 行 = 区间块数（连续），其中 `is_finalized=false` 12 行 |
| `contract_meta` | USDC，`abi_source=sourcify`，72 条目，implementation `0x4350...02dd` |
| 事件分布 | Transfer 约占 90%，其余为 Approval / Mint / Burn / AuthorizationUsed / unknown 3,978 |

目标合约：主网 USDC `0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48`，链 ID 1。

索引器按需手动 `go run`，非常驻。追平后停顿会让 `next_block` 落到 publicnode 的约 10,000 块滚动窗口之外，届时会被判成归档请求而失败（§8.5）。

那 3,978 条 `undecodable` 是**内置 ABI 时代的存量**，切 Sourcify 不会回溯重解，见 §8。

---

## 5. 架构要点

不要在这里重复代码说明，直接看 **`docs/FILES.md`**——它逐文件写了职责和"为什么这样拆"。

三条关键约定，改动时不要破坏：

1. **ADR-1**：区块数据与游标必须在同一事务内提交，否则崩溃会出现"游标前进但数据丢失"。
2. **接口在使用方声明**：`LogFetcher` / `HeaderFetcher` / `LogStore` / `EventDecoder` 定义在 `internal/indexer`，`ChainReader` 定义在 `internal/decoder`；`chain` 与 `store` 只提供具体类型，不反向依赖。
3. **解码失败不中断入库**：`decoder.Decode` 不返回 error，失败记为 `undecodable` 状态入库（§8.3）。

### 5.1 对 `HANDOFF.md` 的偏离登记

**规则**：`HANDOFF.md` 是设计依据，原则上不改。凡是实现与它不一致的地方，必须在**这张表**里登记，写明原文、实际做法、原因、批准人。**没登记的偏离视为缺陷。**

| # | HANDOFF 原文 | 实际做法 | 原因 | 批准 |
|---|---|---|---|---|
| 1 | §5 `internal/store/` 只有 `models.go` / `logs.go` / `cursor.go` | 另有 `headers.go`、`decoded.go`、`meta.go` | 块头、解码结果、合约元信息各有独立写入路径，塞进 `logs.go` 会混淆职责 | 用户确认 |
| 2 | §6 `decoded_event` 无状态列 | 增加 `status text NOT NULL DEFAULT 'decoded' CHECK (status IN ('decoded','undecodable'))`（`migrations/0002`） | §8.3 要求标记 `undecodable`，原表无处安放 | 用户确认（方案 C） |
| 3 | §2/§3 ABI 用 Etherscan API | 改用 Sourcify v2（无需 key） | 不接受代注册第三方账号；Sourcify 免 key 且实测可用 | 用户确认 |
| 4 | **ADR-3 画像用 `eth_getLogs` 按 topic1/topic2 实时查 + 内存缓存 5 分钟** | **改查本地 `chain_log`（已有 `topic1`/`topic2` 索引）** | 免费端点 `eth_getLogs` 跨度上限：publicnode 约 10,000 块、drpc 10,000 块，画像只能覆盖约 33 小时，产品语义失效。本地库无跨度限制且无 RPC 成本，同时仍不违反 ADR-3 的本意（**不预计算**） | 用户确认（方案 B） |
| 5 | §5 `internal/profile/` 只有 `balance.go` / `approval.go` | 增加 `interaction.go` | 交互、持仓、授权三类聚合口径不同，合并到两个文件会让语义混杂 | 用户确认 |
| 6 | §8.1 隐含索引未确认区 | W1 阶段只扫到 `head - confirmations`，`is_finalized` 恒为 true | 当时为缩小 W1 范围；**已在 W2D2 回归 §8.1**，此条已关闭 | — |
| 7 | §7 `Decoder.Decode(log) (DecodedEvent, error)` | `decoder.Decode(log) Event`，**不返回 error**，失败用 `Status`/`Reason` 表达 | 返回 error 会诱导调用方中断整批，与 §8.3「解码失败不中断索引」直接冲突 | 本会话说明，未单独确认 |
| 8 | §5 `internal/store/` 只列 `models` / `logs` / `cursor` | 再加 `query.go`（只读查询面：事件、区间、统计）与 `Pool()` | 写路径服务于索引器的事务语义，读路径服务于查询服务；混在一个文件会让两种事务要求纠缠。`Pool()` 是「一个进程一套连接池」的接线点 | 用户授权本次自行决定 |
| 9 | §5 查询服务路由用 **chi** | 用标准库 `ServeMux`（Go 1.22+ 的「方法 + 路径参数」模式） | 本服务只有五条静态路由，`ServeMux` 完全覆盖；为一个 router 增加一条直接依赖不划算，`go.mod` 仍保持两个直接依赖 | 用户授权本次自行决定 |
| 10 | §7 `GET /api/stats` 含「追平延迟」 | 给 `stale_seconds`（最新区块时间与当前时刻之差），不给「落后多少块」 | 块级落后必须接 RPC 查链头，会让只读服务多一个外部依赖与一种失败模式；时间差同样能判断数据新鲜度 | 用户授权本次自行决定 |
| 11 | §5 进程入口只有 `cmd/indexer`（W3D3 已增 `cmd/api`） | 再加 `cmd/verify`（回放对拍） | 对拍是 W4 的验收手段，需要独立进程与退出码；写成 shell 脚本就无法复用 `chain` 的重试语义与 `store` 的类型 | 用户确认（W4 范围选定「对拍 + README」） |
| 12 | §7 `GET /api/events` 只有 `offset` | 增加 `cursor`（键集分页），`offset` 保留兼容；两者互斥，同时给出报 400 | `OFFSET` 让数据库先数过前面所有行，翻得越深越慢；静默取其一会让客户端分页悄悄错位 | 用户确认（先做加固） |
| 13 | 加固方案原定「stats 计数表」 | 改为 **10 秒进程内 TTL 缓存** | 计数表要在「批提交事务」与「reorg 回滚」两处同时维护，任何一处漏改都会让统计静默漂移且难以发现；缓存的失效方式是过期，不会积累错误 | 用户确认（改选 TTL 缓存） |

补充说明（第 4 条）：ADR-3 的原始理由是"为全量地址预计算持仓成本极高"。查本地库**不做预计算**、不建聚合表、不建缓存表，只有查询，因此与该理由不冲突。查询侧的硬约束是用户另行提出的："必须有过滤、禁止全量扫描"，落实为 `LIMIT` + `statement_timeout` + 地址左填充 32 字节。

---

## 6. 已踩过的坑（重点，别重复）

### RPC 端点

| 端点 | 归档 `eth_getLogs` | JSON-RPC 批量 | 结论 |
|---|---|---|---|
| `ethereum-rpc.publicnode.com` | ❌ 仅最近约 **10,000** 块（实测 head−9,999） | ✅ 100 个/批 | **当前选用** |
| `eth.drpc.org` | ✅ | ❌ 最多 3 个/批 | 见下方 |
| `rpc.flashbots.net` | ✅ | ❌ 10 个即被拒 | 不适用 |

- 其他候选：`eth.llamarpc.com` 报 525，`1rpc.io/eth` 报 method not available，`cloudflare-eth.com` 报 internal error，**Blockscout 在本机网络被代理拦截（CONNECT tunnel 502）**。
- **publicnode 不限制 `eth_getLogs` 的返回条数**：2000 块 USDC 返回 139,910 条、86.8 MB，此前"约 1 万条上限"的假设是错的。
- **drpc 免费档 `eth_getLogs` 单次跨度上限 10,000 块**（`ranges over 10000 blocks are not supported on free plan`），与 publicnode 的归档窗口（同样约 10,000 块）基本持平，换端点并不能扩大实时查询范围。因此画像没有走实时查链，改查本地库。
- **publicnode 的归档窗口实测约 10,000 块，且是滚动的**（2026-11-17 二分探测：链头 26,433,602 时，26,423,603 可回放、26,423,602 被拒）。此前记的「约 8,000」偏保守；但**不要在任何地方硬编码这个宽度**，`cmd/verify` 因此改成运行时二分探测。10,000 块 ≈ 33 小时链上时间。
- **`RequestTimeout` 必须 ≥ 60 秒**：2000 块 USDC 单请求实测 41.65 秒 / 97.4 MB，20 秒会每次都踩线并重传近百 MB。
- **评估端点必须按实际调用模式测**（批量大小、并发），只测单请求会选错。

### 链上数据

- **USDC 不是 EIP-1967 代理**。`0x3608...bbc`（实现槽）与 `0xa3f0...d50`（信标槽）都是全零，真实实现槽是 ZeppelinOS 遗留槽 `0x7050c9e0f4ca769c69bd3a8ef740bc37934f8e2c036e5a723fd8ee048ed3f8c3`。只做 EIP-1967 会把 USDC 判成非代理。
- **`chain_log.topic0~3` 存的是 32 字节完整 topic，不是 20 字节地址**。按地址查询必须先左填充到 32 字节，否则索引失效。
- **`types.Log.Index` 是 `uint` 不是 `uint32`**，需显式转换。
- **`types.Log.Data` 为 nil 时**必须转 `[]byte{}` 再入库，否则违反 `NOT NULL`。
- **Sourcify 必须用 v2 接口**：`https://sourcify.dev/server/v2/contract/{chainId}/{addr}?fields=abi`。旧路径 `repo.sourcify.dev/contracts/full_match/...` 返回 404。查代理地址只得到 2 个事件（`AdminChanged`/`Upgraded`），**必须按实现地址拉**。

### anvil / reorg 验证

- `anvil_reorg` **不能改写从主网 fork 来的历史块**，报 `-32001 Resource not found`。只能作用于本地挖出的块。
- 参数是二元组：`cast rpc anvil_reorg <depth> '[]'`。
- 重写窗口内的**空块可能被按相同时间戳重建、哈希不变**，真实分叉点可能晚于 `head - depth`，断言要按区间判而不是精确等值。
- 造真实日志的办法：`anvil_impersonateAccount` + `anvil_setBalance` + `cast send --unlocked`，用 Morpho Blue `0xbbbb...ffcb`（约 1.41 亿 USDC）作转出方。

### 工具链与流程

- **不要用 `nohup` / `setsid` 起长任务**：shell 退出时进程会被杀，白等一轮。用后台任务机制。
- `gofumpt` 不在 PATH，用 `~/go/bin/gofumpt`。
- 推送前必须跑敏感文件扫描：`git ls-files | grep -E '^\.env$|\.workbuddy'`。
- **切换 ABI 不会回溯重解已有数据**（见 §8）。

---

## 7. 待决策清单

| 事项 | 现状 | 需要谁定 |
|---|---|---|
| `cmd/redecode`（按区间重解、可断点续跑） | 用户明确**暂不需要**，记录在案 | 用户 |
| W4+ 是否做前端（`web/`） | HANDOFF §9 规划为可选，未开工。API 已就绪（5 个端点 + `coverage`），前端可直接接 | 用户 |
| 是否补第三方对拍 | Etherscan 需本人注册 key；Blockscout 在本机不可达。当前只有同源回放对拍，**不能证明语义与第三方一致** | 用户 |

**已定的决策**（不要再翻案，除非用户主动提）：

- 端点用 publicnode 免费档；需要归档时切 Alchemy（**但不接受代注册**，见 §9）。
- ABI 来源用 Sourcify，不用 Etherscan（**偏离 §3，见 §5.1 第 3 条**）。
- 画像数据源用**本地 `chain_log`**（方案 B），不走实时 `eth_getLogs`（**偏离 ADR-3，见 §5.1 第 4 条**）。
- 画像代码放 `internal/profile/`，按 §5；不放 `store`。
- **`coverage` 随每个响应返回**，不只放在 `/api/stats`。空结果必须能区分「地址确实没记录」与「这段历史还没索引」。
- 画像查询的用户硬约束：**必须有过滤、禁止全量扫描**。落实方式：`LIMIT`、连接级 `statement_timeout`、地址左填充、不建任何预计算表或缓存表。
- 不加 `(topic1, topic0)` 复合索引（实测 252k 行索引够用，省磁盘）。
- 一个进程只建**一套连接池**：`cmd/api` 用 `store.New` 建池后把 `st.Pool()` 交给 `profile.New`。

---

## 8. 已知遗留问题

1. **存量 3,978 条 `undecodable` 未重解**。它们是内置 ABI 时代写入的，切 Sourcify 后新写入的数据未解码率为 0（已验证区间 26426000..26426199：237 → 0）。要全量修正需要：
   ```sql
   DELETE FROM decoded_event
   WHERE (tx_hash, log_index) IN (
       SELECT tx_hash, log_index FROM chain_log WHERE block_number BETWEEN $a AND $b
   );
   UPDATE index_cursor SET next_block = $a, finalized_block = $a;
   ```
   然后重跑索引器。**注意 `chain_log` 有主键冲突保护，重扫不会产生重复行**。
   这件事需要 `cmd/redecode` 才能做得安全（可断点、可分批），用户暂缓。

2. **`Makefile` 跑不了**：本机没装 `make`。命令直接手敲。

3. **没有 `.gitattributes`**：Git 一直提示 `LF will be replaced by CRLF`。项目是纯 Go + SQL，暂无害；将来写 shell 脚本时建议加 `* text=auto eol=lf`。

4. **索引器非常驻**：没有 systemd/计划任务，靠手动 `go run`。数据会落后。

5. **publicnode 窗口限制**：索引器停机超过约 33 小时，`next_block` 会落到窗口外，`eth_getLogs` 报 `Archive requests require a personal token`。重启时把 `START_BLOCK` 和游标拨回窗口内。

---

## 9. 任务进度与下一步

### W3D2 —— 地址画像查询层（✅ 已完成 2026-11-17）

产出（`internal/profile/`，四个文件，逐文件职责见 `docs/FILES.md`）：`Holdings` / `Approvals` / `Interactions`，外加 `profile_test.go`。

设计要点：

- **窄接口**：profile 只声明 `Querier`（一个 `BeginTx` 方法），`*pgxpool.Pool` 直接满足，不 import `store`。
- **SQL 归 profile 所有**：画像是带业务规则的只读聚合（净额、去重、无限额度），放 store 会让 store 承担业务语义。
- **执行骨架**：`READ ONLY` 事务 → `set_config(..., true)` 设 `statement_timeout='3s'` 与 `work_mem='16MB'` → 查询 → 回滚。用事务级设置，避免连接池复用时把配置残留给下一个请求。
- **必须带过滤**：`LIMIT` 收敛到 200；超时（SQLSTATE 57014）翻译成 `ErrQueryTimeout`，供 API 层映射 503。
- **不建预计算表、缓存表、物化视图、DB 函数**：`u256` 换算以 SQL 常量内联。

实测（836,939 行 / 10,196 块时；最热地址 56.5k 行）：

| 方法 | 最坏耗时 | 冷地址 |
|---|---|---|
| `Holdings` | 200 ms | 0.2 ms |
| `Approvals`（2,905 条 → 13 个 spender） | 14.6 ms | — |
| `Interactions` | 105 ms | 0.06 ms |

三条踩过的坑：

1. **PostgreSQL 17 不支持 `bit(256)::numeric`**，无法直接把 32 字节 `data` 转数值。解法是拆 8 个 32 位分肢乘 2 的幂。已与 `decoded_event` 的十进制 `payload.value` 在 24 万条 Transfer 上逐条对账，零差异。
2. **默认 `work_mem` 会导致磁盘溢写**：最热地址的 `GroupAggregate` 排序需要约 9 MB，4 MB 默认值下溢写 7 MB。事务内设 16 MB 后排序回内存，耗时 225 ms → 200 ms。
3. **`topic0` 过滤的选择率估算偏差 200 倍**（估 259 行 / 实际 56,499 行），planner 因此选了 `GroupAggregate`。不要用 `enable_sort=off` 这类全局开关压制，靠 `work_mem` 解决即可。

**语义边界（已写进代码注释，不要在 API 层改写成「余额」）**：

- `Holding.Net` 是**已索引区间内的净变化**，不是链上余额，活跃地址可能为负。要真实余额必须查链上 `balanceOf()`。
- `Approval` 是区间内该 `(contract, spender)` 的最新一条日志，**不代表链上当前 allowance**：区间外的后续授权或撤销不可见。
- `Interactions` 因当前只索引一个合约，只会返回一行；多合约后才有分布意义。
- 因此 HANDOFF §8.5 的验收「与 Etherscan 数字一致」在部分区间上不成立，W4 对拍目标应改为「区间内事件聚合一致」。

### W3D3 —— HTTP API（✅ 已完成 2026-11-17）

产出：`internal/api/{router,handlers,handlers_test}.go`、`cmd/api/main.go`，以及 `internal/store/query.go`（事件检索 / 覆盖区间 / 统计）。

五个端点，全部只读，响应统一带 `coverage`：

| 方法 | 路径 | 主要参数 |
|---|---|---|
| GET | `/api/events` | `contract`、`from`、`to`、`limit`、`offset` |
| GET | `/api/address/{addr}/balances` | `limit` |
| GET | `/api/address/{addr}/approvals` | `limit` |
| GET | `/api/address/{addr}/interactions` | `limit` |
| GET | `/api/stats` | — |

实测（848,735 行、索引器停机）：`/api/events` 2 ms、`/api/stats` 176 ms、`/balances` 214 ms、`/interactions` 102 ms、`/approvals` 12 ms。

实现要点：

- **连接池只有一个**：`cmd/api` 调一次 `store.New`，再用 `profile.New(st.Pool())` 复用同一个池。实测 `pg_stat_activity` 中该库只有 1 条业务连接，不存在两套池。
- **超时分层**：请求预算 5 s > profile 的 `statement_timeout` 3 s，让数据库先超时，从而返回可解释的 503 `query_timeout`，而不是笼统的请求超时。
- **错误映射**：`ErrQueryTimeout` / `context.DeadlineExceeded` → 503；其余 → 500，SQL 细节只进日志（测试里断言响应不含 `SQLSTATE` 与表名）。
- **输出约定**：地址与哈希统一小写（ADR-4），时间统一 UTC RFC3339，金额一律十进制字符串（`uint256` 超出 JS 安全整数范围）。
- 非 GET 请求由 `ServeMux` 的 `GET` 模式自动回 405。

W4 之前需要知道的限制：

- `/api/events` 的 `OFFSET` 深翻页会退化，需要游标分页时再补。
- `/api/stats` 的计数是 `count(*)` 全表聚合，全量索引 USDC 后会变成秒级，届时要改计数表或物化视图（与「不预计算」约束冲突，需要重新权衡）。
- 无鉴权、无限流，只适合本机或内网。

### W4 —— 收尾（✅ 对拍与 README 已完成 2026-11-17；前端未做）

产出：`cmd/verify/main.go`（回放对拍）、`README.md`、`docs/BENCHMARK.md` 第 9~10 节（重测与对拍记录），以及 `store.LogsByRange`（对拍用的区间读取）。

**对拍结果**（`go run ./cmd/verify`）：

```
汇总：19 批，链上 783,105 条，库内 783,105 条
  缺失 0，多余 0，字段不一致 0，block_time 不一致 0
结果：一致
```

覆盖库内 848,735 条中的 92.3%；其余 889 块（26,422,714..26,423,602）超出归档窗口，不可回放，未计入判定。

三条重要结论：

1. **第三方交叉验证做不了。** Etherscan API 需 key（不接受代注册，`.env` 里 `ETHERSCAN_API_KEY` 为空），Blockscout 在本机网络连不通（`http=000`）。因此 README 的「局限」明确写了：对拍证明的是**索引器忠实记录**，不是**语义理解与第三方一致**。不要再把 W4 描述成「与 Etherscan 数字一致」。
2. **归档窗口实测约 10,000 块（head−9,999），比此前记的 8,000 宽**，且滚动。`cmd/verify` 用二分探针在运行时确定边界，不写死宽度。
3. **吞吐比 W1 慢约一倍**：单块 53 ms（原 25.5~29.8 ms），约 18.9 块/秒（原 34~39）。日志密度只涨 20%，单条成本却上升，原因无法区分（端点限流 / 带宽 / 端点负载）。README 与 BENCHMARK 记录的是同一环境下的新事实。

**仍未做**：

- 前端 `web/`（`HANDOFF` §9 标为可选，等用户定）。
- 真正的身份体系（现在只有可选的静态 token）。

### 加固（✅ 完成 2026-11-19）

- **键集分页**：`/api/events` 支持 `cursor=<block>:<log_index>`，走行比较 `(block_number, log_index) < (...)`，与倒序排序键同向。`offset` 保留兼容，两者互斥（同时给出报 400）。只在填满一页时返回 `next_cursor`。
- **按 IP 限流**：固定窗口，缺省 120 次/分钟（`HTTP_RATE_LIMIT`，`0` 关闭）。**不读 `X-Forwarded-For`**：没有可信代理，该头可被任意伪造。
- **stats 缓存**：10 秒进程内 TTL，只包 `Stats`，错误不进缓存。放弃计数表的理由见 §5.1 第 13 条。
- **可选鉴权**：`API_TOKEN` 非空时要求 `Authorization: Bearer <token>`，为空则完全不介入（默认关闭，升级代码不会突然要凭据）。定长比较，方案名大小写不敏感。**它是静态共享密钥，不是身份体系**——不能区分调用者、不能单独撤销。

测试：`internal/api` 20 个用例，其中缓存覆盖「命中 / 恰好过期 / 错误不缓存 / ttl=0 关闭」四种边界。

### 缺口补齐（2026-11-19）

停机两天，`next_block` 落后链头 13,854 块，远超 publicnode 的 head−9,999 窗口，**直接重启必然第一批就报归档错误**。做法：

1. 把块头批量做成可配置（`RPC_HEADER_BATCH`）——drpc 免费档只允许 3，写死 100 会直接失败。
2. 用 drpc（归档可查）从游标处往上补：`RPC_HTTP_URL=https://eth.drpc.org`、`BATCH_BLOCK_RANGE=100`、`RPC_HEADER_BATCH=3`。
3. 补到 publicnode 窗口内（约 head−9,000，留 1,000 块余量抵消补的过程里窗口上移）后切回 publicnode。

**drpc 的 `eth_getLogs` 跨度上限实测是 100 块**，不是此前文档写的 10,000——这才是批量必须降到 100 的原因。速度约 2.5 块/秒（每 100 块要 34 次块头批量），4,870 块约 32 分钟。

教训：**免费端点的时间窗口决定了「停机超过一天就等于断档」**。想保持连续历史，索引器要么常驻，要么每次停机后都走一次这套补缺口流程。

---

## 10. 常用命令

```bash
cd E:/chainlens
export PATH="/c/Program Files/PostgreSQL/17/bin:$PATH"
export PATH="$HOME/.foundry/bin:$PATH"

# 质量门禁
go build ./... && go vet ./... && go test ./... && ~/go/bin/gofumpt -l .

# 跑索引器（依赖本地 .env，已 gitignore）
go run ./cmd/indexer

# 跑查询服务（默认 :8080，HTTP_ADDR 可覆盖）
go run ./cmd/api
curl -s localhost:8080/api/stats
curl -s "localhost:8080/api/address/0x000000000004444c5dc75cb358380d2e3de08a90/balances"

# 探测某区间的日志布局
go run ./cmd/probe

# 回放对拍（链上日志 vs 库内日志，逐字段；退出码 0 一致 / 1 有差异 / 2 失败）
go run ./cmd/verify
go run ./cmd/verify -from 26432530 -to 26433029 -batch 500

# reorg 端到端验证（用独立库，不碰开发库）
bash scripts/reorg-test/run.sh

# 查库
PGPASSWORD=chainlens_dev psql "postgres://chainlens:chainlens_dev@localhost:5432/chainlens?sslmode=disable"
```

`.env` 关键项：`DATABASE_URL`、`RPC_HTTP_URL=https://ethereum-rpc.publicnode.com`、`CHAIN_ID=1`、
`TARGET_CONTRACT=0xA0b8...eB48`、`START_BLOCK=26422714`、`CONFIRMATIONS=12`、`BATCH_BLOCK_RANGE=2000`、
`HTTP_ADDR=:8080`（仅查询服务读取）。

---

## 11. 协作约定（用户明确要求，必须遵守）

1. **改代码前先复述方案并等确认**。这是用户的硬性规则，不要跳过。
2. 提交信息遵循 **Conventional Commits**，按模块拆分提交，不要一次 `git add -A` 混成一个。
3. **新增/删除/重命名文件必须同步更新 `docs/FILES.md`**，该文档里写明了这条规则。
4. 不要扩大改动范围；需要扩展时先问。
5. **不接受代注册第三方账号**（Etherscan 等）：注册需本人邮箱、人机验证、以本人名义接受 ToS，属于冒用身份。已因此改用 Sourcify。
6. 工作过程记录在 `.workbuddy/memory/YYYY-MM-DD.md`（append-only，已 gitignore），交接前要写全。
