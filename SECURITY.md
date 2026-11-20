# 安全策略

本文说明哪些版本会收到安全修复、如何**私下**报告漏洞，以及内置权限模型与可选 OS 沙箱各自承诺什么、明确不承诺什么。读者是三类人：把 Rig 嵌进产品的宿主开发者（判断自己的威胁模型是否匹配）、想报告问题的人、以及实现工具或平台的贡献者。

Rig 是纯 Go、无 cgo 的 SDK（唯一依赖 `mvdan.cc/sh/v3`，只被 `sandbox` 策略层使用），agent 循环与全部内置工具都跑在**宿主进程内**。这个事实决定了下面所有边界。

## 支持的版本

只有当前 release line 的最新 patch 会收到安全修复。项目处于 pre-1.0，没有长期支持分支。

| 版本 | 是否修复 |
| --- | --- |
| `v0.24.x`（当前 `v0.24.0`） | ✅ 仅最新 patch |
| `v0.22.x` 及更早 | ❌ 请先升级到 `v0.23.x` |

支持线的 Go 最低版本是 **Go 1.27**（`go.mod` 声明）。更旧的工具链请先升级 Go 再升级 Rig。使用方应在 `go.mod` 里 pin 精确版本并刻意升级，规则见 [docs/VERSIONING.md](docs/VERSIONING.md)。

## 如何私下报告

**永远不要用公开 issue、PR 或 discussion 报告疑似漏洞。** 唯一渠道是 GitHub 的私密漏洞报告：

1. 打开 <https://github.com/zeokeel/zeokeel-rig/security/advisories/new>（仓库 **Security** 页 → **Report a vulnerability**）。仓库已启用私密漏洞报告；[`.github/ISSUE_TEMPLATE/config.yml`](.github/ISSUE_TEMPLATE/config.yml) 里的 security 链接也指向这里。
2. 如果你的账号用不了这个表单，就开一个**只写**"我需要一个私密安全渠道"的最小公开 issue 并 @`zeokeel-dev`。**不要**在公开 issue 里写漏洞细节、复现步骤、受影响组件或凭据。
3. 报告里尽量包含：
   - 受影响版本（`rig.Version` 或 `go.mod` 里的 release tag）与 `go version` 输出；
   - 操作系统/架构；沙箱相关问题请附策略配置（`sandbox.Mode`、`kit.Config.Approval`、可写根、`AllowNetwork`、是否注入了 `Gate`/`SandboxRunner`）；
   - 最小复现（一个 Go 测试或小程序）与观察到的影响；
   - 你能想到的缓解方式。

不要在报告里放真实 API key 或私有数据；日志先脱敏。

## 我们会怎么做

这是志愿者维护的开源项目，下面的时限是尽力而为的目标，不是商业 SLA（见 [SUPPORT.md](SUPPORT.md)）：

- **3 个工作日内**确认收到报告；
- **10 个工作日内**给出初步评估（严重性、受影响版本、是否能修）；
- 确认的问题进入下一个 patch，并发布 GitHub Security Advisory（GHSA）；
- **协调披露**：目标是在报告后 90 天内发布公告（修复可用时更早）。在公告前请不要公开细节，除非双方另有约定；
- 除非你要求匿名，公告里会署名致谢。

如果报告只涉及下面[范围外](#范围外)列出的内容，我们可能会以"超出范围"关闭，并说明理由。

## 威胁模型

### 进程内围栏不是内核边界

SDK 跑在宿主进程里。宿主链接进来的代码（自定义 `rig.Tool`、`rig.Provider`、`rig.Gate`、`rig.History`、searcher、skill 实现）是**可信代码**。内置的 `sandbox` 策略层约束的是**内置工具**能碰什么：它不是 OS 沙箱，也不是第三方工具的沙箱。

**不可信的模型应当跑在容器或 VM 里。** 进程级隔离是部署层的事，不是这个 SDK 能提供的东西。

### 审批（Gate）才是真实边界

策略层是给"合作模型"的门禁；真正的边界是宿主注入的审批通道。

- `Approval: "ask"`（默认）**只有两层**（DSH 对齐）：① 只读工具**免问**；② 其余
  （写文件、`bash`/`pwsh`、后台任务，**包括 `rm -rf /` 这类危险命令**）**问**
  `kit.Config.Asker`。**默认不设超时**：人多久不回就等多久，只有取消本轮才撤回
  （`Config.AskTimeout>0` 才设预算）。
- 宿主没接 `Asker` 时不会"自动有人问"：kit 退到 `gate.PersonalAskWithoutAsker()`——
  只读放行、其余拒绝并明确告诉模型"没有可用的审批通道"。headless 宿主必须自己接
  `Asker`，或明确接受这个拒绝行为。
- `Approval: "auto"` **就是** `AllowAll()`：不问人、也不分类命令——`rm -rf /` 会执行。
  这是"不问人"的准确含义；需要保护就换 `ask` 档，或显式叠
  `gate.CommandRiskGate`。
- `Approval: "never"` 拒绝一切。
- 默认门禁**不含** `CommandRiskGate`；宿主若要命令风险硬拦，必须自己把它叠在
  `AllowThenAsk` **之外**（最外层），否则每条 shell 命令都会先弹窗。
- 把 `Gate` 换成 `gate.AllowAll()` 等于主动放弃这条边界；`Approval: auto` 就是它的
  预设形态，这是宿主的选择。

### 危险命令判定

**默认门禁不做命令风险分类**（DSH 对齐）：`rm -rf /`、`rm -rf ~`、`mkfs.*`、
`curl … | sh` 与其它可变命令一样走人工审批，由人决定。`sandbox.ClassifyCommand` 与
`gate.CommandRiskGate` 仍然完整保留，宿主可显式组合来做"命中即拒绝"。

命令允许列表（allowlist）层面的加固仍然生效：拒绝未加引号的控制操作符（`;`、换行、`|`、`&`、重定向、子 shell）、拒绝命令替换 `$(...)` 与反引号（**即使在双引号内**）、拒绝进程替换、拒绝 `VAR=value` 前缀与路径形式命令名（防止 `PATH=.`、`LD_PRELOAD=...` 借用被允许的 basename）、只按裸命令名匹配允许项。

允许列表里放进解释器（`sh`、`bash`、`env`、`xargs`、`python`、`eval` 等）等于给它任意代码执行能力——不要为不可信模型允许解释器。允许列表是给合作模型用的策略层，不是安全边界。

### 逐命令提权的边界

`tools/builtin` 支持 `sandbox_permissions: "require_escalated"` + `justification`（≥10 字符）。它的语义被刻意压到最小：

- 只在**人工批准**之后，让**这一条命令**不被 OS runner 包装；
- 策略、workdir、超时、输出上限**一律不变**；
- `auto` / `never` 档**拒绝**该参数；
- 审批由宿主通过 `builtin.WithEscalator` / `full.AllOptions.Escalator` 注入，审核钩子见 `builtin.EscalationRequest`。

它替换的是"这一条命令的 OS 包装"，不是权限策略本身。

### 保护路径：已不再提供（DSH 对齐）

**本 SDK 当前不提供保护路径。** 早期版本会默认保护 workspace 元数据（`.git`）与主目录
凭据位置（`~/.ssh`、`~/.aws`、`~/.config/gh`、`~/.netrc`），DSH 对齐后这层强制已经
停用：

- `sandbox.ProtectedPaths()` 恒返回空，`WithProtectedPaths`/`AddProtectedPaths` 只是
  记录、不参与任何判定；
- 拒绝 glob（`WithDeniedGlobs`）同样只记录不拦截；
- `kit` 不再追加任何保护路径，`DisableProtection`/`ProtectPaths`/
  `ProtectWorkspaceMetadata` 都是 no-op（保留仅为源码兼容）。

仍然生效的"根内只读"只有一种表达：`sandbox.WritableRoot.ReadOnlySubpaths`（例如
`.git/config`、`.git/hooks`，只对 bwrap/Seatbelt/ACL 后端有效）。需要保护凭据或
`.git` 的宿主必须自己构造 `sandbox.Policy` 并在**宿主侧**做校验（或把敏感文件移出
可读面、用容器/VM）。**不要**把本 SDK 当成凭据保护层。

### OS runner 只治理文件效果

可选的 `sandbox/osrunner` 是**opt-in、默认关闭**的纯 Go runner（无 cgo、零新增依赖）：宿主显式 `osrunner.New` 并注入工具的 `WithRunner` 才生效。它实现 `sandbox.Runner`，把 `bash`/`pwsh`、后台作业、持久终端、git worktree 的进程包成平台原生受限形式：

- Linux：`bwrap` 优先，其次内置的 Landlock 启动器；
- macOS：系统自带的 `sandbox-exec`（Seatbelt/SBPL）；
- Windows：Write-Restricted 受限令牌 + 目录 ACL 授权的隐藏 helper。

它的承诺边界很窄，必须准确理解：

- **只治理文件效果**（写、删、建），且只到"写哪里"。网络访问、读取范围、进程可见性、
  设备访问**都不在承诺内**——DSH 对齐后**不再**渲染 `--unshare-net`/seccomp/`(deny
  network*)`/socket 遮蔽，沙箱内的命令可以联网。
- 它是**同世界（same-world）约束**，不是容器/VM 级隔离。
- fail-closed：无法强制请求的模式时返回 `sandbox.ErrSandboxUnavailable`，**绝不裸跑**。
- 可写面：`read-only` 只留 `/dev/null`（**没有**可写 `/tmp`）、`workspace-write` =
  `Roots()` ∪ `AdditionalWritableRoots()` ∪ 私有 `/tmp`、`danger-full-access` 不包装
  （`Enforcement=none`）。**没有** `− ProtectedPaths()` 这一项。运行期授权
  （`request_sandbox_access`）只能由审批通过后调用 `Policy.AddWritableRoot`，工具自身
  不审批。
- 平台覆盖程度不同：Windows 与 Landlock 后端报告 `partial`（见[已披露限制](#已披露限制)）。

### 凭据

**默认不脱敏。** 产品定位是个人 agent 客户端（单用户），工具输出、命令环境与错误里可能包含宿主的 API key 与环境变量；`kit` 默认不在工具边界做脱敏，因为对一个单用户客户端来说，把用户自己的凭据遮起来通常只是在干扰排障。

**命令环境也原样继承。** `sandbox.Policy.CommandEnv` 在宿主未显式
`sandbox.WithEnvAllowlist(...)` 时不做任何过滤：`bash`/`pwsh`/`jobs`/`terminal` 以及
`run_code` 的解释器都能读到宿主的全部环境变量——包括 API key、token、云凭据。这与
DSH 的 bash 行为一致，也是"沙箱是唯一防护层"的决定的一部分：进程内的环境过滤既不是
安全边界，又会挡住模型本该能用的工具链（`PYTHONPATH`/`VIRTUAL_ENV`/`GOPATH`）。

要收紧环境，显式给白名单：`sandbox.WithEnvAllowlist(sandbox.DefaultEnvAllowlist()...)`
（只保留 PATH/HOME/LANG 与 Windows 的 temp/system 变量），或自己列一份；
`WithEnvAllowlist()` 传空则命令以空环境运行。要真正隔离凭据，请把 agent 跑在容器/VM
里，或用 `sandbox/osrunner` 把文件效果与宿主分开——**不要**假设 `Rig` 会自动剥离凭据。

`secrets.RedactForExport` 是**仅用于导出**的清洗：宿主要在把日志/会话导出、上报或提交到外部系统之前自行调用它。若你的宿主会把内容发给第三方，脱敏责任在宿主这一侧（可通过 `kit.Config.Redactor` 接线）。

### 交互式终端不是安全边界

`terminal_*` 的 shell 二进制必须通过允许列表，且每一行 `terminal_send` 都经过与非交互 shell 相同的操作符/替换与命令检查（`cd` 之类的 shell 内建需要显式加入允许列表）。但交互式会话里的状态不构成隔离：一旦允许了任意解释器，后续输入就是任意代码。

## 范围内

- **策略绕过**（已发布代码）：`sandbox.ResolvePath` 的路径穿越/符号链接逃逸、glob/grep 的隐藏文件泄漏、命令允许列表绕过、`tools/web` 的 URL/SSRF 策略绕过。
- **OS runner 绕过**：在被声明为 `full` 强制的平台上，受限命令逃出声明的可写根；或 `Confine` 报错后降级成不受限的 spawn。
- **命令执行**：`bash`/`pwsh`/终端/作业工具执行了配置策略本应拒绝的命令。
- **网络策略**：`web_fetch` 在策略禁止时仍到达 loopback、私有地址、链路本地或云元数据地址，包括通过重定向或 DNS rebinding 实现。实现上，fetcher 自己解析每个被拨号的域名并**连接已校验的 IP**（校验与连接之间地址不能改变）；重定向的每一跳同样校验。
- **不可信输入导致的崩溃**：模型输出、工具参数、SSE/JSON-RPC/LSP 帧或 URL 触发的 panic 或无界资源占用。LSP 的单条消息头块有 64 KiB 硬上限（在 `Content-Length` 已知之前就已生效，包括一条永不结束的头行）。
- **凭据处理**：通过工具输出、命令环境或错误泄漏 API key / 环境变量。

## 范围外

- 需要"在进程内运行不可信代码"的场景：宿主提供的自定义 `rig.Tool`、`rig.Provider`、`rig.Gate`、`rig.History`、searcher、skill 实现按设计是可信代码。
- 审批 `Gate` 的选择：宿主装了宽松的 gate（或 `nil`，等于放行一切）就是选择了放行。
- OS、容器或文件系统配置错误；用 root 跑 Rig；工作区根本身对不可信方可达；`GOMAXPROCS`/内核限制。
- 模型端点或代理本身、提示词注入导致"被允许的工具做了不该做的事"、计费/用量、宿主环境的 TLS 拦截。
- 宿主与 Rig 一起链接进来的代码（包括基于 Rig 的产品）中的漏洞；缺少 TLS 证书 pinning。
- 只针对更早的、不受支持版本的报告（除非能在受支持版本上复现）。
- 通过 `sandbox/osrunner` 要求的读、网络、进程可见性或设备隔离：它的契约只有文件效果，macOS 分支还依赖 Apple 继续提供已弃用的 `sandbox-exec`。

## 已知边界（KNOWN-BOUNDARIES）

下面这些是**明确接受、刻意不修**的能力边界：修掉它们会破坏契约、需要平台专用代码，或成本远超收益。写在这里是为了让宿主的风险判断有依据——它们不是待办清单，也不会在补丁版本里悄悄变化。每条在代码里都有测试或配置项守着（本文不列内部测试路径）；能修且值得修的问题按[范围内](#范围内)的流程报告。

> **DSH 对齐后的状态**：带「仅历史」的行描述的是 v0.23 及更早版本的加固，那些加固
> 已经停用（见上文的"保护路径：已不再提供"与 OS runner 的承诺边界），保留在此只为
> 解释历史报告；带「需显式组合」的行仍然成立，但只在宿主自己把
> `gate.CommandRiskGate` / `sandbox.ClassifyCommand` 接进审批链时才有意义。

| 编号 | 组件 | 边界 | 影响 | 缓解 |
| --- | --- | --- | --- | --- |
| KN-01 | `secrets` | 不做熵检测（不按信息熵/字符分布打分） | 未列入内置模式、也未被 `Add` 登记的自定义凭据形态不会被识别 | 宿主把当前凭据用 `Add`/`AddNamed` 登记为已知明文 |
| KN-02 | `secrets` | 内置模式全部**单行** | YAML 里 `password:` 与缩进值分居两行、跨行拼接的凭据会漏判 | 敏感值写成单行；或先自行拼接再 `Redact` |
| KN-03 | `secrets` | 数字型秘密不脱敏：敏感键下的 number/bool/null 逐字节保留 | `{"pin":123456}` 里的数字原样输出 | 敏感值用字符串承载；或在上游序列化之前替换 |
| KN-04 | `secrets` | 流式 `Writer`/`Reader` 的单行上限是 `MaxBuffered` = 64 KiB | 没有换行的超长单行会被强制冲刷（有延迟），单条已知明文超过 64 KiB 时可能被切开；非流式路径不受影响 | 超大输出先整体 `Redact` 再写（无边界面、无延迟），或按行输出 |
| KN-05 | `tools/search` | 硬链接是同一 inode 的另一个名字，可能指向根之外；`lstat` 无法知道另一个名字在哪 | 内容检索可能读到根外的数据（不是路径逃逸） | `WithHardlinkGuard(true)` 整类跳过 `Nlink > 1` 的文件（默认关闭以保持兼容）；Windows 取不到链接数，守卫退化为不跳过 |
| KN-06 | `tools/search` | 内容检索每次调用现建内存 BM25 索引，不分片、不缓存 | 内存峰值与入选文件总字节数同阶，大仓库有分配压力 | `WithMaxFileBytes`（默认 1 MiB）与 `WithMaxResults`（默认 20，上限 200）收窄；宿主侧先粗筛 |
| KN-13 | `session/store` | 单进程单实例：索引在内存里（`NewDir` 扫目录后由增删改维护），只在进程内按 ID 分片加锁，不做跨实例/跨进程协调 | 同一目录上的两个实例各持独立索引，看不到对方新建的会话；数据都在盘上不会丢，但两边 `List` 不完整、计数偏小 | 一个目录只由一个 `Dir`/agent 实例使用；"重启后继续"只需关掉旧实例再 `NewDir`（索引就是扫目录，无需迁移） |
| KN-16 | `tools/patch` | Unix 上 `os.Root.Chmod` 有 stdlib 级竞态：`Lstat` 与 `Chmod` 之间目标被换成符号链接时可能作用到链接本身 | 极端并发下"改权限"可能落到非预期对象；不能用来防敌对本地进程 | 落盘走 `internal/fsguard` 的原子写（`Chmod` 施加在自己 `O_EXCL` 创建的临时文件上，不暴露该竞态）；工具文档声明"约束配合的模型，不是敌对进程" |
| KN-17 | `tools/patch` | 路径不做大小写归一化，同一补丁内的路径去重按字节比较 | 大小写不敏感的文件系统（macOS 默认、Windows）上 `A.md` 与 `a.md` 会被当成两个文件而同写一个物理文件 | 宿主在策略层拒绝同名不同大小写的路径 |
| KN-19 | `agent` + `session/compact` | 自动压缩的结果**不写回** `History`：压缩只作用于下一轮请求的内存对话，持久化替换只发生在手动 `kit.Agent.CompactNow` | 用 `compact.LLMCompactor` 的宿主每轮都要重新摘要（多一次模型调用、按 token 计费），摘要文本可能逐轮变化 → provider 的 prompt 缓存失效；长会话下这是最贵的一条隐性成本 | ① 用确定性压缩器（`WindowCompactor`，零额外调用，kit 默认）；② 回合之间调 `CompactNow` 持久化；③ 自己按会话维护摘要缓存 |
| KN-20 | `agent` | 不做"零改动绕圈检测"：只识别"完全相同的一批调用连续重复"，不判断"多轮不同调用但工作区零进展" | 模型在两个工具之间来回摇摆（每次参数都不同）时不会被熔断，只能靠 `MaxIterations` 兜底 | ① 总闸 `kit.Config.MaxIterations`（kit 默认 200）与 `MaxRunCostUSD`；② 宿主可用 `UndoTracker().Changes()` 判断回合是否有改动，自己实现"零改动 N 轮就问用户" |
| KN-21 | `sandbox` | **需显式组合**：`write.git.control` 只识别**字面量**路径（含引号、`$PWD/`、`~/`、相对路径）；动态拼接识别不了 | `p=.git/hooks; echo x > $p/pre-commit`、Python 拼字符串、base64/eval 解码后再写等形态逃过该规则 | 这些形态退化为 `redirect.dynamic` / `unknown.program` → **需要审批**（不是静默放行）；真正的边界仍是 OS 沙箱（bwrap/Seatbelt/ACL 后端上 `.git/hooks` 在 OS 层只读） |
| KN-22 | `sandbox` | **需显式组合**：git 自己的子命令可以在明确路径上写控制面文件：`git checkout <rev> -- .git/hooks/x`、`git restore --source`、`git apply`（补丁内容指向 `.git/…`） | 用仓库里的内容写出一个 hook，绕过"命令写文件"规则 | 这是 git 的正常语义，拦它会误杀（`git checkout -- .` 是最常见操作）；恶意仓库本身就能让你执行它的脚本。要更强保证就用 bwrap/Seatbelt/ACL 后端 |
| KN-23 | `sandbox` | **需显式组合**：输入决定写入目标的程序不分析内容：`tar -xf`、`unzip`、`patch -p1 < d.diff`、`git am`、`npm install`（postinstall） | 归档/补丁/安装脚本里可以含 `.git/hooks/...` | 这些程序本身是 unknown/mutating → 需要审批；安装脚本执行属于供应链面，不在命令风险层 |
| KN-24 | `sandbox` | **需显式组合**：`pwsh`/`cmd` 的命令文本按 **POSIX** 解析器判定 | `Set-Content .git\config x`、`Out-File`、`Copy-Item` 在 `pwsh` 工具里是未知程序 → 需要审批；`> .git/hooks/x` 仍会被识别为写并判危险 | 审批 + OS 沙箱（Windows ACL 后端下 `.git/hooks` 有 deny ACE）；PowerShell 专用规则尚未添加 |
| KN-25 | `sandbox` | **需显式组合**：worktree/submodule 的**真实** gitDir 可能在别处（`.git/worktrees/<name>`、子模块的 `.git` 目录） | 直接写 `<真实 gitDir>/hooks/x` 的路径里不含 `.git/`，规则不匹配 | 该目录默认不在可写面内；Landlock 的降级只覆盖工作区里的 `.git` |
| KN-26 | `sandbox/osrunner` | `WithLandlockGitDegrade(false)` 下 Landlock 后端无法 `git commit`（创建不了 `.git/index.lock`） | 功能受损是**有意的**：宁可不能提交，也不留 OS 层 hook 可写面 | 需要提交的宿主用 bwrap/Seatbelt/ACL 后端；或保持默认（true）+ 命令风险层兜底 |
| KN-27 | `sandbox/osrunner`（Landlock） | 不治理元数据变更：`chmod`/`chown`/`utimensat` 完全在 Landlock 的 `handled_access_fs` 之外 | read-only 域下 `chmod 666 ~/.gitconfig`、`touch -d 2020-01-01 <file>` 真的生效——内容写不进去，但权限位与时间戳能改 | Landlock 恒报 `EnforcementPartial`，`RunnerLimits()` 逐字说明这三类操作；需要元数据保护时用 bwrap/Seatbelt |
| KN-28 | `sandbox/osrunner` | **仅历史**（socket 遮蔽已停用，沙箱内 socket 完全可达）：unix socket 逃逸面：bwrap 的路径级 socket 屏蔽只覆盖名单里的少数几个，Landlock 完全不管 unix socket（只治理 TCP） | 经 `SSH_AUTH_SOCK`/dbus/X11/容器守护进程 socket 发出的请求属于**宿主动作**——例如 read-only 模式下仍能借容器客户端读取宿主信息 | `AllowNetwork=false` 时装手写 BPF seccomp（bwrap 走 `--seccomp`，Landlock 由 helper 在 exec 前装）拦截 `connect`/`bind`/`listen`/`accept`/`sendto`/`sendmsg` 等并返回 `EPERM`，装不上即 `ErrSandboxUnavailable`（fail closed）；`AllowNetwork=true` 时不装（否则回环不可用），此时同机守护进程必须视为可信面 |
| KN-29 | `sandbox/osrunner`（Landlock） | 并集语义：path-beneath 规则没有否定规则，"根可写但其中某子路径只读"无法表达 | ① `.git` 为了让 `git commit` 跑通而**整根可写**——`.git/config`、`.git/hooks` 在该后端 OS 层可写（进程内围栏与 bwrap/Seatbelt/ACL 仍拒绝）；② 网络治理取决于 Landlock ABI（< 4 完全没有） | `WritableRoot.DegradeOnUnionBackends` 只对 git 元数据开启，缓存根（含凭据文件）仍严格展开；`RunnerLimits()` 如实报告网络覆盖程度 |
| KN-30 | `sandbox/osrunner` | **仅历史**（runner 不再追加工具链缓存根）：工具链缓存根用**宿主进程**的 `HOME`/`TMPDIR`/`%LOCALAPPDATA%` 计算，被约束进程继承的却是**启动时的环境** | 两者不一致时白名单会指向别处 → 相关目录全部 DENY | `BuildProfile` 把实际生效的根暴露在 `Profile.ToolchainCaches` 与 `MissingToolchainCaches`，`sandbox_status` 的 `toolchainCaches`/`missingToolchainCaches` 直接展示；**宿主改过 `HOME`/`TMPDIR` 之后必须重新 `BuildProfile`/`Confine`** |

已移除的能力不再有对应边界条目：`mcpserver` 与 `cmd/rigmcp`、`internal/filewatch`、`sandbox/escalate`、`tools/escalate` 都已整包删除（本产品是个人 agent 客户端）。要放宽权限用 `danger-full-access`、审批，或 `tools/builtin` 的逐命令提权；需要把自己的工具暴露成 MCP server 的宿主应自建 server，再用 `full.AllOptions.MCPClients` 接回来。

## 供应链

- **恰好一个依赖**：`mvdan.cc/sh/v3`（仅 `sandbox` 使用）。CI 把构建出来的第三方 import 集合钉死为它的两个包，并检查根包 `rig` 只 import 标准库；`CGO_ENABLED=0` 由专门的门禁保证。
- **Actions 固定到完整提交 SHA**（每个 workflow 都是），由 Dependabot 更新 pin（`.github/dependabot.yml`）。
- **annotated tag**：发布只能来自 `main` 上的 annotated tag（**不要求签名**，个人 SDK 的 tag 由维护者直接推）；`release.yml` 的 `verify` job 检查 tag 是 annotated 对象、指向 main 的提交、`version.go` 与 tag 一致。
- **发布构建跑同一套门禁**：tag 通过 `workflow_call` 复用 `ci.yml`；随后校验 `version.go` 与 tag 一致、tag 提交在 `main` 上；再做干净环境的 `go get` + `go mod verify` 冒烟，最后**直接发布** Release（不打草稿）。`contents: write` 只出现在这个发布 job 上；门禁不过就什么都不发。
- **完整性锚**：模块代理与 `sum.golang.org`。smoke 里的 `go mod verify` 会把下载内容与校验和数据库比对；人工核对用 `https://sum.golang.org/lookup/github.com/zeokeel/zeokeel-rig@vX.Y.Z`。
- **漏洞与静态检查**：CI 的 `lint` job 跑 govulncheck v1.9.0（Go 已知漏洞）与 staticcheck v0.8.2（固定版本，现场安装）。
- 供应链方面的疑虑同样走私密报告渠道。

## 已披露限制

这些限制是已知且被接受的，不以"漏洞"处理；发现它们与文档不符则是缺陷，请报告。逐条的、刻意不修的能力边界另见[已知边界](#已知边界known-boundaries)。

- **Windows 运行时是 `partial`。** 真机由 `windows-latest` 上的 `sandbox/osrunner` 用例覆盖（ACL/受限令牌/Job 对象），但读与网络整体不受限；helper 会为授权根下的既有对象添加针对 capability SID 的继承 ACE，这些 ACE 作为缓存资产在 helper 退出后仍留在真实目录上（只对该 SID 生效，不影响宿主用户）。该 runner 的镜像**故意不固定**版本（ACL/Job/完整性级别语义随 Windows 演进，希望第一时间看到漂移）。
- **macOS 依赖已弃用的 `sandbox-exec`。** Apple 若移除它，Seatbelt 后端就不可用（会 fail-closed 返回不可用，而不是裸跑）。macOS 真机验证在独立 workflow `macos-sandbox.yml` 里：只由推 `main` 且改动命中沙箱相关路径或手工触发，**tag 推送不触发**，且它不阻塞发布——所以某次发布没有 macOS 真机结论是可能出现的。
- **Landlock 后端是 `partial`。** 它不治理 `chmod`/`chown`/`utimensat`；`read-only` 下唯一的可写位置是 `/dev/null`（没有可写 `/tmp`）；路径规则是并集语义，因此 `WritableRoot.ReadOnlySubpaths`（根内只读子路径）在该后端上无法表达。
- **FreeBSD 及其它平台**只有交叉编译 + `GOOS` 的 `go vet`，没有运行期验证。
- **交互式终端、允许列表里的解释器**不构成隔离（见上文）。
- **默认不脱敏**：见[凭据](#凭据)。

## 行为准则

报告与后续沟通受 [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) 约束。骚扰或行为问题也走私密渠道（上面的 advisory 表单），并标注为 conduct concern；不要发在公开 issue 里。

## 相关文档

- [docs/SANDBOX-OS.md](docs/SANDBOX-OS.md) — OS runner 的 opt-in 接入、平台差异与限制
- [docs/CONFIG.md](docs/CONFIG.md) — 权限四档、两档审批、可写面、`Preflight()` 的配置细节
- [docs/VERSIONING.md](docs/VERSIONING.md) — 支持线与 tag 不可变策略
- [docs/RELEASE.md](docs/RELEASE.md) — annotated tag 与发布校验
- [SUPPORT.md](SUPPORT.md) — 非安全问题的支持渠道
- [CONTRIBUTING.md](CONTRIBUTING.md) — 代码规则与 PR 流程
- [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) — 行为准则