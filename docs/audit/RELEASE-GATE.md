# 发布门禁审计（RELEASE-GATE）

- **审计对象**：`/data/zeokeel-rig`，分支 `main`，`HEAD = fbb8e6e`（未提交的工作树：17 个修改 + 23 个新增目录/文件）
- **审计时刻**：2026-11-17 21:29（Asia/Shanghai）；工具链 `go1.27.3 linux/amd64`、`staticcheck 0.8.2`
- **结论**：**可以发布（附条件）** —— 见 §1。审计期间工作树被并发修改（详见 §9 附录），本报告的测试结论以 21:29 快照为准。

> 审计期间发生过一次「红 → 绿」翻转：21:16 时 `kit` 有 5~6 个审计用例红、`tools/mcp` 的 nil-map panic 用例红；21:23–21:26 作者修掉/收口后，21:29 快照全绿。§9 记录了这次翻转，因为它是本次审计最有价值的发现（发布门禁的可信度问题），而不是噪音。

---

## 1. 结论摘要

| 门禁 | 结果 | 判定 |
| --- | --- | --- |
| `go build ./...` | 通过 | ✅ |
| `go vet ./...` | 通过 | ✅ |
| `go test ./... -count=3` | 全绿（无偶发失败） | ✅ |
| `go test -race`（12 个新包） | 12/12 通过 | ✅ |
| `gofmt -l .` | 通过（21:31 复核为空；21:29 时曾有 1 个文件缺尾换行） | ✅ |
| `staticcheck ./...` | **10 条**（3 条在产品代码，7 条在测试） | ⚠️ 阻塞（CI 硬门禁） |
| 交叉编译 windows/darwin/freebsd | 通过 | ✅ |
| 依赖守卫（仅 `mvdan.cc/sh/v3`） | 通过 | ✅ |
| 根包 stdlib-only | 通过 | ✅ |
| `go run ./cmd/rigdoc -check` | 通过 | ✅ |
| 覆盖率地板（总 ≥80%、每包 ≥70%） | 通过 | ✅ |
| 文档计数一致性 | **多处 81 vs 84 漂移** | ⚠️ 建议修 |
| CHANGELOG Unreleased 覆盖 11 项新能力 | 全覆盖 | ✅ |

**一句话**：产品代码与测试在 21:29 快照上是健康的（构建 / vet / 测试 ×3 / 竞态 / 交叉编译 / 覆盖率地板 / 依赖守卫全部通过），**唯一会让 CI 变红的是 10 条 staticcheck**，必须清掉才能打标签；文档计数存在系统性漂移。

---

## 2. 稳定性：`go test ./... -count=3`

命令与结果：

```bash
export PATH=$PATH:/usr/local/go/bin
export GOCACHE=/data/zeokeel-rig/.audit-scratch/gate/gocache
CGO_ENABLED=0 go test ./... -count=3 -timeout 300s
```

| 轮次 | 时刻 | 结果 |
| --- | --- | --- |
| 第 1 轮（21:16） | 工作树在改 | `kit` 5~6 个 `TestAudit*` 红；其余绿 |
| 第 2 轮（21:21） | 工作树在改 | 同上；另外 `tools/search` 曾因并发编辑编译失败 |
| 第 3 轮（21:29，快照） | 稳定 | **全绿**：`kit 0.144s`、`mcpserver 11.653s`、`sandbox/escalate 17.215s`、`internal/filewatch 8.451s` 等 51 个 `ok`，无 `FAIL` |

**关于偶发失败**：快照轮次里没有观察到偶发失败；三轮之间出现的红全部可归因于「工作树正在被编辑」，不是测试脆弱。但两类历史抖动值得记录：

1. `tools/search` 的编译失败是**并发编辑**（`executeText` 返回值签名改到一半），不是竞态。
2. 21:16 的 `kit` 红是**真实缺陷**（见 §9），不是 flaky。

> 结论：**没有发现"测试脆弱"型偶发失败**；`-count=3` 在稳定工作树上干净。

## 3. 竞态：`go test -race`（12 个新包）

```bash
CGO_ENABLED=1 go test -race ./<pkg>/ -count=1 -timeout 300s
```

| 包 | 结果 | 耗时 |
| --- | --- | --- |
| `events` | ok | 4.1s |
| `session/store` | ok | 2.9s |
| `secrets` | ok | 6.8s |
| `instructions` | ok | 1.2s |
| `mcpserver` | ok | 33.6s |
| `tools/patch` | ok | 1.1s |
| `tools/search` | ok | 3.7s |
| `tools/undo` | ok | 1.2s |
| `sandbox/escalate` | ok | 7.3s |
| `internal/filewatch` | ok | 3.9s |
| `kit` | ok | 1.2s |
| `gate` | ok | 1.1s |

**12/12 通过，无 DATA RACE 报告。** 补充观察：21:23 那一轮 `-race` 时 `mcpserver` 曾「ok」而普通模式「红」——那是缺陷的**时序依赖**（慢速调度下 panic 不复现），不是竞态报告的差异，见 §9。

## 4. 静态检查

### 4.1 `gofmt -l .`

21:29 时为：

```
tools/mcp/audit_abort_test.go
```

差异只有**文件末尾缺换行**（`\ No newline at end of file`）。注意 CI 的 gofmt 步骤只扫 **tracked** 文件（`git ls-files -z '*.go'`），所以这个未跟踪文件当时不会红、**一旦 commit 就会红**。21:31 复核时该文件已被作者补上换行，`gofmt -l .` 为空 → **当前通过**。

### 4.2 `go vet ./...`

通过（含 `GOOS=windows` 下对 `sandbox/osrunner/ mcpserver/ secrets/ session/store/` 的 vet）。

### 4.3 `staticcheck ./...`（10 条，CI 硬门禁）

| 位置 | 检查 | 性质 | 级别 |
| --- | --- | --- | --- |
| `agent/redact.go:187` | U1000 `(*run).redactText` 未使用 | 产品代码死代码（包级 `redactText` 仍在用，非脱敏缺口） | P3 |
| `sandbox/command_risk.go:1169` | S1017 可用无条件 `strings.TrimPrefix` | 产品代码风格 | P3 |
| `tools/undo/files.go:352` | U1000 `hashContent` 未使用 | 产品代码死代码（与 `tools/undo` 快照比较路径疑似有重复实现） | P3 |
| `instructions/instructions_test.go:849` | U1000 `ensureError` 未使用 | 测试 | P3 |
| `mcpserver/audit_protocol_test.go:338` | U1000 `auditError` 未使用 | 测试 | P3 |
| `mcpserver/audit_protocol_test.go:348` | U1000 `fmtMethod` 未使用 | 测试 | P3 |
| `tools/undo/audit_undo_test.go:413` | U1000 `mustChanges` 未使用 | 测试 | P3 |
| `kit/audit_wiring_internal_test.go:25` | ST1012 `leadAuditErr` 命名 | 测试 | P3 |
| `sandbox/escalate/audit_limits_test.go:309` | SA1012 传 nil context | 测试 | P3 |
| `session/store/audit_store_test.go:554` | SA4006 `first` 赋值未使用 | 测试 | P3 |

**没有一条是真 bug**，但 `.github/workflows/ci.yml` 的 `staticcheck ./...` 步骤没有 `continue-on-error`，**这 10 条会让 CI 变红** → 发布阻塞（清理即可，都是删死代码/改命名）。

## 5. 交叉编译

```bash
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
GOOS=darwin  GOARCH=arm64 CGO_ENABLED=0 go build ./...
GOOS=freebsd GOARCH=amd64 CGO_ENABLED=0 go build ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go vet ./sandbox/osrunner/ ./mcpserver/ ./secrets/ ./session/store/
```

全部通过（3/3 构建、windows vet 无输出）。

## 6. 约束守卫

```bash
go list -deps ./... | grep -v '^github.com/zeokeel/zeokeel-rig' | grep -E '\.'
```

输出只有：

- `mvdan.cc/sh/v3/{syntax,fileutil}` —— 允许的唯一第三方依赖；
- `crypto/internal/entropy/v1.0.0` —— 标准库内部包（路径里带点）；
- `vendor/golang.org/x/{crypto,net,sys,text}/…` —— **Go 标准库自带的 vendored 依赖**（`net/http`、`crypto/tls` 等内部使用），不是本模块的依赖。

`go.mod` 仍然只有一条 `require mvdan.cc/sh/v3 v3.14.1`，`go.sum` 无新增。根包：

```bash
go list -deps .   # 只有 github.com/zeokeel/zeokeel-rig + 无点号的标准库包
```

**结论：无新增第三方依赖，根包保持 stdlib-only。**

## 7. 文档一致性

| 检查 | 结果 |
| --- | --- |
| `go run ./cmd/rigdoc -check` | 通过（exit 0，AI 目录与代码一致） |
| 每个新包有包文档 | 通过：`go doc ./events`、`./secrets`、`./mcpserver`、`./instructions`、`./session/store`、`./tools/patch`、`./tools/search`、`./tools/undo`、`./sandbox/escalate`、`./internal/filewatch` 全部可读中文包文档（`mcpserver`、`session/store` 的包注释写在非 `doc.go` 文件里，`go doc` 正常） |
| CHANGELOG Unreleased 覆盖 11 项新能力 | 通过：命令级风险、events、session/store、tools/patch、instructions+environment、secrets、tools/search、sandbox/escalate、tools/undo、mcpserver、internal/filewatch 全部有条目 |
| **工具计数一致性** | **不通过**，见下 |

实际计数（程序计算，非文档抄写）：

```
full.All                       = 84
full.All + SandboxGrants       = 86
preset minimal / standard / all = 8 / 73 / 84
```

漂移清单（应统一为 84 / 86，preset 为 8 / 73 / 84）：

| 文件:行 | 现值 | 应为 |
| --- | --- | --- |
| `docs/zh/TOOLS.md:9` | 81 | 84 |
| `docs/COVERAGE.md:27` | "81 tools" | 84 |
| `docs/COVERAGE.md:81` | `static tools=81` | 84 |
| `docs/zh/COVERAGE.md:44` | `static tools=81` | 84 |
| `docs/zh/README.md:11` | 81 | 84 |
| `docs/zh/README.md:30` | `81 static tools` | 84 |
| `docs/zh/README.md:32` | `static tools=81` | 84 |
| `docs/CONFIG.md:302` | `standard (70) or all (81)` | `standard (73) or all (84)` |

（`README.en.md:418`、`docs/AUDIT-REPORT.md:8` 里的 81 属于**历史发布说明/历史审计报告**，可保留；`docs/TOOLS.md:14`、`docs/COVERAGE.md:22`、`docs/zh/COVERAGE.md:19,24`、`docs/zh/AI-SDK.md:26`、`AGENTS.md:23` 已是 84/86，正确。）

## 8. 覆盖率

CI 的真实门禁是 `scripts/coverage-floor.sh`（总 ≥80%、每个有测试的包 ≥70% 函数覆盖）。实跑结果：

```
total statement coverage: 84.4%
coverage: every tested package is at or above the 70% per-package floor
```

即 **CI 覆盖率门禁通过**。新包的语句覆盖率：

| 新包 | 语句覆盖 |
| --- | --- |
| `events` | 96.4% |
| `tools/search` | 92.6% |
| `internal/filewatch` | 91.5% |
| `secrets` | 90.9% |
| `sandbox/escalate` | 90.4% |
| `instructions` | 89.9% |
| `mcpserver` | 87.4% |
| `tools/patch` | 85.3% |
| `tools/undo` | 81.6% |
| `session/store` | 79.6% |
| `kit` | 92.2% |
| `gate` | 93.4% |

全仓最低 5 个（**都是有测试的既有包**，非新包）：`tools/full` 76.6%、`tools/mcp` 78.7%、`tools/terminal` 79.2%、`session/store` 79.6%、`tools/jobs` 81.3%。**新包里最低的是 `session/store` 79.6%**。

**没有新包低于 70%。** 0% 的包都是"按设计不测"：`cmd/rigdoc`、`cmd/rigdoctor`、`cmd/rigmcp`、`examples/*`、`tools/catalog`（仅 init 注册）、`tools/internal/background`（由 jobs 间接覆盖）。

## 9. 附录：审计期间的工作树翻转（最有价值的发现）

审计开始时（21:16）整仓是**红的**，21:29 变绿。这不是噪音，它暴露了一个流程问题。

| 时刻 | 事件 | 证据 |
| --- | --- | --- |
| 21:16 | `mcpserver` `TestAuditFinding_ClientPanicsAfterTransportAbort` **连续 3 轮红** | `client panicked after the transport aborted: assignment to entry in nil map (tools/mcp/stdio_transport.go:103)` |
| 21:17 | `tools/search` 编译失败（并发编辑中） | `multiple-value t.executeText(...) in single-value context` |
| 21:17:57 | 该用例被改成 `t.Skipf("KNOWN DEFECT …")` | `-v` 输出：`SKIP … a request after transport abort panics with assignment to entry in nil map instead of returning ErrClosed` |
| 21:18 | `kit` 出现 6 个红色审计用例 | `TestAuditAssembleSystemPromptZeroEnvironment`（零值 Environment 渲染 `date: 0001-01-01`）、`TestAuditKitNilToolElementWithRedactor`（nil 工具元素未报错）、`TestAuditSessionStore*`（跨实例覆盖/并发 `session not found`）等 |
| 21:25–21:26 | `kit/wiring.go` 修改，多数缺陷修复；1 条改成 `t.Skipf("已知残余边界…")`（部分填充的 Environment 渲染零值日期） | `kit/audit_wiring_internal_test.go:282` |
| 21:29 | 全绿快照 | §2/§3 |

**残留风险（必须在发布说明里显式记录）**：

1. `tools/mcp/stdio_transport.go:103` 的 **nil-map panic 仍未修**：`abort()` 把 `t.pending = nil` 但不设 `t.closed`，`Request()` 在锁内只查 `t.closed` 就写 `t.pending[msg.ID]`。修复是一行（锁内加 `t.pending == nil` 检查，或在 abort 里 `closed.Store(true)`）。现在被 `t.Skip` 掩盖 —— **CI 绿不代表这个缺陷不存在**。
2. `instructions` 在 Environment 部分填充时会渲染 `date: 0001-01-01` 与空 `workspace/cwd/temp`（会把模型带偏并污染 prompt 缓存），现在是 `t.Skip` 记录的已知边界。
3. `session/store` 的跨实例写（两个 `kit.New` 指向同一目录）曾有丢数据用例；21:26 后已转绿，但 `session/store` 包文档明确"**不提供跨进程互斥**"（`session/store/store.go:40`），多进程共写同一目录仍是已知限制。

**建议的门禁纪律**：`t.Skip` 只允许用于"环境不允许"（如 Windows 权限），"已知产品缺陷"必须改成一个**带 issue 编号的 xfail 机制**（或干脆红着），否则发布门禁会长期失去意义。

## 10. 可疑模式扫描

| 模式 | 结果 |
| --- | --- |
| `io/ioutil` | 0 处（仓库代码内） |
| `panic(` 非测试 | 2 处，均为"构造期不可能失败"：`internal/rigdoc/rigdoc.go:62`（marshal catalog）、`tools/registry.go:30`（NewRegistry 初始化） |
| `TODO`/`FIXME` | 2 处，均为**有意的语义说明**：`instructions/doc.go:19`、`instructions/instructions.go:340`（`TODO(symlink-cwd)` 解释为什么不解析链接） |
| `fmt.Print*` 库代码 | 1 处，属 helper 协议输出：`sandbox/osrunner/linux_landlock.go:365`（Landlock 启动器向宿主报告 ABI）。其余命中都在 `cmd/`、`examples/`、注释里 |
| `exec.Command(` | 只在允许清单内：`sandbox/osrunner/*`、`sandbox/escalate/broker.go`、`tools/builtin/*`，以及**既有的** `tools/{jobs,lsp,mcp,ptc,terminal,worktree}`。11 个新包中只有 `sandbox/escalate` 需要 exec（设计如此），其余全部干净 |
| `os.WriteFile`/`os.Create`/`os.Rename` | 新包里集中在 `tools/patch`、`tools/undo`、`session/store`。`tools/patch` 走 `policy.ReadOnly()` + `policy.CheckPath()` + `ProtectedPaths()`（`tools/patch/apply.go:856-863`），但落盘用 `filepath` 拼绝对路径 + `os.Rename` 而非 `os.Root`（`tools/patch/doc.go:36` 已明文记录该取舍）；`session/store` 只写自己的目录，不经策略（合理）。 |
| `_ =`（丢弃错误） | 新包共 25 处，人工判断：多数是 `Close`/`Remove` 清理与 best-effort 回滚（可接受）。值得知道的两处：`tools/patch/apply.go:689` 回滚时 `_ = replaceFile(...)`（回滚失败会被吞）；`mcpserver/server.go:442` 最终响应 `_ = s.writeLine(payload)`（写失败时客户端只能等超时）。均非阻塞。 |

---

## 11. 阻塞项 / 建议项

### 必须修（阻塞发布）

1. 清掉 10 条 staticcheck（删 3 处死代码：`agent/redact.go:187`、`tools/undo/files.go:352`、测试里的 `ensureError`/`auditError`/`fmtMethod`/`mustChanges`；改 `kit/audit_wiring_internal_test.go:25` 命名；`sandbox/command_risk.go:1169` 改无条件 `TrimPrefix`；两处测试的 nil ctx / 未用赋值）。
2. 决定 `tools/mcp/stdio_transport.go:103` 的 nil-map panic：要么修（一行），要么在 `CHANGELOG` 的 Known Issues 里写清并给出绕过方式——**不能只靠 `t.Skip` 静默**。

### 建议修（非阻塞）

3. 统一工具计数：`84 / 86`，preset 为 `8 / 73 / 84`（§7 表格）。
4. 把 `session/store` 的"不提供跨进程互斥"从包注释提升为**面向用户的文档**（`docs/CONFIG.md` 或 `SessionStoreDir` 的说明里），避免宿主在同一目录上跑两个进程。
5. `instructions` 的零值 `date` 边界：`Time` 为零时省略 `date` 行（一条判断）。
6. `tools/patch` 的 `os.Rename` 路径：若要防 TOCTOU，后续可迁到 `os.Root`（doc.go 已记录现状）。