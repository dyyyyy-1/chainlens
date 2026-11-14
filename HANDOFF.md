# ChainLens 项目交接文档

> 单人可交付的区块链工程案例项目。本文件是新会话的唯一起始上下文，包含定位、架构、数据模型、难点、里程碑与环境清单。

---

## 1. 项目定位

**一句话**：从零实现一个 EVM 日志索引器，把任意合约的事件流转换为结构化数据，并据此生成地址画像（代币持仓、未撤销授权、合约交互历史）。

**项目名**：ChainLens

**做这个项目的目的**：作为技术案例，证明对链上数据模型（区块 / 日志 / 回执 / 最终性）的工程理解，而不是"会用 web3.js 调接口"。

**核心价值主张**：解决一个真实且普遍的问题——绝大多数用户不知道自己授权过哪些合约、授了多少额度。ChainLens 把这件事查清楚。

---

## 2. 边界：做什么，不做什么

### 做

- 指定合约的事件全量索引（从部署块到链头）
- reorg 安全：最终确认区与未确认区分离，游标与数据写入同事务
- 事件解码：代理合约（EIP-1967）、anonymous event、indexed 参数超限、无 ABI 退化
- 地址画像：ERC20 持仓聚合、未撤销 approve 列表（标注无限额度）、近期交互合约
- 查询 API + 一个轻量前端（3 个页面以内）
- 与第三方数据源（区块浏览器 / The Graph）对拍，误差为 0

### 不做（明确排除，避免范围蔓延）

- 不做通用多链支持，只做以太坊主网（Sepolia 仅用于调试）
- 不做实时 WebSocket 订阅，第一版纯轮询；WS 作为可选优化
- 不做完整 ABI 自动获取的产品化（用 Etherscan API，失败就退化）
- 不做用户系统、不做鉴权、不做部署上线
- 不做合约开发（本项目不写 Solidity）
- 不做地址画像的全量预计算，采用按需查询 + 缓存

---

## 3. 技术栈与选型理由

| 层 | 选型 | 理由 |
|---|---|---|
| 索引器 | Go 1.27 | 已有背景，四周内出活优先于极致性能 |
| RPC 客户端 | `github.com/ethereum/go-ethereum`（ethclient + abi 包） | 事实标准，ABI 解码不用自己写 |
| 数据库 | PostgreSQL 17 | 需要 JSONB、部分索引、`ON CONFLICT`、`COPY` |
| DB 驱动 | `github.com/jackc/pgx/v5` | 原生协议，支持 COPY 批量写 |
| HTTP | `chi` 或 `gin` | 路由轻量，无需框架能力 |
| 日志 | 标准库 `log/slog` | 结构化输出，不引第三方 |
| 前端 | Vite + React + TypeScript + Tailwind | 体量小，只做展示 |
| 前端数据层 | TanStack Query | 缓存与加载态开箱可用 |
| 本地测试链 | Foundry（anvil） | 支持 `anvil_reorg`，可直接验证 reorg 逻辑 |

### 关键决策记录（ADR）

**ADR-1：不引入 Redis。**
索引游标必须与区块数据写入在同一数据库事务内提交，否则进程崩溃会导致"游标前进但数据丢失"。跨存储无法保证这一点。单实例部署也不需要分布式锁。结论：游标用 Postgres 表，`UPDATE ... RETURNING` 即可。

**ADR-2：用 `eth_getLogs` 范围查询，不逐块 `eth_getBlockByNumber`。**
一次 `eth_getLogs` 可覆盖 2000 个块，请求数量降低三个数量级。逐块扫 2000 万块在单机上不可行。

**ADR-3：地址画像不落库，按需查询 + 缓存。**
为全量地址预计算持仓需要扫描所有 Transfer 事件并建立多列索引，成本极高且大部分地址从不被查询。改为：查询时用 `eth_getLogs` 按 `topic1`/`topic2` 过滤该地址的 Transfer，内存缓存 5 分钟。

**ADR-4：地址与哈希统一用 `bytea`。**
定长、索引更小、比较更快，且天然规避大小写与前缀不一致的问题。对外输出时统一转小写 hex，另提供 `logs_hex` 视图便于调试。

**ADR-5：主网做数据展示，Sepolia 做本地调试。**
主网数据有说服力，Sepolia 块少、迭代快。索引器配置化，通过环境变量切换。

---

## 4. 系统架构

```
┌─────────────────────────────────────────────┐
│  web (React)   地址画像 / 事件检索 / 统计      │
└────────────────────┬────────────────────────┘
                     │ HTTP
┌────────────────────▼────────────────────────┐
│  cmd/api       查询服务（只读）                │
└────────────────────┬────────────────────────┘
                     │
┌────────────────────▼────────────────────────┐
│  internal/profile   画像聚合（持仓/授权/交互）  │
│  internal/store     pgx 读写 + COPY 批量写     │
└────────────────────▲────────────────────────┘
                     │
┌────────────────────┴────────────────────────┐
│  cmd/indexer   扫描调度 / 游标 / reorg 处理    │
│    ├ internal/chain     RPC 封装、getLogs      │
│    └ internal/decoder   ABI 解码、代理识别      │
└────────────────────┬────────────────────────┘
                     │ JSON-RPC
              ┌──────▼──────┐
              │  Ethereum   │
              └─────────────┘
```

索引器与 API 是两个独立进程，共享同一个数据库。索引器写入，API 只读，天然隔离。

---

## 5. 目录结构

```
chainlens/
├── cmd/
│   ├── indexer/main.go          # 索引器入口
│   └── api/main.go              # 查询服务入口
├── internal/
│   ├── chain/
│   │   ├── client.go            # RPC 封装、重试、限流
│   │   └── logs.go              # getLogs 范围拉取
│   ├── decoder/
│   │   ├── registry.go          # 事件签名注册表
│   │   ├── abi.go               # ABI 加载与解码
│   │   └── proxy.go             # EIP-1967 代理识别
│   ├── indexer/
│   │   ├── scanner.go           # 主循环
│   │   ├── cursor.go            # 游标推进（事务内）
│   │   └── reorg.go             # 分叉检测与回扫
│   ├── profile/
│   │   ├── balance.go           # 持仓聚合
│   │   └── approval.go          # 授权解析
│   ├── store/
│   │   ├── models.go
│   │   ├── logs.go              # COPY 批量写
│   │   └── cursor.go
│   ├── api/
│   │   ├── router.go
│   │   └── handlers.go
│   └── config/config.go
├── migrations/                  # 纯 SQL，按序号命名
├── web/                         # 前端
├── scripts/
│   └── reorg-test/              # anvil 分叉验证脚本
├── docs/
└── Makefile
```

---

## 6. 数据模型

```sql
-- 索引游标（单行表）
CREATE TABLE index_cursor (
    id              smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    chain_id        bigint      NOT NULL,
    next_block      bigint      NOT NULL,
    finalized_block bigint      NOT NULL,
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- 块头，用于 reorg 检测
CREATE TABLE block_header (
    number       bigint      PRIMARY KEY,
    hash         bytea       NOT NULL,
    parent_hash  bytea       NOT NULL,
    block_time   timestamptz NOT NULL,
    is_finalized boolean     NOT NULL DEFAULT false
);

-- 原始日志（不可变事实）
CREATE TABLE chain_log (
    block_number bigint      NOT NULL,
    block_time   timestamptz NOT NULL,
    tx_hash      bytea       NOT NULL,
    log_index    integer     NOT NULL,
    contract     bytea       NOT NULL,
    topic0       bytea,
    topic1       bytea,
    topic2       bytea,
    topic3       bytea,
    data         bytea       NOT NULL,
    PRIMARY KEY (tx_hash, log_index)
);
CREATE INDEX idx_log_block   ON chain_log (block_number);
CREATE INDEX idx_log_topic1  ON chain_log (topic1);
CREATE INDEX idx_log_topic2  ON chain_log (topic2);

-- 解码结果（可与 chain_log 合并，先分表便于对照调试）
CREATE TABLE decoded_event (
    tx_hash    bytea   NOT NULL,
    log_index  integer NOT NULL,
    event_name text    NOT NULL,
    payload    jsonb   NOT NULL,
    PRIMARY KEY (tx_hash, log_index)
);

-- 合约元信息缓存
CREATE TABLE contract_meta (
    address        bytea PRIMARY KEY,
    implementation bytea,
    abi            jsonb,
    abi_source     text,          -- etherscan | manual | fallback
    resolved_at    timestamptz NOT NULL DEFAULT now()
);

-- 调试视图
CREATE VIEW logs_hex AS
SELECT block_number, '0x'||encode(tx_hash,'hex') AS tx_hash, log_index,
       '0x'||encode(contract,'hex') AS contract,
       '0x'||encode(topic0,'hex')   AS topic0
FROM chain_log;
```

**三个约束是正确性的最后防线**：

1. `chain_log` 主键 `(tx_hash, log_index)` —— 重扫不产生重复
2. `index_cursor` 单行 CHECK —— 不可能出现两个游标
3. `contract_meta.address` 主键 —— 代理识别结果不会重复解析

---

## 7. 核心接口

### 索引器内部

```go
// 拉取区间日志
type LogFetcher interface {
    Fetch(ctx context.Context, from, to uint64, addr common.Address) ([]types.Log, error)
}

// 解码单条日志
type Decoder interface {
    Decode(log types.Log) (DecodedEvent, error)
}

// 事务内推进游标
type CursorStore interface {
    Advance(ctx context.Context, tx pgx.Tx, to, finalized uint64) error
}
```

### HTTP API

```
GET /api/events?contract=0x..&from=..&to=..&limit=..&offset=..
GET /api/address/{addr}/balances
GET /api/address/{addr}/approvals      # 含无限额度标记
GET /api/address/{addr}/interactions
GET /api/stats                          # 已索引块数、事件总数、追平延迟
```

---

## 8. 五个技术难点与验收方式

### 8.1 reorg 安全（核心）

朴素实现"扫到就写"会在分叉时留下脏数据。正确做法：

- 新块写入前，校验 `parent_hash` 是否等于本地最大块的 `hash`
- 不匹配则回退：删除该块及之后的所有 `chain_log` 与 `block_header`，重扫
- 确认数未达标的块标记 `is_finalized = false`，达到后置 true 并推进 `finalized_block`
- **游标推进必须与区块写入在同一事务**

**验收**：本地 anvil fork 主网，用 `anvil_reorg` 制造深度 5 的分叉，索引器应回滚 5 块并重扫，最终数据与分叉后链一致。

### 8.2 代理合约识别

按 EIP-1967 读取 `0x360894a13ba1a3210667c828492db98dca3e2076cc3735a920a3ca505d382bbc` 槽位得到 implementation，再取该地址的 ABI。

**验收**：选一个 USDC 这类代理合约，能正确解出 Transfer 事件；未识别时能退化为只存 topic 不报错。

### 8.3 事件解码边界

四种必须覆盖：

- `anonymous` 事件：`topic0` 不是签名，不能作为过滤条件
- indexed 参数超过 3 个：EVM 只有 4 个 topic，超出的哈希进 `data` 不可逆，标记 `undecodable`
- ABI 缺失或版本不符：退化到原始日志层，不中断索引
- `data` 长度不合法：记录并跳过，不 panic

**验收**：单元测试覆盖以上四种，全部通过。

### 8.4 吞吐与幂等

- `COPY` 批量写，不逐条 INSERT
- `ON CONFLICT (tx_hash, log_index) DO NOTHING`
- getLogs 分批，单批 2000 块，并发 4~8

**验收**：同一区间连扫两遍，表行数不变；记录单块平均耗时与总追平时间。

### 8.5 地址画像

持仓不能从全量 Transfer 直接聚合（成本过高）。改为：

- 用 `eth_getLogs` 以 `topic1 = addr` 与 `topic2 = addr` 分别查询 Transfer 事件
- 按 token 合约聚合余额变化
- approve 单独按 `Approval` 事件查询，`value = 2^256-1` 标记为无限额度

**验收**：取 3 个真实地址，持仓与授权列表与 Etherscan 一致。

---

## 9. 四周里程碑

| 周 | 目标 | 验收 |
|---|---|---|
| W1 | 单合约扫描 → 解码 → 落库 → 幂等重扫 | 同区间扫两遍行数不变 |
| W2 | reorg 回滚 + 断点续扫 + 代理合约支持 | anvil 分叉验证通过 |
| W3 | 查询 API + 地址画像（持仓 / 授权 / 交互） | 与 Etherscan 数字一致 |
| W4 | 前端 + 对拍报告 + README | 全量数据与第三方误差为 0 |

**第一周第一天清单**：

1. 初始化 Go module，拉取 go-ethereum 与 pgx
2. 建库建表，跑通 migrations
3. 写 `chain.Client`，能拉到指定块的 header 与一段区间的 logs
4. 硬编码一个合约地址，把日志原样打印出来，确认 topic 结构

---

## 10. 环境准备清单

### 已具备（本机已确认）

| 项 | 版本 |
|---|---|
| Go | 1.27.1 |
| Node | 22.22.2 |
| npm | 10.9.7 |
| Git | 2.55.0 |
| Python | 3.13.14 |

### 需要安装

**1. PostgreSQL 17**

```bash
winget search postgresql          # 先确认包 ID
winget install --id PostgreSQL.PostgreSQL.17 -e
```

安装时设置 postgres 超级用户密码，记下来。装完后把 `C:\Program Files\PostgreSQL\17\bin` 加入 PATH，否则 `psql` 不可用。

建库：

```sql
CREATE USER chainlens WITH PASSWORD 'chainlens_dev';
CREATE DATABASE chainlens OWNER chainlens;
```

**2. Foundry（提供 anvil / cast）**

Windows 原生支持，从 GitHub Releases 下载 `foundry_*_win32_amd64.zip`，解压后把 `bin` 目录加入 PATH。

```bash
anvil --version
cast --version
```

若安装受阻，退化方案：用 Hardhat（`npm i -D hardhat`）的 `hardhat_reset` + 快照做 reorg 模拟，功能等效但操作更繁琐。

**3. pnpm（可选，前端包管理更快）**

```bash
npm install -g pnpm
```

**4. 数据库客户端（可选）**

DBeaver 或 pgAdmin，方便看表。不加也能用 `psql`。

### 明确不需要安装

| 项 | 原因 |
|---|---|
| Docker | 单人项目无部署需求，Windows 上还多一层网络问题 |
| Redis | ADR-1 已排除，游标必须与数据同事务 |
| WSL | 本机虚拟化已关闭；且本项目工具链在 Windows 全部原生可用。引入 WSL 只会增加跨文件系统 IO 与 NAT 网络的成本，收益为零。需要 Linux 环境时用 GitHub Actions 而非本地双系统 |

### 需要申请（免费档足够）

**1. RPC 端点**

- 开发调试：`https://ethereum-rpc.publicnode.com`（公共端点，无需 key，限流较严）
- 正式索引：Alchemy 或 Infura 免费档（注册后拿 HTTPS key）

Alchemy 免费档约 3 亿计算单元/月，单合约索引完全够用。

**2. Etherscan API Key**

用于拉取合约 ABI 和识别代理实现。免费档 5 req/s，够用。注册地址 etherscan.io，在 API Keys 页面创建。

### 环境变量模板（.env）

```bash
DATABASE_URL=postgres://chainlens:chainlens_dev@localhost:5432/chainlens?sslmode=disable
RPC_HTTP_URL=https://ethereum-rpc.publicnode.com
ETHERSCAN_API_KEY=
CHAIN_ID=1
TARGET_CONTRACT=
START_BLOCK=0
CONFIRMATIONS=12
BATCH_BLOCK_RANGE=2000
LOG_LEVEL=info
```

`.env` 加入 `.gitignore`，只提交 `.env.example`。

---

## 11. 代码规范

### Go

- 格式化：`gofumpt`（比 gofmt 严格）
- 静态检查：`golangci-lint run`
- 错误处理：用 `fmt.Errorf("...: %w", err)` 包装，保留调用链
- 禁止 `panic` 处理业务错误，只在初始化阶段允许
- 命名：包内变量 `camelCase`，导出 `PascalCase`，常量 `UpperSnakeCase`
- 布尔值使用 `is/has/should` 前缀（`isFinalized`、`hasReorg`）
- 接口定义在**使用方**所在包，不在实现方

### TypeScript

- `strict: true`，禁止 `any`，所有接口定义 `interface` 或 `type`
- Prettier：2 空格缩进、单引号、行尾分号
- 命名：变量/函数 `camelCase`，组件/类型 `PascalCase`
- 导入顺序：第三方 → 内部模块 → 相对路径，各自字母序

### 提交信息

Conventional Commits：

```
feat(indexer): add reorg rollback with parent hash check
fix(decoder): handle anonymous events without topic0
docs(readme): add benchmark result of 2M blocks
```

---

## 12. 可写进 README 的量化指标

项目完成后必须能填上这四个数字，比任何功能列表都有说服力：

1. 已索引区块数、事件条数
2. 单块平均处理耗时、端到端追平最新块的延迟
3. 重扫 100 万块的耗时
4. 与第三方数据源对拍的误差（应为 0）

---

## 13. 新会话起始提示

把下面这段直接粘贴到新会话：

> 我要开始开发 ChainLens 项目，完整设计见工作区根目录的 `HANDOFF.md`，请先读这个文件。
>
> 当前状态：环境已探测，Go 1.27.1 / Node 22 / Git 已具备；PostgreSQL 与 Foundry 待安装。
>
> 本次任务：先帮我完成第一周第一天的工作——初始化 Go 项目骨架（按 HANDOFF.md 第 5 节的目录结构）、写好 migrations、实现 `internal/chain` 的 RPC 封装，能够拉取指定区块头和一段区间的日志并打印出来。
>
> 动手前请先复述一遍你的实现思路，等我确认后再写代码。
