# 版本策略

本文说明一个 release 号对使用者承诺了什么、版本信息住在仓库的哪些文件里、标签如何被校验与保护，以及弃用与破坏性变更按什么规则走。读者是三类人：在宿主里依赖 Rig 的产品开发者（决定 pin 哪个版本、升级时该看什么）、准备发版的维护者（版本号与 tag 怎么定）、提交改动的贡献者（什么样的改动算破坏性）。

## 当前状态

- 项目处于 **pre-1.0**，当前 release line 是 **`v0.23.x`**，最新发布版本是 **`v0.23.4`**。
- 模块路径是 `github.com/zeokeel/zeokeel-rig`（v0/v1 都用裸路径），兼容面是模块根包 `rig`。
- 下一个小版本号由维护者按本次变更的性质决定：只修缺陷走 patch，带新能力或带文档化的破坏性变更走 minor。本文不预先承诺具体版本号。
- `v1.0.0` 是稳定性承诺，只有在维护者明确宣布 API 冻结后才会有；在那之前语义化版本的保证弱于 `v1+`（见 [pre-1.0 规则](#pre-10-规则v0x)）。
- 仓库里已经存在但**尚未随任何正式 release 发布**的内容（`storage`、`storagetest`、`memory` 公共契约、`rig.SpillStore`、`session/sqlite` 子模块）记录在 `CHANGELOG.md` 的 `## Unreleased` 段；正式 release 仍是 `v0.23.4`，文档提到这些内容时会单独标注"未发布"。

## 版本信息住在哪

同一个版本号必须同时出现在几处，任何一处不一致都算发布缺陷。权威只有一个：git tag。

| 来源 | 位置 | 作用 |
| --- | --- | --- |
| release tag | git tag `vX.Y.Z` | **权威**。`go get module@vX.Y.Z` 解析的就是它；模块代理与校验和数据库记录的内容也由它决定 |
| `rig.Version` | 模块根 `version.go` 的 `const Version = "X.Y.Z"` | 运行时版本，用于诊断信息与支持请求上报。必须等于 tag 去掉前导 `v` |
| `CHANGELOG.md` | `## vX.Y.Z — YYYY-MM-DD` 小节 | 人类可读的发布说明。release workflow **原样抽取**这一节作为 GitHub Release 正文 |
| `COMPATIBILITY.md` | release line 对照表 | 每条 release line 的 Go 最低版本与依赖策略 |
| `go.mod` | `go 1.27` | 模块要求的最低 Go 工具链 |

补充约定：

- `version.go` 的声明必须保持**单行**，`scripts/sync-version.sh` 用单行 `sed` 读取它。
- `version.go` 反映的是**发布状态**，不是开发标记。只有发版提交会把它改成待发版本，`main` 上其余时间它都等于最新已发布 tag；普通开发不要提前 bump。
- 只有 tag 是发布身份。分支名、提交哈希、`main` 上的 `version.go`、日期都不构成发布标识。

## 同步机制

`scripts/sync-version.sh` 是把上面几处钉在一起的检查器，三种调用方式：

```sh
bash scripts/sync-version.sh                 # 从检出/CI 环境推断 tag
bash scripts/sync-version.sh v0.23.4         # 与显式 tag 比较
bash scripts/sync-version.sh --require-tag   # 发布构建：HEAD 必须有 tag
```

- **推断顺序**：显式参数 → CI 的 `GITHUB_REF_TYPE=tag` / `GITHUB_REF_NAME`（或 `GITHUB_REF=refs/tags/...`）→ `git describe --tags --exact-match HEAD`。
- **普通开发检出**：HEAD 不在任何 tag 上时打印 `SKIP` 并以 0 退出。这是常态，不是失败。
- **release-prep 提交**：发版提交把 `version.go` bump 到新版本后，`main` 的 HEAD 可能仍带着上一个 tag。这种情况只在"tag 来自 `git describe` 且没传 `--require-tag`"时记为 `SKIP`；显式参数、CI tag 上下文与 `--require-tag` 一律严格比较。
- **格式校验**：`version.go` 里的值必须是 `X.Y.Z`（允许 `-pre`/`+build` 后缀）；tag 必须以 `v` 开头。
- **annotated tag 提醒**：在 CI tag 上下文里，脚本会对 lightweight tag 打 `WARNING`（发布 tag 必须是 annotated；release workflow 里这是硬失败，见下）。
- 本地入口有两个：`make version` 单独跑，`make ci` 把它排在最后。

发布侧有三道校验，全部在 `.github/workflows/release.yml`：

1. **验签与 annotated 检查**（`verify` job）：`actions/checkout` 在 tag 事件上会把 `refs/tags/<tag>` 重写成提交 SHA，所以该 job 先用 `git fetch --force --no-tags origin "+refs/tags/<tag>:refs/tags/<tag>"` 把 annotated tag 对象取回来，要求 `git cat-file -t refs/tags/<tag>` 返回 `tag`（不是 `commit`），再用 `gpg.ssh.allowedSignersFile=.github/allowed_signers` 跑 `git tag -v`。签名键不在该文件里就直接失败。
2. **版本一致性**：`bash scripts/sync-version.sh --require-tag`。
3. **提交归属**：`git merge-base --is-ancestor <tag 提交> origin/main`，tag 必须打在一个已经在 `main` 上的提交。

`publish-release` 工作流在发出 `gh release edit --draft=false` 之前还会再跑一次 `scripts/sync-version.sh <tag>` 并确认 Release 仍是草稿。

## 语义导入版本

Go 模块把主版本写进 import 路径，本仓库**严格遵守这一点**：

- **v0 与 v1** 共用裸路径 `github.com/zeokeel/zeokeel-rig`。兼容面就是模块根包 `rig`：`Message`/`Provider`/`Tool`/`Gate`/`History`/`Agent` 以及各类宿主接口。
- **v2 及以后**使用 `github.com/zeokeel/zeokeel-rig/v2`（`/v3` 同理）。`v1.0.0` 之后要破坏契约面，就开一个新主版本的新 import 路径。
- **不要版本化子目录。** 不存在也不会有 `v1/`、`v2/`、`contracts/v1` 这类"同一模块内的版本目录"。子目录 module 只用于**可以独立发布**的单元（目前只有 `session/sqlite`，见[子模块版本](#子模块版本)）。

同一个主版本内，契约面只做加法：

- 新增包、工具、类型、函数、结构体字段、接口都属于加法。新字段加在结构体末尾；调用方应该始终用键值字面量（`rig.AgentSpec{Model: ...}`），这样加字段不会破坏编译。
- **给已导出的接口加方法属于破坏性变更**（外部实现会编译不过）。能力增长走"可选接口 + 类型断言"，不要拓宽已有接口。
- 行为与默认值变化即使签名不变，也可能实际破坏调用方。非平凡的行为变化必须写进 `CHANGELOG.md` 并给出迁移说明。

### 什么算破坏性变更

- 删除或重命名已导出的标识符（包、类型、函数、方法、常量、变量），或改其签名/类型。
- 删除或重命名已导出的结构体字段；在导出布局有意义的场景里把字段插到中间。
- 改变已文档化的函数语义、选项语义或默认值，且合理的使用方可能依赖旧行为。
- 提高最低 Go 版本，或新增模块依赖。
- **默认放宽沙箱/权限**（例如某个模式默认变成 `danger-full-access`、默认不再保护某类路径）。这是安全相关变更，即使 API 没变也按破坏性处理。

新增导出 API、新增工具、新增可选配置字段、新增后端实现都不算破坏性变更。

## pre-1.0 规则（`v0.x`）

模块还是 pre-1.0，所以承诺比 `v1+` 弱。本项目的具体规则：

| 变更 | 允许内容 | 兼容承诺 |
| --- | --- | --- |
| `v0.Y.Z+1`（patch） | 缺陷修复、文档、性能、安全修复；加法式导出 API 通常也应留到 minor | 现有代码能编译、行为一致，除了被修正的缺陷本身 |
| `v0.Y+1.0`（minor） | 新特性、新工具、新包；**可能包含破坏性契约变更** | 破坏性变更必须在 `CHANGELOG.md` 里标注 `BREAKING` 并给出迁移说明 |
| `v1.0.0`（major） | 稳定化；第一次兼容性承诺 | 从这里开始严格执行语义化版本 |

使用方的建议：

- 在 `go.mod` 里 pin 精确版本，升级要刻意进行；
- 升级前读 `CHANGELOG.md` 的 `## Unreleased` 与目标版本小节；
- 结构体一律用键值字面量，避免加字段时编译失败；
- 把根包 `rig` 当作集成点，不要依赖文档没有承诺的内部细节。

## 弃用策略

弃用是"先把替代品放出去，再移除旧东西"，三类对象分别处理：

**字段、选项、方法**

1. 在文档注释里用 Go 标准写法标记：`// Deprecated: 用 X 取代。`（说明替代品与迁移方式）。
2. 在 `CHANGELOG.md` 的 `### Deprecated` 里记一条。
3. 至少保留一个 minor 周期（pre-1.0）；废弃代码保持有测试，直到真正删除。
4. 删除属于破坏性变更：pre-1.0 需要 `BREAKING` 标记与迁移说明；post-1.0 只能落在新的主版本里。

**模型可见的工具名**

工具名是接口的一部分：提示词、宿主脚本、工具账本（`catalog.Static`/`Legacy`）与审计都按名字工作。因此：

1. 先新增工具名（并在描述里说明它取代谁），旧名在同一版本里标记为 deprecated（描述里指向新名）；
2. 旧名保留至少一个 minor，期间新旧并存；
3. `CHANGELOG.md` 写清"哪个名字在哪个版本移除、迁移到什么"；
4. **移除模型可见的工具名属于破坏性变更**，不是普通重构。

**整包移除**

包被整包删除时，除了 `CHANGELOG.md` 的 `Removed` 条目，还要同步删掉仓库里守护"这些包已删除、不得回归"的测试允许清单——本仓库用测试拦住"删掉的包被悄悄加回来"。

当前存在的废弃项举例：`rig.Presenter` 由 `rig.FilePresenter` 取代；`kit.Config.UndoTracker` 已废弃（仅为兼容自建 tracker 的宿主保留）；`kit.Config.ProtectWorkspaceMetadata` 是无效字段。它们继续编译、继续可用，但不再演进。

## 子模块版本

`session/sqlite` 是**独立 module**（自己的 `go.mod`、自己的 tag），它遵循 Go 的多 module 仓库规则：

- 子目录 module 的名字是 `<仓库路径>/<子目录>`，所以它的 tag 必须带目录前缀：**`session/sqlite/vX.Y.Z`**。主模块的 `vX.Y.Z` tag **不覆盖**它。
- 它可以有自己的依赖（`modernc.org/sqlite` 及其间接依赖），这些不进根模块的依赖门禁——根模块仍然只有一个解析器依赖。主模块**永不** import 它。
- 它的 `go.mod` 用 `require github.com/zeokeel/zeokeel-rig vX.Y.Z` 指向某个主模块版本；这个 `require` 必须等于**最近一个根 tag**（唯一的例外是待发版窗口：`require` 先指向本次要发的版本，根 tag 打出后才相等）。
- 发布顺序不能反：**先根 tag，再子模块 tag**，两者可以指向同一个提交。子模块 tag 的提交必须包含根 tag 的提交。
- 它可以在 `go.mod` 里带 `replace github.com/zeokeel/zeokeel-rig => ../..`，但这只服务本仓开发（本地与 CI 直接用工作树）。**依赖方会忽略它**：Go 只应用主 module 的 `replace`。所以"本地能编"不是"宿主能编"的证据。
- `release.yml` 的触发条件是 `tags: v*`，只匹配以 `v` 开头的 tag，**推子模块 tag 不会触发它**；子模块 tag 是手工步骤。

当前状态：`session/sqlite/go.mod` 的 `require` 指向**尚未发布**的下一版主模块（它 import 的根模块 `memory` 包在 `v0.23.4` 里还不存在），因此该子模块要随下一个版本一起发布。在此之前不要把"SQLite 会话存储已经可用"写进任何面向用户的说明；`docs/sqlite.md` 记录的是"待随下一版本发布"的事实。

CI 只盯代码/打包面：`sqlite-consumer smoke` 在**空的**模块缓存、临时 module 里
`go get` 子模块并编译运行（无 `go.work`、无 `replace`），证明宿主真的能用起来；它读
`go.mod` 里的 `require`，不写死版本。

"require == 最近根 tag""子模块 tag 存在""子模块 tag 提交包含根 tag 提交"这三条属于
**发布面自洽**，不在 CI 里：发版前由发版人自查（`make version`、`git tag -l`），
`release.yml` 的 `verify` 与 `published-module smoke` 会再次校验。

## 标签不可变与校验和

已发布的 tag 是**不可变**的：

- 不要移动、删除或 force-push 已发布的 tag。模块代理与 `sum.golang.org` 会缓存 tag 内容；移动 tag 会产生校验和不匹配，破坏所有已经依赖它的构建。
- 推送时只推单个 tag（`git push origin vX.Y.Z`），不要 `git push --tags`，避免把本地无关 tag 推出去。
- 发布 tag 必须是 **annotated 且签名** 的（`git tag -s vX.Y.Z -m "..."`，SSH 签名键的私钥由维护者持有，公钥登记在 `.github/allowed_signers`）。lightweight tag 或验签失败会在 release workflow 的 `verify` job 被拦下，Release 对象根本不会创建。
- tag 必须打在 `main` 上的提交，且该提交带着本版本的 `CHANGELOG.md` 小节与匹配的 `version.go`。

发错了怎么办：

1. **向前修**：在 `main` 上修好，发一个新的 **patch** 版本。永不移 tag。
2. 如果这个版本绝对不能被使用，在修复版本的 `go.mod` 里加 [`retract`](https://go.dev/ref/mod#go-mod-file-retract) 指令；retract 只是给 `go` 命令的信号，不改写历史。安全问题同时按 [SECURITY.md](../SECURITY.md) 走私密渠道并出 GHSA。
3. GitHub Release 本身缺失或正文有误时，见 [docs/RELEASE.md](RELEASE.md) 的失败处理；tag 依然不可变。

校验和的核对方式：

- release workflow 的 `smoke` job 在干净环境里 `go get` 新 tag、`go mod tidy`、`go mod verify`，下载内容与校验和数据库比对；
- 人工核对：`https://sum.golang.org/lookup/github.com/zeokeel/zeokeel-rig@vX.Y.Z`，以及 pkg.go.dev 上该版本页面是否渲染。

## 预发布版本

`vX.Y.Z-rc.N`、`-beta.N` 都是合法的 Go 模块版本，走同样的 tag 规则。`go get` 不会自动选中预发布版本，必须显式指定。

注意：`release.yml` 对**任何** `v*` tag 都只创建草稿 Release，且不设 `--prerelease`。如果你要发预发布版本，要么在发布时手工 `gh release edit <tag> --prerelease`，要么先扩展工作流。

## 与 Go 版本、依赖的关系

- 模块要求 **Go 1.27**（`go.mod` 的 `go 1.27`）。提高最低版本要在 `CHANGELOG.md` 与 `COMPATIBILITY.md` 同时记录；pre-1.0 期间它按兼容性相关变更对待。
- 模块只有**一个**依赖：`mvdan.cc/sh/v3`（POSIX shell 解析器，仅 `sandbox` 策略层使用）。根包 `rig` 是叶子，只用标准库；CI 把"构建出来的第三方 import 集合"钉死为这个依赖的两个包。
- 构建与测试一律 `CGO_ENABLED=0`。`go test -race` 需要 cgo 是 Go 工具链的要求，不属于库的构建依赖，也不影响发布产物。
- `COMPATIBILITY.md` 记录每条 release line 的 Go 最低版本与依赖情况；`README.md` 展示当前安装用的 tag。

## 相关文档

- [docs/RELEASE.md](RELEASE.md) — 发版步骤、失败处理与发布后检查清单
- [COMPATIBILITY.md](../COMPATIBILITY.md) — release line 的 Go 版本与依赖对照
- [CHANGELOG.md](../CHANGELOG.md) — 每个版本的实际变更
- [CONTRIBUTING.md](../CONTRIBUTING.md) — 提交规范与破坏性变更的提交流程
- [SECURITY.md](../SECURITY.md) — 支持版本与漏洞报告渠道