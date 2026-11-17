# 个人客户端审计 2F：成本数字、事件流与导出脱敏

- 审计对象：`costmeter/`、`modelprice/`、`secrets/`（降级为"仅导出用"之后）、`events/`
- 审计方式：只读代码 + 新增 `audit_*_test.go`（不改实现，未 commit）
- 时间：2026-11-18
- 核心问题：**"这个会话花了多少钱"这个数字，能不能直接拿去给用户看？**

## 0. 结论（一句话）

**能看，但必须带着两个限定**：① DeepSeek 一族的价目已过时且缺现行型号，用它算出的钱会**偏小到 0**；② `estimated=true` 在契约里已要求渲染 `≈`，但 `docs/` 里没有任何一处写这件事，且 `by_model` 行内没有 `estimated` 字段。计量本身的精度、归因、缓存折扣、回滚往返都**没有发现问题**（100 万次累加零漂移、Snapshot/Restore 逐纳元相等）。

## 1. 复现方式

```bash
export PATH=$PATH:/usr/local/go/bin
export GOCACHE=/data/zeokeel-rig/.audit-scratch/aud3f/gocache

# 审计用例（非 race：包含吞吐/耗时门槛断言）
CGO_ENABLED=0 go test ./costmeter/ ./modelprice/ ./events/ ./secrets/ ./kit/ -run 'TestAudit' -count=1 -v

# 审计目标四包（race：仅断言正确性，跳过性能门槛）
CGO_ENABLED=1 go test -race ./costmeter/ ./modelprice/ ./events/ ./secrets/ -count=1
```

实跑结果：非 race 全绿；`-race` 四包全绿（costmeter 8.8s / modelprice 1.0s / events 14.5s / secrets 6.1s）。

新增测试文件（全部为审计用例，未改动任何实现）：

| 文件 | 用例数 | 覆盖 |
|---|---|---|
| `costmeter/audit_money_test.go` | 9 | 精度/纳元精确性/亚纳元丢失/快照往返/预算边界/warn 一次/缓存折扣/脏数据/归因 |
| `modelprice/audit_table_test.go` | 5 | 表自洽/名称形态/前缀边界/本地与未知分流/现行型号覆盖 |
| `events/audit_ndjson_test.go` | 6 | Unicode 与行边界/未知 kind 透传/未知 version 拒绝/超长原子性/10 万吞吐/截断行 |
| `secrets/audit_export_test.go` | 5 | 导出入口与 Redactor 一致（8 条样例）/已知明文/幂等/1 MiB 耗时/零误伤 |
| `kit/audit_costmeter_test.go` | 3 | RunRequest.Model 归因/未定价标记/Estimated 语义 |
| `events/audit_race_{on,off}_test.go`、`secrets/audit_race_{on,off}_test.go` | — | 让吞吐/耗时门槛只在非 `-race` 构建下断言（`-race` 下慢 3–12 倍，属检测开销） |

## 2. 最严重的三条

### P1：DeepSeek 价目过时 + 缺现行型号 → 成本**偏小到 0**

- 表内 `deepseek-chat`：input `$0.27` / cached `$0.07` / output `$1.10`；`deepseek-reasoner`：`0.55 / 0.14 / 2.19`。
- DeepSeek 官方现行页（2026-11 抓取，见 §3）给出的是 **V4.1-Flash / V4-Pro** 两档，且是 **峰谷双价**：
  - `deepseek-flash`：cache-hit `$0.003`(off-peak)/`$0.006`(peak)，cache-miss `$0.15`/`$0.30`，output `$0.60`/`$1.20`
  - `deepseek-v4-pro`：`0.022`/`0.044`，`0.66`/`1.32`，`1.98`/`3.96`
- 后果分两类：
  1. **命名漂移**：当前主力型号名（`deepseek-flash`、`deepseek-v4-pro`）在表里**完全没有** → `Lookup` 返回未命中 → 成本记 0、只出现在 `unpriced_models`。用户看到"本次会话 $0.00"。
  2. **缓存价高 12–23 倍**：若仍用旧名 `deepseek-chat`，cached 单价 `0.07` 是官方 `0.003–0.006` 的 12–23 倍 → 缓存命中越多，**高估越狠**（与 P1 第 1 条方向相反，同一个家族两个方向都错）。
- **最小修法**：把 DeepSeek 段改成现行两档（并补一个"峰价"标记或在 `Note` 里写明"表内取峰值"），旧名保留为前缀别名映射到 Flash 价：

```go
"deepseek-flash":   {Model: "deepseek-flash", Provider: "deepseek", InputPerMillion: 0.30, CachedInputPerMillion: 0.006, OutputPerMillion: 1.20, Estimated: true, Note: "取峰时价；谷时为其一半", Updated: TableUpdated},
"deepseek-v4-pro":  {Model: "deepseek-v4-pro", Provider: "deepseek", InputPerMillion: 1.32, CachedInputPerMillion: 0.044, OutputPerMillion: 3.96, Estimated: true, Note: "取峰时价；谷时为其一半", Updated: TableUpdated},
```

> 取峰值而不是均值是刻意的：**宁高不低**（与包内既有原则一致），避免用户以为花得比实际少。

### P1：`o3-mini` 被前缀匹配成 `o3`，金额高估约 1.8 倍

- `lookupKey` 的最长前缀匹配对 `o3-mini` 命中 `o3`（`-` 是合法边界），于是按 `$2 / $8` 计价。
- 实际 `o3-mini` 是 `$1.10 / $4.40`（与表内 `o4-mini` 同价）。**这是"看错模型"级别的错误**：同一会话里用 o3-mini 越多，金额越虚高。
- 类似风险：`gpt-5.1` 会按 `gpt-5` 计价（若官方价不同则同样错）。
- **最小修法**：给已知的"便宜兄弟"补显式条目（精确匹配优先于前缀），并在 `modelprice/doc.go` 写一句"前缀匹配会命中同族更贵的父型号，新增子型号必须显式加条目"：

```go
"o3-mini": {Model: "o3-mini", Provider: "openai", InputPerMillion: 1.10, CachedInputPerMillion: 0.275, OutputPerMillion: 4.40, Estimated: true, Updated: TableUpdated},
```

（另有测试 `TestAuditPricePrefixDoesNotCrossModelFamilies` 守着"粘在一起的名字不会命中"这一半，`gpt-4oz`/`qwen-maxx` 等都正确拒绝。）

### P2：预算告警的整数比较会溢出（> 约 $1.15M 的预算判定出错）

`meter.go` 的判定是 `m.total.costNano*10000 >= m.budgetNano*m.warnBasisPoints`。当 `budgetNano*8000 > MaxInt64`（预算 ≳ **$1,152,922**）时两侧 int64 溢出回绕，判定方向可能翻转。实测（每 token = $1 的自定义 pricer，扫描 $0.9M–$6M）：

```
budget=$1150000 pct=81% warned=false want=true     ← 该告警却没告警
budget=$3500000 pct=79% warned=true  want=false    ← 没到阈值却告警
budget=$5800000 pct=79% warned=true  want=false
```

个人客户端预算通常远小于此，属"窄边界"，但修法极便宜——**构造期算好阈值纳元数**，比较不再相乘：

```go
// WithBudget / WithWarnAt 之后统一重算（New 里算一次即可）
m.warnNano = int64(math.Round(float64(m.budgetNano) * float64(m.warnBasisPoints) / 10000))
// Add 内：
if m.hasBudget && !m.warned && m.total.costNano >= m.warnNano { ... }
```

## 3. 价目表抽查（8 条，含官方 URL）

| # | 模型 | 表内 input/cached/output | 官方 | 结论 | 出处 |
|---|---|---|---|---|---|
| 1 | `deepseek-chat` | 0.27 / 0.07 / 1.10 | Flash: 0.15–0.30 / **0.003–0.006** / 0.60–1.20 | ❌ cached 高 12–23× | [DeepSeek Models & Pricing](https://api-docs.deepseek.com/quick_start/pricing) |
| 2 | `deepseek-reasoner` | 0.55 / 0.14 / 2.19 | V4-Pro: 0.66–1.32 / **0.022–0.044** / 1.98–3.96 | ❌ input 略低、cached 高 3–6× | 同上 |
| 3 | `deepseek-flash` | **缺条目** | 0.30 / 0.006 / 1.20（峰） | ❌ 未收录 → 记 0 | 同上 |
| 4 | `deepseek-v4-pro` | **缺条目** | 1.32 / 0.044 / 3.96（峰） | ❌ 未收录 → 记 0 | 同上 |
| 5 | `qwen-max` | 1.60 / — / 6.40 | 国际 $1.6 / $6.4 | ✅ 一致 | [Alibaba Model Studio pricing](https://www.alibabacloud.com/help/en/model-studio/model-pricing) |
| 6 | `qwen-plus` | 0.40 / — / 1.20 | 美区 $0.4 / $1.2（Global 档 $0.115/$0.287） | ⚠️ 只对美区成立，未标注地域 | 同上 |
| 7 | `qwen-turbo` | 0.05 / — / 0.20 | 该页未列（已下架/改名？） | ⚠️ 待复核 | 同上 |
| 8 | `claude-opus-4` | 15.00 / 1.50 / 75.00 | 与公开 list price 一致 | ✅（本轮未能抓官方页，见下） | — |

补充说明：
- 官方页可得性：DeepSeek、阿里云官方页**已抓取核对**；`openai.com`/`platform.openai.com`/`developers.openai.com` 返回 **403**，`anthropic.com` 有跨域跳转，**本轮无法用官方页复核 OpenAI/Anthropic**（表中数值与公开 list price 一致，但"是否已过期"这一项在报告里属于**未验证**，建议下轮用可访问的官方镜像补）。
- 用户提到的"内置 25 条"实际是 **24 条**（`modelprice/price.go` 的 `table` 长度），文档如写 25 需订正。
- **价目表无地域/峰谷/批量维度**：`Price` 只有 input/cached/output 三个数。阿里云与 DeepSeek 都是**多档价**（地域、峰谷、batch 50%），单值表必然有偏差。建议在 `Price.Note` 里强制写明"取哪一档"，UI 上把 `TableUpdated` 显示成"价目表更新于 2026-11-17"。

## 4. 逐项结论（对应任务里的 7 个问题）

### 4.1 价格表正确性 / 过时风险 → 见 §2 P1、§3

`Estimated=true` 的 UI 契约：**契约存在但只写在 `go doc` 里**。`costmeter/doc.go` 明确写了"`Estimated` 为 true 时在数字前加 `≈`，`UnpricedModels` 非空时追加'部分模型未计价'"，但：

- `grep -rn "cost_usd\|Estimated" docs/ README.md` → **零命中**：产品文档没有这条契约，UI 实现者看不到；
- `Summary.Models[].ModelUsage` **没有 `Estimated` 字段**（只有 `Priced`），逐行渲染时无法区分"表内估算价"与"已定价但估算"；
- `Summary` 不带 `TableUpdated`，UI 要显示"价目表更新于"必须另外 import `modelprice`。

修法（都是一行）：① 在 `docs/zh/AI-SDK.md` 补一段"金额展示契约"；② `ModelUsage` 加 `Estimated bool \`json:"estimated,omitempty"\``；③ `Summary` 加 `PriceTableUpdated string \`json:"price_table_updated,omitempty"\``。

### 4.2 精度 → **通过**

- 100 万次 1-token 调用（`gpt-4o` / `deepseek-reasoner` / `qwen-turbo`）：总成本与解析解**逐纳元相等**，误差 0（阈值 1e-6 美元）。
- 表内 24 条价目的"每 token 纳美元"全部是整数（`P*1000 ∈ ℤ`），因此逐次 round 到纳元不产生漂移（`TestAuditMeterTablePricesAreNanoExact`）。
- `Snapshot → Restore → JSON → Restore` 三轮往返后 `Snapshot` **DeepEqual**、`Summary` 逐字段相等、`cost_nano_usd` 逐纳元相等。
- **已知边界**（已写成 characterization 用例）：单次成本 < 1 纳元时（例如宿主 `WithPricer` 配置 $0.0001/M 的折扣价），每次 round 到 0，**100 万次累计仍是 0**（丢失 100%）。表内价目不会触发（最便宜的 `qwen-turbo` 也是 50 纳元/token）。

### 4.3 归因 → **通过**

- `RunRequest.Model` 覆盖 `Config.Model` 时，金额记到**实际模型**：`by_model` 出现 `gpt-4o` 与 `gpt-5` 两个键，金额分别为 `$0.00725000` 与 `$0.00602500`（手算一致），总额等于两者之和。
- `models` 保留原始名（`gpt-4o`/`gpt-5`/`gpt-4o-2024-08-06`/`openai/gpt-4o` 各自一行），`Resolved` 正确归并到规范名。
- 未定价模型：成本 0、进 `unpriced_models`（已排序）、`Estimated=true`、requests/tokens 照常累加（不会把用量也吞掉）。
- 补充：kit 的 `setMeter(model)` 在每次 `Run` 前更新模型名，代码路径 `req.Model > Config.Model` 与测试一致。

### 4.4 缓存折扣 → **通过**

- 全命中：`gpt-4o` 1000 cached token = `1250×1000` 纳元（cached 单价），**不是** input 单价。
- 混合：750 input + 250 cached + 100 output 与手算逐纳元相等。
- `cached=0` 折扣的模型（`gpt-4-turbo`）回落到 input 价（宁高不低）。
- 脏数据：`cached > prompt` 被**夹到 prompt**，成本非负且等于"全部按 cached 计价"；负 token 按 0 处理，请求数仍 +1。

### 4.5 预算 → **通过（小预算）+ 溢出缺陷（大预算，见 §2 P2）**

- 79% 不告警 / 80% 告警 / 81% 告警 / 100% `Exceeded()` 为真，`warn` 回调各触发 1 次。
- 并发 100 goroutine × 10 次 `Add`（总额远超预算）：warn 回调**恰好触发 1 次**（`-race` 通过）。
- `Reset()` 后 `Warned()` 归零，再次跨阈值会再触发一次（符合"按会话重置"语义）。

### 4.6 `secrets` 降级后 → **通过**

- `RedactForExport(text, known...)` 与 `New(known...).Redact(text)` 在 8 条样例（`sk-`、`ghp_`、`AKIA`、JWT、`AIza`、`Bearer`、URL userinfo、PEM 私钥块）上**逐字节一致**，凭据全部被替换成 `[REDACTED]`，上下文保留。
- 已知明文：≥8 字节按字面量替换（两处出现两处替换），<8 字节被忽略。
- **幂等**：对已清洗文本再洗一次不再变化（导出流水线重复调用安全）。
- **性能**：1 MiB 文本（含稀疏凭据）`RedactForExport` **25.1 ms**（阈值 50 ms，余量 50%）。注意本包文档已声明"不要放进逐 token 热路径"，导出是低频操作，可接受。
- 零误伤：含 SHA-256 十六进制与 UUID 的正常中文日志**原样返回**。

### 4.7 `events` → **通过**

- **行完整性**：中文、emoji（含 ZWJ 家庭组合）、`\n`/`\r`/`\t`/NUL、引号反斜杠、U+2028/U+2029、HTML 尖括号共 8 组文本 → 每条事件**恰好一行**（物理换行数 == 事件数）、每行 `json.Valid`、解码后文本逐字符相等。
- **前向兼容**：未知 `kind`（`future.thing`）连同任意 payload 被 `Replay` 原样透传；未知 `version`（99）报 `ErrUnsupportedVersion` 且**错误粘性**（后续 `Next` 反复返回同一错误，不会退化成 EOF）。
- **超长原子性**：>1 MiB 的事件被拒绝、**不写任何字节**、不占 `seq`，emitter 之后仍然可用。
- **截断行**：流末尾半行报 `errors.Is(ErrMalformed)`，之前的事件仍可完整回放（不会把半条记录当成合法事件）。
- **吞吐**：10 万事件写入 **452.9 ms（4.5 µs/条，≈2.2 万条/秒）**，解码回放 **227.5 ms（2.3 µs/条，≈44 万条/秒）**。单线程、无缓冲、直连 `io.Writer`；个人客户端的 UI 日志量（万级/会话）完全够用。
- `seq` 严格 1..N 递增，`time` 一律归一化到 UTC。

## 5. 建议的价目表修正（可直接抄）

```go
// 删除/改名：deepseek-chat、deepseek-reasoner 保留为别名（映射到现行价），
// 并新增现行型号。
"deepseek-flash":  {Model: "deepseek-flash", Provider: "deepseek", InputPerMillion: 0.30, CachedInputPerMillion: 0.006, OutputPerMillion: 1.20, Estimated: true, Note: "取峰时价（谷时半价）；官方 2026-11 页", Updated: TableUpdated},
"deepseek-v4-pro": {Model: "deepseek-v4-pro", Provider: "deepseek", InputPerMillion: 1.32, CachedInputPerMillion: 0.044, OutputPerMillion: 3.96, Estimated: true, Note: "取峰时价（谷时半价）；官方 2026-11 页", Updated: TableUpdated},
"o3-mini":         {Model: "o3-mini", Provider: "openai", InputPerMillion: 1.10, CachedInputPerMillion: 0.275, OutputPerMillion: 4.40, Estimated: true, Updated: TableUpdated},
```

另外建议补（本轮未逐条核实价格，但**缺条目=记 0** 的风险已经证实）：

- `claude-haiku-4-5`、`claude-sonnet-4-5`、`claude-opus-4-1`（Anthropic 现行命名）
- `qwen4-max`、`qwen3.8-max`、`qwen3.7-plus`、`qwen-flash`（阿里云现行命名，官方页已抓到价格）
- `gemini-2.5-pro`/`gemini-2.5-flash`、`grok-4`、`kimi-k2`、`glm-4.6`（整族缺失）

19 个候选现行型号里 **13 个未收录**（`TestAuditPriceTableMissingCurrentModels` 会打印清单）。

## 6. 修复优先级

| 级别 | 项 | 修法成本 |
|---|---|---|
| P1 | DeepSeek 价目过时 + 缺现行型号（成本偏小到 0） | 改 `modelprice/price.go` 三行 + 一条守卫测试 |
| P1 | `o3-mini` 前缀错配成 `o3`（高估 1.8×） | 加一条显式条目 |
| P2 | 预算比较 int64 溢出（> $1.15M） | 构造期预算阈值纳元化，~5 行 |
| P2 | `Estimated`/`≈` 契约只在 `go doc`，`docs/` 无记载 | 文档补一段 + `ModelUsage.Estimated` |
| P3 | 表内无地域/峰谷/batch 维度 | 文档声明"取哪一档"+ `Note` 强制填写 |
| P3 | 亚纳元单次成本被 round 掉 | 文档已声明边界；或改累加器为 `int64` 匹美元 |

## 7. 没有发现问题的部分（明确写出来，避免重复审计）

- 纳美元整数累加：100 万次无漂移；表内价目全部"纳元精确"。
- `Snapshot`/`Restore`：逐纳元往返一致、版本不符拒绝、负数拒绝、先校验后替换。
- 归因：`by_model` 归并与 `models` 原始名两个视图一致，明细和等于总额。
- 缓存折扣与脏数据钳制（`cached>prompt`、负 token）都不会产生负金额。
- 并发：100 goroutine 下 warn 只触发一次（`-race`）。
- `events`：行边界、Unicode、未知 kind 透传、未知 version 拒绝、超长原子性、截断行、吞吐。
- `secrets.RedactForExport`：与 `Redactor` 逐字节一致、幂等、1 MiB 25 ms、零误伤。
