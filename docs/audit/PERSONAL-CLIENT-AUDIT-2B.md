# 个人客户端可用性审计 · 2B（本地模型接入 / diff 预览 / 摘要文案）

- **审计对象**：`provider/local`、`provider/ollama`、`provider/lmstudio`、`internal/diffpreview`、`tools/builtin/diffsummary.go`（**全部非审计者所写**）
- **审计者角色**：只读实现，**只新增 `audit_*_test.go`**，不改任何产品代码、不 commit
- **时刻**：2026-11-18 01:20–01:40（Asia/Shanghai），`go1.27.3 linux/amd64`
- **视角**：**"个人用户真的能用它吗"**（不是安全）
- **新增用例**：73 个（`provider/local` 15、`provider/ollama` 13、`provider/lmstudio` 5、`internal/diffpreview` 15、`tools/builtin` 25），全部 PASS，含 `-race`

## 0. 结论摘要

本地模型接入的**协议兼容性非常好**：ollama / LM Studio 的六种真实形态（message 尾帧、对象型 `arguments`、缺 `index`、无 `[DONE]`、`usage` 缺 `total_tokens`、CRLF + 注释行）都能正确解析，且**装配结果与官方 `provider/openai` 逐字段一致**；连不上时的提示文案质量很高；空模型列表没有被误判为错误；`diffpreview` 经 500 轮（另加 5000 轮压测）随机属性测试**没有发现一处 diff 错误**。

但存在 **3 个会真实伤害个人用户的问题**：

| # | 问题 | 严重度 |
| --- | --- | --- |
| 1 | 传了 `Config.HTTPClient` 时 **`HeaderTimeout` 被静默忽略** → 服务端不回响应头就无限挂起 | **P1** |
| 2 | 同一事件里多个 `tool_calls` 都缺 `index` 时**塌成一个坏调用**（参数被拼接、名字被覆盖） | **P1** |
| 3 | 1 MiB 文件每次 `write_file` 摘要花 **15–22ms**（预算 10ms），根因是 `splitLines` 逐行分配 string | **P2** |

---

## 1. 本地模型接入：真实形态 × 与 openai 的一致性

方法：httptest 起 server 回放**真实 ollama / LM Studio / 代理**会发出的 SSE；用与 `agent.runStream` 相同的装配逻辑（按 `delta.Index` 分组、参数拼接）得到"装配结果"；再让 `provider/openai` 吃**等价的规范流**，逐字段比对文本 / 推理 / 工具调用 / `finish_reason`。

| 断言 | 方法 | 结果 | 结论 | 最小修法 |
| --- | --- | --- | --- | --- |
| ollama 最后一帧给整条 `message`（而非 delta） | local 只发一帧 `{"choices":[{"message":{...},"finish_reason":"stop"}]}` 且无 `[DONE]`；openai 吃等价 delta 流 | ✅ 两者都得到 `"hello world"`，`finish=stop` | `delta.isEmpty()` 时回退 `choice.Message` 的逻辑正确 | 无需修 |
| `tool_calls[].function.arguments` 是 **JSON 对象** | local 发对象 `{"path":"a.txt","limit":10}`；openai 发字符串 | ✅ 对象被 `json.Compact` 归一，两者工具调用**逐字节相同** | 归一化正确，且保持了服务端给的键序 | 无需修 |
| `index` 缺失（单次调用） | local 发无 `index` 的单个 `tool_calls`；openai 发 `index:0` | ✅ 等价于 index 0 | 单调用场景不会坏 | 无需修 |
| 没有 `[DONE]`、只有 `finish_reason` | local 无 DONE；openai 有 DONE | ✅ 文本/工具/结束原因一致 | 默认 `RequireDone=false` 的选择对本地服务是对的 | 无需修 |
| `usage` 缺 `total_tokens` | 两侧都只给 prompt/completion | ✅ local 推导 `total=18`；⚠️ openai 保留 `0` | **local 更正确**；两 provider 行为不一致（见 §7-F5） | 把 local 的推导逻辑搬进 openai，或明确文档化差异 |
| `cached_tokens` 两种拼写 | 嵌套 `prompt_tokens_details.cached_tokens` 与扁平 `prompt_cache_hit_tokens` | ✅ 两种都落到 `rig.Usage.CachedTokens=64` | 覆盖了 DeepSeek/网关两种写法 | 无需修 |
| 代理注入的**注释行 + CRLF** | `: keep-alive` 与 `\r\n` 行尾 | ✅ 与纯 LF 结果完全一致 | `sseData` 的 `:` 前缀跳过与 `\r` trim 正确 | 无需修 |
| `finish_reason` 本地变体 | `tool_call` / `max_tokens` / `eos` / `stop` | ✅ 归一成 `tool_calls` / `length` / `stop` | 别名表有效 | 无需修 |
| **多个** `tool_calls` 且都不带 `index` | 一个事件里两个调用，均无 `index` | ❌ **塌成一个**：`call_b/glob({"path":"a"}{"pattern":"*.go"})` | **P1 缺陷**：`Index int` 默认 0 → 两个调用都进 `accumulators[0]`，名字被后者覆盖、参数被拼接成非法 JSON | 见 §7-F2 |
| ollama 端到端（走 `/v1/chat/completions`） | 混合 message 尾帧 + 对象 arguments + `finish_reason:"tool_calls"`，无 `[DONE]` | ✅ 文本 `"让我看看这个文件。"`、`read_file({"path":"main.go"})`、`finish=tool_calls`；且**默认不发 `Authorization`** | 厂商薄封装没有破坏基座语义 | 无需修 |

> 说明：`index` 缺失的**多调用**场景，`provider/openai` 与 `provider/local` 有同样的缺陷（`Index` 都是 `int` 零值），所以这是**共用设计缺陷**而不是本地适配问题；ollama / LM Studio 自己都会带 `index`，但部分 OpenAI 兼容网关不带——个人用户换网关就会撞上。

## 2. 错误可读性

| 断言 | 方法 | 结果 | 结论 | 最小修法 |
| --- | --- | --- | --- | --- |
| 连不上 127.0.0.1 时告诉用户"先启动 ollama" | 关闭端口后 `Stream` | ✅ 逐字：`ollama: request failed: Post "http://127.0.0.1:38425/v1/chat/completions": dial tcp …: connect: connection refused: ollama 未启动？先运行 \`ollama serve\`（或设置 OLLAMA_HOST 指向已有服务）` | 前缀=`ollama:`，含 `ollama serve` 与 `未启动`，**可直接照做** | 无需修 |
| LM Studio 的同类提示 | 同上 | ✅ `lmstudio: …: LM Studio 未启动？打开 LM Studio → Developer → 开启 Local Server（默认端口 1234）` | 指向 GUI 面板，正确 | 无需修 |
| `Models` 连不上也有提示 | 关闭端口后 `Models` | ✅ 含 `ollama serve` | 原生 `/api/tags` 与兼容层共用 Transport | 无需修 |
| 401 / 403 / 500 是否区分 | 三种状态码各一次 | ⚠️ 只靠状态码文本：`local: request failed: 401 Unauthorized: {"error":"unauthorized"}` | **能区分**（状态码+服务端 body 都在），但**没有"该怎么办"**：401/403 不提示"检查 APIKey / 反代鉴权"，500 不提示"可重试/看日志" | 在 `statusError` 里按码追加一句：401/403 → “检查 `Config.APIKey` 或反代鉴权”；5xx → “服务端错误，可重试；若持续，看 ollama/lmstudio 日志” |
| `Models` 返回空列表 | `{"models":[]}` / `{"data":[]}` | ✅ 返回空切片、err=nil | 空列表**不是**错误（没装/没加载模型是常见状态） | 无需修 |
| `/api/tags` 404 可诊断性 | 404 响应 | ⚠️ `ollama: request failed: 404 Not Found: 404 page not found` | **没有请求路径**，用户无法判断是 ollama 版本太老还是反代把路径改了 | `statusError` 带上 method+URL（`GET http://…/api/tags`） |
| 401（LM Studio 套鉴权反代） | 401 + `{"error":{"message":"invalid api key"}}` | ⚠️ 同上，无 `APIKey` 提示 | 与 ollama 同因 | 同上一条修法可一并覆盖 |

## 3. 超时：会不会"静默挂死"

| 断言 | 方法 | 结果 | 结论 | 最小修法 |
| --- | --- | --- | --- | --- |
| `DefaultHeaderTimeout` = 5 分钟（ollama/lmstudio） | 读常量 | ✅ `5 * time.Minute`（`provider/local` 是 60s） | 对"冷启动加载权重"是合理默认 | 无需修 |
| 只回响应头、不回 body | server 写头后阻塞；ctx 150ms | ✅ **152.6ms** 返回 `local: request canceled: context deadline exceeded`（`errors.Is` 可判） | ctx 取消能可靠打断，**不会**卡在 `Recv` | 无需修 |
| 连响应头都不回（默认 client） | `HeaderTimeout=120ms` + 默认 client | ✅ **120.8ms** 返回 `…: net/http: timeout awaiting response headers` | 机制本身是正确的 | 无需修 |
| **连响应头都不回（自定义 `HTTPClient`）** | `HeaderTimeout=50ms` + 传了 `cfg.HTTPClient` | ❌ 50ms 没生效，一直等到 ctx 的 **400ms** | **P1 缺陷**：`local.New` 只在 `cfg.HTTPClient == nil` 时设置 `ResponseHeaderTimeout`，**传了自定义 client 就静默失去 5 分钟兜底**——宿主通常也不设 ctx，于是表现为无限转圈 | 见 §7-F1 |
| 等待期没有任何进度信号 | 代码走查 | ⚠️ 头未到时 UI 只能显示"转圈"，最长 5 分钟 | 冷启动确实慢（加载 7B 权重几十秒），但**用户不知道是在加载还是挂了** | 建议宿主在 Stream 前显示"正在加载模型…"；或在 `Transport` 里提供 onWait 回调（非必须） |

## 4. `internal/diffpreview` 正确性（属性测试）

方法：**独立实现**一个统一 diff 应用器（`applyUnified`，只解析 diff 文本 + 旧内容，不复用 diffpreview 任何代码），随机生成 old/new，要求 `applyUnified(old, Preview(old,new)) == new` 逐字节成立。

| 断言 | 方法 | 结果 | 结论 | 最小修法 |
| --- | --- | --- | --- | --- |
| 500 轮随机 diff 可应用回新内容 | `rand(0x5EED)`，行池含空行/制表/中文/伪装成 diff 的文本；随机插/删/改、随机整篇替换、`\n` 与 `\r\n` 混用、30% 概率去掉尾换行 | ✅ 500/500 通过 | diff 是**可执行**的，不是"看起来像 diff" | 无需修 |
| 加大到 5000 轮 | `AUDIT_DIFF_ROUNDS=5000` | ✅ 通过（0.23s） | 稳定性足够，可进 CI | 建议在 CI 里带 `AUDIT_DIFF_ROUNDS=1000` |
| `Stats` 与 diff 里的 `+`/`-` 行数一致 | 独立统计 diff 正文 | ✅ 500/500 一致 | `Summary` 的 `+N/-M` 有据 | 无需修 |
| 行数自洽：`LineCount(new) = LineCount(old) - removed + added` | 同上 | ✅ 全通过 | 摘要里的"3 lines → 4 lines"不会自相矛盾 | 无需修 |
| 空文件 / 无尾换行 / CRLF / 超长单行（100 KiB）/ 纯空行文件 | 10 个定点边界用例 | ✅ 全通过（含 `\ No newline at end of file` 的精确位置） | 最容易出错的一类边界是干净的 | 无需修 |
| 二进制（含 NUL）与超 `MaxBytes` | NUL 内容 + `MaxBytes+1` | ✅ 降级为 `binary file changed (N bytes → M bytes)`，不含 `@@`，`Stats` 返回 0/0 | 与 git 的 `Binary files differ` 口径一致，审批不会卡 | 无需修 |
| 确定性 | 同一输入跑 50 次 | ✅ 逐字节相同 | golden 快照前提成立 | 无需修 |

## 5. `diffsummary` 文案（write_file / edit_file 的一行摘要）

| 场景 | 实际输出 | 结论 | 最小修法 |
| --- | --- | --- | --- |
| 新建 | `write_file: src/a.txt (created, 3 lines)` | ✅ 清楚 | 无需修 |
| 新建空文件 | `write_file: empty.txt (created, 0 lines)` | ✅ 不出现断尾 | 无需修 |
| 覆盖且有变化 | `write_file: src/a.txt (3 lines → 4 lines, +2 -1)` | ✅ 恰好回答了"我改了多少" | 无需修 |
| **内容相同（0 变化）** | `write_file: src/a.txt (3 lines → 3 lines, **+0 -0**)` | ⚠️ **字面正确但信号弱**：模型看到 `+0 -0` 需要自己推理"等于没改"；而这次 `write_file` 事实上把文件重写了一遍（可能改了 mtime、丢了权限位） | 改成 `(unchanged, 3 lines)`——明确、且省去模型的推理 |
| 二进制 | `write_file: bin.dat (3 bytes → 3 bytes)` | ✅ 不报行数 | 无需修 |
| 旧内容未读入（文件过大） | `write_file: big.json (5242880 bytes → 3 bytes)` | ✅ 不会假装知道行数 | 无需修 |
| `replace_all`（3 次） | `edit_file: src/a.txt (replace 3 occurrences, +3 -3)` | ✅ 复数形态正确 | 无需修 |
| 单次替换 | `edit_file: src/a.txt (replace 1 occurrence, +1 -1)` | ✅ `occurrence` 单数正确 | 无需修 |
| 二进制上的 edit | `edit_file: bin.dat (replace 1 occurrence, 3 bytes → 3 bytes)` | ✅ 保留了替换次数 | 无需修 |

## 6. 性能：1 MiB 文件上的摘要开销

方法：`write_file` 的摘要走 `lineStats` → `diffpreview.Stats` → Myers。测 200 次取单次均值；另做尺寸曲线。

| 文件规模 | 单次耗时 | 结论 |
| --- | --- | --- |
| 64 KiB（1 149 行） | 1.13 ms | 可接受 |
| 256 KiB（4 598 行） | 3.75 ms | 可接受 |
| 512 KiB（9 197 行） | 8.29 ms | 接近预算 |
| **1 MiB（18 393 行）** | **14.66 ms**（另一次测得 16.4 ms） | ❌ **超 10ms 预算** |
| 1 MiB 且**整篇重写** | 21.7 ms | ❌ 超得更多（退化路径也要先跑满 `MaxEditDistance=512`） |

CPU profile（`-cpuprofile`，改一行场景）：

```
1310ms 34.75%  splitLines            ← 逐行 string(content[start:index]) 分配
 650ms 17.24%  slicebytetostring
 450ms 11.94%  mallocgcSmallNoScanSC6
```

即 **`splitLines` 一项占 56% 累计**：一次摘要要对 old/new 各切一次行（`Stats`）、再加两次 `LineCount`，1 MiB × 4 次 = 7 万次字符串分配。

最小修法（三选一，按性价比排序）：
1. **`diffLine.text` 从 `string` 改成 `[]byte` 子切片**（零分配，`bytes.Equal` 比较）——改动约 10 行，预计 1 MiB 降到 3–5ms；
2. `writeFileSummary` 里先 `bytes.Equal(old, content)` 短路 → 顺手把 §5 的 `+0 -0` 一起修掉（但只救"没变"这一种）；
3. 把 `comparable()` 里"逐行统计"的阈值从 `MaxBytes`(1 MiB) 降到 256 KiB——一句话修完，代价是 >256 KiB 的文件只报字节数。

## 7. 缺陷清单（按严重度）与最小修法

### F1（P1）自定义 `HTTPClient` 让 `HeaderTimeout` 静默失效

- **位置**：`provider/local/local.go` 的 client 构造（`client := cfg.HTTPClient; if client == nil { …ResponseHeaderTimeout = headerTimeout }`）
- **复现**：`provider/ollama/audit_personal_test.go: TestAuditFinding_HeaderTimeoutIgnoredWithCustomClient`——`HeaderTimeout=50ms` + 传自定义 client + 服务端不回响应头，实测等到 ctx 的 **400ms** 才返回，50ms 完全没生效。
- **影响**：宿主为了代理/TLS/指标注入自己的 `http.Client` 时（很常见），本地模型服务一旦"起但不响应"，客户端**无限等待**（宿主一般不设 ctx 超时），用户看到永久转圈、只能杀进程。文档里写了"只影响本包构造的默认 client"，但**没有任何运行时提示**，属于静默降级。
- **最小修法**（3 行）：不论 client 来源，都保证 header 超时存在——
  ```go
  if client == nil { …现有逻辑… } else if client.Transport == nil || hasNoHeaderTimeout(client.Transport) {
      // 克隆一份并补 ResponseHeaderTimeout，或返回一个包装 Transport
  }
  ```
  退一步：在 `New` 时若 `cfg.HTTPClient != nil && cfg.HeaderTimeout > 0`，返回一个明确错误或 `slog` 告警，别静默忽略。

### F2（P1）多个 `tool_calls` 缺 `index` 会塌成一个坏调用

- **位置**：`provider/local/wire.go` `streamToolCall.Index int`（零值 0）+ `provider/local/stream.go` `handleEvent`；`provider/openai` 同构。
- **复现**：`provider/local/audit_stream_parity_test.go: TestAuditFinding_TwoToolCallsWithoutIndexCollapse`，实测装配结果 `call_b/glob({"path":"a"}{"pattern":"*.go"})`——一个调用、名字被后者覆盖、参数是非法 JSON。
- **影响**：模型本来要并行读两个文件，结果得到一个语法非法的调用 → 工具报"参数解析失败" → 模型重试或放弃，用户看到"模型突然变傻"。ollama/LM Studio 自己带 `index`，但**大量 OpenAI 兼容网关不带**，正好落在"换模型就坏"的判据上。
- **最小修法**：在同一事件内按出现顺序补 `index`（仅当缺失/重复时）：
  ```go
  for i, call := range delta.ToolCalls {
      if len(delta.ToolCalls) > 1 && call.IndexSeen == false { call.Index = i }
  }
  ```
  实现上给 `streamToolCall` 加 `HasIndex bool`（`json:"index"` 无法区分 0 与缺失，可用 `*int` 或自定义 Unmarshal）。

### F3（P2）1 MiB 文件的写摘要超预算（14.7–21.7ms/次）

见 §6。最小修法：`splitLines` 改零分配（首选），或先 `bytes.Equal` 短路 + 把统计阈值降到 256 KiB。

### F4（P2）404 错误里没有请求路径

- **位置**：`provider/local/local.go: statusError`（只拼 `resp.Status` 与 body）。
- **影响**：`ollama: request failed: 404 Not Found: 404 page not found` 无法定位是 `/api/tags` 还是反代改写；个人用户排查"模型列表为空"时会卡住。
- **最小修法**：`statusError` 增加 `method + url`（已有 `resp.Request`）：`ollama: GET http://127.0.0.1:11434/api/tags failed: 404 Not Found: …`。

### F5（P3）401/403 没有"该怎么办"

- **最小修法**：`statusError` 按状态码追加一句可执行提示（401/403 → 检查 `Config.APIKey` 或反代鉴权；5xx → 服务端错误，可重试、看服务日志）。

### F6（P3）"内容相同"的摘要输出 `+0 -0`

- **最小修法**：`writeFileSummary` 里 `if bytes.Equal(old, content) { return fmt.Sprintf("%s: %s (unchanged, %s)", ToolWriteFile, relative, lineCountWords(content)) }`。

### F7（P3）`total_tokens` 推导只在 local 侧

- **影响**：`provider/openai` 拿到缺 `total_tokens` 的服务端响应时 `Usage.TotalTokens=0`，而 `provider/local` 会补成 `prompt+completion`。同一个 SDK 两条路径给出不同用量，成本/上下文统计会不一致（对刚加的 `costmeter` 尤其明显）。
- **最小修法**：把 `local/stream.go` 的三行推导搬进 `openai/stream.go` 的 `usageFromWire`。

## 8. 新增审计用例清单

| 文件 | 用例数 | 覆盖 |
| --- | --- | --- |
| `provider/local/audit_stream_parity_test.go` | 15 | 六种真实形态 + 与 openai 的逐字段一致性 + `finish_reason` 别名 + 2 个 finding |
| `provider/ollama/audit_personal_test.go` | 13 | 连不上提示（逐字）、401/403/500、空模型列表、404 可诊断性、5 分钟默认值、3 个超时行为、1 个端到端形态 |
| `provider/lmstudio/audit_personal_test.go` | 5 | 默认地址与提示、空模型列表、401 文案、默认不发 Authorization、`Endpoint()` 与实际路径一致 |
| `internal/diffpreview/audit_property_test.go` | 15 | 500 轮（可压到 5000）随机属性 + 10 个边界 + 二进制/超限降级 + Summary 口径 + 确定性 |
| `tools/builtin/audit_diffsummary_test.go` | 25 | 新建/覆盖/相同/二进制/旧内容未知/replace_all 文案 + 尺寸曲线 + 1 MiB×200 耗时 |

全部用例在 `CGO_ENABLED=0` 与 `-race` 下通过；**审计文件不修改任何产品代码**。

## 9. 复现

```bash
export PATH=$PATH:/usr/local/go/bin
export GOCACHE=/data/zeokeel-rig/.audit-scratch/aud3b/gocache

CGO_ENABLED=0 go test ./provider/local ./provider/ollama ./provider/lmstudio \
    ./internal/diffpreview ./tools/builtin -run TestAudit -count=1 -v

# 更大规模的 diff 属性测试
AUDIT_DIFF_ROUNDS=5000 CGO_ENABLED=0 go test ./internal/diffpreview \
    -run TestAuditProperty -count=1

# 性能曲线与 profile
CGO_ENABLED=0 go test ./tools/builtin -run TestAuditPerformance -v
CGO_ENABLED=0 go test ./tools/builtin -run TestAuditPerformance -cpuprofile=/tmp/p.prof
go tool pprof -top -nodecount=10 /tmp/p.prof
```

## 10. 审计过程中的两个自我更正（记录用）

1. `applyUnified` 第一版只从 diff 正文重建新内容 → 被属性测试打回：**unified diff 的 hunk 之外没有上下文**，必须结合旧内容才能重建。这本身就是"diff 必须可执行"的证据。
2. 第二版把 `\ No newline at end of file` 当成"对输出流的修正"→ 错：标记是**前一行的属性**，跟在 `-` 行上时只说明旧文件缺尾换行。改成前视后 500/500 通过。

> 这两次都是**审计器自己的 bug**，不是产品缺陷——写在这里是为了说明：属性测试的价值取决于应用器是否独立且正确，否则会得到假阳性/假阴性。