# 贡献指南

本文说明怎么在本地把仓库跑起来、哪些规则由 CI 强制、加一个工具要改哪些文件、提交与 PR 长什么样。读者是准备提交代码或文档改动的贡献者；只想用 SDK 的人应该看 [README.md](README.md) 与 [docs/CONFIG.md](docs/CONFIG.md)。

参与本项目即表示同意遵守 [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)。**安全漏洞不要开公开 issue 或 PR**，按 [SECURITY.md](SECURITY.md) 走私密渠道。

## 环境与前置

- **Go 1.27 或更高**。`go.mod` 声明 `go 1.27`，CI 用 `actions/setup-go` 的 `go-version-file: go.mod` 读取它。
- **`CGO_ENABLED=0`**。库与全部测试构建都不含 cgo；`Makefile` 里 `export CGO_ENABLED`（默认 `0`），`make race` 是唯一例外（race 检测器本身需要 cgo 工具链，它内部用 `CGO_ENABLED=1`）。
- `git`；`awk`/`diff`（可选的 README quickstart 漂移检查要用）；`make` 是可选的便利入口，所有目标都能用对应的 `go` 命令替代。
- 本地**不需要**安装 staticcheck 或 govulncheck：CI 的 `lint` job 现场 `go install` 固定版本（staticcheck v0.8.2、govulncheck v1.9.0）。
- 确认工具链可用：

  ```sh
  export CGO_ENABLED=0
  go version          # go1.27 或更高
  go env GOCACHE      # 沙箱/只读 HOME 环境下建议显式设置可写目录，例如 GOCACHE=/tmp/gocache
  ```

## 本地开发

```sh
git clone https://github.com/zeokeel/zeokeel-rig.git
cd zeokeel-rig
make build          # 等价于 go build ./...
make test           # 等价于 go test ./... -count=1
```

仓库没有生成文件、没有 vendoring、也没有代码生成步骤——除了 `docs/ai/` 是由 `cmd/rigdoc` 从真实构造器生成（改了工具面必须重新生成，见下）。

### Makefile 目标

`make` 的默认目标是 `ci`。

| 目标 | 命令要点 | 检查什么 |
| --- | --- | --- |
| `make help` | 列出目标 | — |
| `make fmt` | `git ls-files -z '*.go' \| xargs -0 gofmt -l` | 所有已跟踪 Go 文件都已被 gofmt 格式化（用 `gofmt -w <file>` 修） |
| `make build` | `go build ./...` | 全部包可编译 |
| `make vet` | `go vet ./...` | 标准静态检查 |
| `make deps` | `go list -deps` | 构建出来的第三方 import 集合**恰好**是 `mvdan.cc/sh/v3/fileutil` 与 `mvdan.cc/sh/v3/syntax`；根包 `rig` 不 import 任何非标准库包 |
| `make test` | `go test ./... -count=1` | 单元测试 |
| `make race` | `CGO_ENABLED=1 go test ./... -count=1 -race` | 并发路径的数据竞争 |
| `make cover` | 覆盖率 profile + 打印总数 | 看一眼数字（写 `coverage.out`，已 gitignore） |
| `make cover-floor` | `scripts/coverage-floor.sh` | 覆盖率下限：总语句覆盖 ≥80%，每个有测试的包函数平均 ≥70% |
| `make fuzz` | 逐个 `Fuzz` 目标跑 `FUZZTIME`（默认 3s） | 解析器/输入面的模糊测试冒烟；`make fuzz FUZZTIME=10s` 可加长 |
| `make bench` | `go test -run='^$' -bench=. -benchtime=1x` | 每个 benchmark 跑一次，验证 benchmark 代码本身可用 |
| `make ai-docs` | `go run ./cmd/rigdoc` | 重新生成 `docs/ai/tools.json` 与 `docs/ai/tools.md` |
| `make ai-docs-check` | `go run ./cmd/rigdoc -check` | `docs/ai` 是否与代码一致（本地/发布前检查，**不进 CI**） |
| `make gate` | `go test ./tools/full -run TestToolCatalogCoverage`、`go test ./tools/audit` | 工具账本覆盖与现代工具面审计 |
| `make quickstart` | awk 抽取 README 第一个 Go 代码块并 `diff` | README 快速开始必须与 `examples/quickstart/main.go` 逐字节一致（本地检查，**不进 CI**） |
| `make examples` | `go build ./examples/...` | 示例可编译 |
| `make cross` | 对 windows/amd64、darwin/arm64、freebsd/amd64 交叉编译 | 跨平台编译 |
| `make version` | `bash scripts/sync-version.sh` | `version.go` 与 release tag 一致（未打 tag 的开发检出会 `SKIP`；发布流程用，**不进 CI**） |
| `make clean` | 删除 `coverage.out` | — |
| `make ci` | 依次跑 fmt/build/vet/deps/test/race/cover-floor/gate/examples/cross | 本地**代码**门禁，与 CI 一致（不含 fuzz/bench，也不含 CI 的 `lint`、`windows-sandbox`、`sqlite-consumer smoke`；文档与版本检查是独立目标） |

`make ci` 是 PR 前的最低要求，**只验证代码**。CI 的 job 定义见 [docs/RELEASE.md](docs/RELEASE.md#1-pr--主-ci)；文档生成物（`ai-docs-check`）、README/示例同步（`quickstart`）与版本/tag（`version`）不进入 CI，属于本地习惯与发布流程。

## 代码规则

以下规则都有 CI 或测试守着，不是风格建议。

1. **唯一依赖 + 依赖门禁。** 模块只依赖 `mvdan.cc/sh/v3`（POSIX shell 解析器，仅 `sandbox` 策略层使用）。CI 与 `make deps` 把"构建出来的第三方 import 集合"钉死为它的两个包；随手加一个 `require` 会让门禁变红。要加依赖必须先改依赖策略（[docs/VERSIONING.md](docs/VERSIONING.md)、本文），并保证根包仍然 stdlib-only。
2. **根包是叶子。** 模块根包 `rig` 是稳定契约面，只能 import 标准库，也不能 import 本仓库任何子包。`internal/rigdoc` 的模块化测试与 CI 都检查这一点。
3. **叶子能力包不 import `kit` / `tools/full`。** 分层方向是 `rig → 能力包 → kit/full`，反过来就是循环。`internal/rigdoc` 的 `leafCapabilities` 名单（31 项）是"必须能被单独拿走使用"的守门清单——新增一个可选能力包时把它加进去，否则这条承诺对它没有守护。
4. **`CGO_ENABLED=0`。** 任何包出现 `CgoFiles` 都会被 CI 的 cgo-free 门禁拦下。
5. **注释与包文档用中文。** 导出标识符必须有文档注释，`go doc <pkg>` 应该能直接读。平台差异、边界、fail-closed 行为要写清楚，别让读者去猜。
6. **模型可见字符串不翻译。** 工具的 `Description`、JSON Schema、错误文案、门禁规则 id 与理由属于接口：模型、宿主 UI、审计与快照测试都依赖它们。改这些等于改接口——要更新相关测试/快照，并在 `CHANGELOG.md` 记一条。
7. **工具不自动注册。** 新工具必须显式接线（见下节），否则 CI 的账本覆盖测试会报"构建出来但不在 `catalog.Static`"或"账本里有但没构造"。
8. **契约面只做加法。** 新字段加在结构体末尾；调用方用键值字面量。给已导出接口加方法是破坏性变更，走"可选接口 + 类型断言"。
9. **库代码不 panic**，除了文档化的编程错误（例如给 `tools.NewRegistry` 传重复工具名）。
10. **不要碰发布状态。** 除非这次改动本身就是发版任务，否则不要改 `version.go`、`CHANGELOG.md` 的发布小节或 `LICENSE`；新工作写在 `CHANGELOG.md` 的 `## Unreleased` 下。
11. **不要提交** secret、凭据、生成代码、vendored 代码。
12. **外部运行时依赖禁止。** PTC（`run_code`）只调用系统已安装的 Python 3 / Node.js；`sandbox/osrunner` 的 macOS 分支只 exec 系统自带的 `sandbox-exec`；OS 沙箱只能是 opt-in、默认关闭，且策略层 `sandbox` 不得依赖平台代码或 `sandbox/osrunner`。

## 新增一个工具

工具面是账本驱动的，漏一步 CI 就红。完整清单与账本口径见 [docs/COVERAGE.md](docs/COVERAGE.md) 的"新增工具"一节，顺序如下：

1. **实现包**：在 `tools/<name>/` 下实现，返回一个或多个 `rig.Tool`（稳定的模型可见名字、`Description()`、`Schema()`）。只用标准库；宿主能力通过根包 `rig` 的接口获取。
2. **接线 `tools/full`**：在 `tools/full` 里构造它（`All`/`NewRuntime` 路径）。注意 `NewRuntime` 是推荐入口（能 `Close` 后台句柄），且必须传入 `Root` 或带 roots 的 `Policy`——否则依赖工作区的工具直接不构造。
3. **登记 `catalog.Static`**：`tools/catalog` 的账本。若它复刻旧 agent 的工具名，同时进 `Legacy`；只有配置了 MCP/LSP 客户端才存在的名字进 `ServerBound`。
4. **预设（如需）**：决定它进不进 `minimal`/`personal`/`standard`/`all`（`tools/presets` 的名字表）。`personal` 刻意不包含多智能体编排一族。
5. **测试**：schema、happy path、非法参数、策略拒绝各一组；模型可见文案的断言随实现一起加。
6. **生成 AI 目录**：`go run ./cmd/rigdoc`（或 `make ai-docs`），把 `docs/ai/tools.json`、`docs/ai/tools.md` 一起提交。发布前可用 `rigdoc -check` 自查漂移（不进 CI）。
7. **文档**：更新 `docs/TOOLS.md`（工具目录）、[docs/COVERAGE.md](docs/COVERAGE.md)（清单与口径数字）；工具面数字变了，同批更新 `README.md`、`AGENTS.md` 里的对应数字。
8. **CHANGELOG**：在 `## Unreleased` 下加条目；移除/重命名模型可见工具名要按破坏性变更处理。
9. **跑 `make ci`** 确认代码门禁通过；文档与版本检查按需单独跑（`make ai-docs-check`、`make quickstart`、`make version`）。

## 测试要求

- **覆盖率门禁**（`make cover-floor`，`scripts/coverage-floor.sh`）：总语句覆盖 **≥80%**，每个**有测试的包**函数平均覆盖 **≥70%**。每包下限的存在是为了不让一个测试充分的大包掩盖一个欠测试的小包。`storagetest` 是显式豁免：它是测试辅助包，导出的套件只被别的包的测试调用，覆盖率由消费者负责。
- **race**：涉及并发（工具注册表、事件、后台任务、会话）的改动必须跑 `make race`。
- **fuzz**：模块有 10 个 fuzz 目标，`make fuzz` 会逐个跑 3 秒（CI 用 5 秒）：`FuzzParseSSE`、`FuzzStreamDecode`（`provider/openai`）、`FuzzDecodeRPCEnvelope`、`FuzzDecodeResponseID`、`FuzzReadSSEResponse`（`tools/mcp`）、`FuzzReadMessage`、`FuzzParseLSPResults`（`tools/lsp`）、`FuzzCheckURL`、`FuzzResolvePath`、`FuzzParseShellCommand`（`sandbox`）。改动这些输入面时把 `FUZZTIME` 加长再跑一遍。
- **bench**：`make bench` 让每个 benchmark 跑一次，保证它们还能编译和运行。
- **测试要自包含**：不要依赖 runner 的 `$HOME` 内容、真实用户缓存/临时目录位置、8.3 短名 vs 长名这类路径写法。涉及主目录内容、目录位置、平台后端能力时，用 `t.Setenv` + `t.TempDir()` 自造环境，或按后端能力 `t.Skipf`（否则会"开发机绿、CI 红"）。
- **平台运行期测试**：`sandbox/osrunner` 的 Windows 后端在真实 `windows-latest` 上跑，macOS 的 Seatbelt 在独立 workflow 里跑；新增用例时按平台能力 `t.Skip`，不要让 Linux 上的 `go test ./...` 依赖其它平台。

## 提交规范

使用 [Conventional Commits](https://www.conventionalcommits.org/)，摘要用祈使句、小写：

```text
<type>(<scope>): <summary>

<为什么需要这个改动>
```

- 类型：`feat`、`fix`、`docs`、`test`、`refactor`、`perf`、`build`、`ci`、`chore`、`revert`。
- **破坏性变更**：类型/scope 后加 `!`（例如 `refactor(rig)!:`），并在 footer 写 `BREAKING CHANGE:` 说明迁移方式；同时在 `CHANGELOG.md` 标注 `BREAKING`。
- 引用 issue 写在 footer（`Fixes #123`）。
- 一个提交只做一件逻辑上的事；不要把无关重命名和行为改动混在一起。

## PR 与评审

- Fork 仓库，从 `main` 拉分支（`feat/...`、`fix/...`、`docs/...`）。
- 按 [.github/PULL_REQUEST_TEMPLATE.md](.github/PULL_REQUEST_TEMPLATE.md) 填：摘要、关联 issue、变更类型、**实际跑过的命令**、评审备注。
- **CI 必须绿**。CI 只验证代码：阻塞 job 是 `test`、`windows sandbox (real Windows)`、`lint`、`sqlite-consumer smoke`。文档、版本与 tag 一致性不由 CI 检查（见发布流程）。
- **文档与代码同批**：改了工具面、默认值、版本数字，就在同一个 PR 里更新 `README.md` 的数字、`docs/TOOLS.md`、`docs/CONFIG.md`、`AGENTS.md` 的对应段落。
- **变更模型可见行为必须写 `CHANGELOG.md`**（`## Unreleased`）：工具描述/Schema/错误文案、默认值、权限与审批语义都算。
- **破坏性变更**需要 `CHANGELOG.md` 的 `BREAKING` 条目 + 迁移说明，PR 描述里还要写清影响面与升级路径。
- 解析器、输入面、热路径改动，评审者可能要求补 `make fuzz` / `make bench` 的结果。
- 不新增依赖、生成代码、vendored 代码或密钥。
- 维护者可能 squash 合并；把大改动拆成可评审的提交是受欢迎的。

## 安全漏洞

**不要开公开 issue、PR 或 discussion 报告疑似漏洞。** 唯一的私密渠道是 GitHub 的私密漏洞报告，细节与响应预期见 [SECURITY.md](SECURITY.md)。报告里不要放真实 API key 或私有数据，日志要脱敏。

## 许可

贡献按 **MIT** 接受，inbound = outbound：提交 PR 即表示你以仓库 [LICENSE](LICENSE) 的同一条款授权你的贡献。**没有 CLA**，也不需要版权转让。

只提交你有权贡献的代码。若贡献基于其它项目，其许可必须与 MIT 兼容，且必须在 PR 里说明来源。保留现有的 `LICENSE` 与版权行；不要求逐文件 license 头。

## 相关文档

- [docs/COVERAGE.md](docs/COVERAGE.md) — 工具账本、口径数字与新增工具清单
- [docs/TOOLS.md](docs/TOOLS.md) — 工具目录
- [docs/CONFIG.md](docs/CONFIG.md) — `kit.Config` 全字段与默认值
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — 分层、请求流与模块化约束
- [docs/RELEASE.md](docs/RELEASE.md) — 发布流程与 CI job 说明
- [docs/VERSIONING.md](docs/VERSIONING.md) — 什么算破坏性变更
- [SECURITY.md](SECURITY.md) — 漏洞报告渠道
- [SUPPORT.md](SUPPORT.md) — 提问与支持渠道