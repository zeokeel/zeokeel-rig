# 可选 OS 沙箱 runner（`sandbox/osrunner`）

> 中文文档以 [README.md](../README.md) 与 [docs/zh/](zh/) 为主；本文件是
> `sandbox/osrunner` 与沙箱策略层的用户与维护者参考。

## 1. 它解决什么问题

`rig/sandbox` 的 `Policy` 是**进程内围栏**：它约束 Rig 内置工具自己的路径/命令
检查，但 shell 命令仍以宿主进程的权限运行。对不可信模型，真正的边界要么是审批
Gate，要么是 OS 层隔离。

`sandbox/osrunner` 提供**可选的、默认关闭的** OS 层文件效果约束。它只做一件事：
把要 spawn 的命令包装成平台原生受限形式，让写入只能命中"有效可写集合"：

```
有效可写集合 = Roots() ∪ AdditionalWritableRoots() ∪ 平台临时目录 − ProtectedPaths()
```

它**不是**容器/VM 级隔离：网络、进程可见性、设备访问都不在承诺内（网络开关是
尽力而为，见 §2 与 §8）。

## 2. 平台矩阵

| 平台 | 机制 | Enforcement | 前置条件 | 备注 |
| --- | --- | --- | --- | --- |
| Linux | bubblewrap `bwrap` | full | 已安装 bwrap | 需要 unprivileged user namespace；`!network` 时 `--unshare-net`；`--cap-drop ALL` |
| Linux | 自带 Landlock 启动器 | **partial** | 内核支持 Landlock（ABI 1–5） | 纯 Go、raw syscall。**Landlock 无法治理 chmod/chown/utimensat/setxattr**，因此恒报 partial |
| macOS | 系统 `sandbox-exec`（Seatbelt/SBPL） | full | `/usr/bin/sandbox-exec` 可用 | Apple 已弃用该私有工具，未来可能移除；allow-default + deny `file-write*` |
| Windows | Write-Restricted 受限令牌 + 目录 ACL | partial | 内置 Win32 API | 只约束写/删；读与网络不受限；`ProtectedPaths` 已强制（保护目录及其既有子树逐对象 deny ACE，读仍允许；见 §8） |
| 其他 | — | — | — | fail closed：`sandbox.ErrSandboxUnavailable` |

**临时目录**：bwrap 给每次执行挂一个**私有 tmpfs `/tmp`**（`--tmpfs /tmp`，不落
宿主），因此 `mktemp`/`git status` 这类命令在 read-only 下也能用。Seatbelt 用的是
另一种办法：`subpath` 白名单**不需要挂载能力**，所以 Seatbelt 的 read-only 也放行
`/dev/null` 与 pty 设备；但**不**放行 `/tmp`（保持"read-only 不写宿主"的承诺——
有的实现会在 read-only 也授 `/private/tmp`、`/private/var/tmp`，本项目不授）。
Landlock 两者都做不到：它只能授予既存路径的访问权，因此 Landlock 的 read-only
**没有**临时目录，需要写临时文件的命令会失败（平台能力差异，不是 bug；`rigdoctor`
会显示每个后端实际的 `read-write` 集合）。

系统临时目录包含 `/tmp` 与 `/var/tmp`（macOS 上还有 `/private/tmp`、
`/private/var/tmp`），都受 `WithSlashTmp` 控制；`$TMPDIR` 指向的自定义目录受
`WithTmpdirEnv` 控制。

`WithSlashTmp(false)` / `WithTmpdirEnv(false)` 分别把字面 `/tmp`（含 macOS 的
`/private/tmp`）与 `$TMPDIR` 移出可写集，用于收紧隔离（两档独立可配：字面临时目录
与 `$TMPDIR` 指向的自定义目录）。

runner 会 exec 一个"沙箱本体"（`bwrap` / `sandbox-exec`）。找到它之后还要确认它
**可信**：候选二进制或其目录 group/world 可写、或候选落在当前工作目录（agent
工作区）之下，一律拒收——从 PATH 里挑一个可被替换的沙箱本体等于把边界交给攻击者。

选择顺序由平台链决定，每个候选做一次**功能探测**（真的跑一次受限命令），第一个
可用者被选中并缓存在 runner 内。探测失败会给出**分类原因**（未安装 / 用户命名空间
被禁 / WSL1 / profile 被拒），全部不可用时这句话会进 `ErrSandboxUnavailable` 的错误
文本，`rigdoctor` 也会显示。macOS 的探测还包含一次**拒绝自检**：在只读 profile
下尝试写工作区外的临时文件，写入成功即判定 Seatbelt 未按预期强制（"API 还在但行为
静默变弱"），runner 报 `sandbox.ErrSandboxUnavailable` 而不是把不可信后端交给工具。

## 3. 快速接入

### 3.1 推荐：一次接好默认工具集

```go
import (
    "os"

    "github.com/zeokeel/zeokeel-rig/sandbox"
    "github.com/zeokeel/zeokeel-rig/sandbox/osrunner"
    "github.com/zeokeel/zeokeel-rig/tools/full"
)

func main() {
    // Linux/Windows 的 argv 前缀式 runner 依赖这个隐藏 helper；
    // macOS 不需要（直接 exec sandbox-exec），调用会返回 false。
    if osrunner.MaybeRunHelper(os.Args) {
        return
    }

    runner, err := osrunner.New(osrunner.Config{})
    if err != nil {
        // 本机无法强制 OS 沙箱：按部署策略拒绝启动，或显式放弃 OS 层
    }

    policy := sandbox.New(
        sandbox.WithRoots(workspaceRoot),
        sandbox.WithMode(sandbox.ModeWorkspaceWrite),
        // 某个工具只认固定路径时，显式放行它自己的目录（而不是放开 $HOME）：
        sandbox.WithWritableRoots(homeDir+"/.some-tool"),
        // 可写根内部仍需只读的路径：
        sandbox.WithProtectedPaths(".git"),
    )
    tools := full.All(full.AllOptions{
        Policy:        policy,
        Root:          workspaceRoot,
        SandboxRunner: runner, // 注入 bash/pwsh、jobs、terminal、worktree
        SandboxGrants: true,   // 追加 sandbox_status / request_sandbox_access
    })
    _ = tools
}
```

`kit` 门面同样支持 `kit.Config.SandboxRunner` 与 `kit.Config.SandboxGrants`（仅在
`Tools == nil`、使用默认工具集时生效）。

### 3.2 手动组装

```go
jobsManager := jobs.NewManager(jobs.WithPolicy(policy), jobs.WithRunner(runner))
terminals := terminal.NewManager(terminal.WithPolicy(policy), terminal.WithRunner(runner))

tools := builtin.NewWithPolicyOptions(policy,
    builtin.WithBackgroundJobs(jobsManager),
    builtin.WithRunner(runner),
)
tools = append(tools, worktree.Tools(policy, worktree.WithRunner(runner))...)
_ = terminals
```

**默认关闭**：`runner` 为 `nil` 时，所有工具的行为与 v0.13.x 完全一致；挂接了
runner 却没有策略时工具 fail closed（不 Confine、不 spawn）。

## 4. 行为契约

- `Confine(argv, policy)` 返回最终应 spawn 的 argv、`Enforcement`
  （`full` / `partial` / **`none`**）与 `DenialSignatures`（stderr 拒绝方言）。
- `EnforcementNone` 表示**没有**被任何 runner 包装（`danger-full-access` 透传），
  调用方不能把它当作受限执行。
- `policy == nil` → `sandbox.ErrSandboxUnavailable`（fail closed）。
- `ModeDangerFullAccess` → 原样返回 argv（不包装）。
- 任何探测/构造失败 → 错误；调用方必须拒绝执行。
- 一次 `Confine` 只对本次调用有效；运行期授权是**策略状态**（见 §6），与 runner
  无关，因此在同一次策略生命周期内对所有后续调用生效。
- 挂接 runner 后，bash/pwsh 的模型可见描述改写为准确的 OS 沙箱边界句；无 runner
  时保留原「not an OS sandbox」句（v0.18.1，审计 RG-05）。
- 所有 shell/terminal/job/worktree 的失败结果都会消费 `DenialSignatures` 渲染
  结构化拒绝归因（v0.20 起，见 §5）。
- profile 由 `BuildProfile(policy)` **单一真源**派生：`Profile{ReadOnly,
  ReadWrite, Protected, Network, PrivateTmp, Dropped}`。四个后端只负责把它渲染成
  各自方言，不再各自解释策略。
- 策略里显式配置但不存在/不可用的追加可写根不会静默生效：它们出现在
  `Profile.Dropped` 与 `Policy.DroppedWritableRoots()` 里，宿主应当报警。

## 4.5 首次运行：缓存目录会被创建

干净机器（新装机、CI 首次跑）上，工具链的缓存与配置目录**本来就不存在**：
`~/.cargo`、`~/.m2`、`~/.gradle`、`~/Library/Developer`、`~/.cache/go-build` 等。
如果沙箱把"不存在的缓存根"当作"不授权"，用户第一次 `cargo build` / `mvn` /
`xcodebuild` 就会在做 `mkdir` 时吃到 `EPERM`——这是首次使用就撞上的缺陷。

**规则：可写的语义 = 可以创建。** 缓存根即使不存在，也在可写面里：

| 后端 | 处理方式 |
| --- | --- |
| Seatbelt（macOS） | 直接渲染 `(allow file-write* (subpath …))`。SBPL 的 `subpath` 规则**按访问时求值**，路径不存在也有效——这是最干净的修法。 |
| bwrap（Linux） | `Confine` 之前 `MkdirAll(path, 0o700)`：`--bind` 要求目标存在。 |
| Landlock（Linux） | 同上：`O_PATH` 规则要求路径存在。 |
| Windows ACL | 同上：ACE 需要一个真实对象。 |

约束：

- **只创建目录，绝不创建文件**（目录是工具自己会写的东西，文件才是用户数据）；
- 权限 `0700`（缓存目录不该被同机其它用户读到）；
- **创建失败不阻断**：有些位置本来就该由用户或工具创建（例如 `~/Library/Developer`
  的父目录可能属于系统），失败项留在 `Profile.MissingToolchainCaches` 里如实上报；
- 创建成功后会从 `MissingToolchainCaches` 移除，避免诊断长期喊狼来了；
- 这些缓存根**不会**进 `Protected`：保护一个不存在的东西没有意义（保护项的语义
  仍是"存在才下发"，两者不要混）。

诊断上的对应关系（`sandbox_status`）：

| 字段 | 含义 |
| --- | --- |
| `toolchainCaches` | 本次**已授权**的缓存根（含尚不存在的：它们会被自动创建） |
| `missingToolchainCaches` | 声明了、但**本次未能授权**（创建失败或后端要求存在），需要宿主决定怎么处理 |
| `maskedSockets` | 被 `/dev/null` 覆盖的宿主守护进程 socket（防御纵深；真正根治靠 seccomp 拦 `connect`） |
| `networkCoverage` | 该后端对 `AllowNetwork=false` 的实际覆盖度（`full` / `none`） |

## 5. 拒绝识别与归因

从 v0.20 起，bash/pwsh、jobs、terminal 与 worktree 消费
`ConfinedCommand.DenialSignatures`：只要**输出**（stdout+stderr 合并）按大小写
不敏感子串匹配到 runner 方言，结果文本末尾就追加结构化归因——**不再要求退出码
非零**（很多 CLI 会把落盘失败包成自己的文案后仍然退出 0）。

```text
[sandbox: denied create to /home/me/.some-tool/creds.json (matched "read-only file system"; mode workspace-write, enforcement full)]
workspace roots: /repo
additional writable roots: (none; only the workspace roots and platform temporary areas are writable)
1. Prefer redirecting the tool's own state into the workspace (config-dir flag, XDG_* / HOME override, or a tool-specific env var) and retry; no grant is needed for that.
2. If the tool only writes to a fixed absolute path, request a writable root for exactly that directory and explain why the tool needs it.
3. Do not repeat the same failing command unchanged; a denial is deterministic under the same policy.
This is an OS sandbox denial, not a tool bug: file effects are confined to the roots above.
```

- 归因是**结构化**的：`sandbox.DetectDenialWithExit` 从常见错误文案里提取被拒路径
  （取不到就留空，绝不猜测）、操作类别（write/create/delete/rename/metadata）、
  模式与退出码，再给出通用处置顺序。它**不硬编码任何具体工具或目录**：通用性来自
  "路径 + 原因"这一结构。
- **退出码参与判定**：`2/126/127`（命令没跑起来、文本来自 shell）直接抑制，避免把
  shell 的 "permission denied" 误报成沙箱拒绝；退出码 `159`（`128+SIGSYS`）即使
  没有任何文案也判定为拒绝并标注 `signal SIGSYS`（Linux seccomp 路径）。PTY 会话
  没有退出码，退化为纯文本判定。
- **审计**：每次识别出的拒绝都会调用 `sandbox.WithDenialSink` / `Policy.SetDenialSink`
  注册的钩子（`tools/full` 与 `kit` 分别有 `SandboxDenialSink` 选项）。钩子拿到
  结构化 `Denial`（模式、enforcement、路径、退出码/信号、可写面），可写进会话
  日志用于统计。注意同一次拒绝可能因多个结果视图（例如 job 的多次读取）重复上报。
- 成功且未命中方言的结果逐字节不变；被模型杀死的任务、`ctx` 取消、无 runner、
  空方言都不追加任何内容。
- 仍属"疑似"：方言子串也可能来自命令自身的普通错误（例如 `cat` 一个讲权限错误
  的文档）。`Note()` 的措辞与下一步提示都在降低误导；最终判断仍属于模型/用户。

内置方言：

| runner | denial signature |
| --- | --- |
| bwrap | `read-only file system` |
| Landlock | `permission denied` |
| Seatbelt | `operation not permitted` |
| Windows ACL | `access is denied` / `拒绝访问` |

## 6. 运行期可写授权

静态配置（`WithWritableRoots`）只能覆盖**部署时就知道**的目录。真实工具经常把
凭据/缓存写在固定路径，而那个路径因工具而异、甚至因版本而异——硬编码任何具体
目录都是错的。因此策略提供一层**路径作用域、会话级**的授权：

```text
模型侧（工具）           宿主侧（审批）
sandbox_status      ──→  rig.Gate（Ask / Review / 自定义）
request_sandbox_access ─→ 批准 → Policy.AddWritableRoot(path, reason)
```

- **`sandbox_status`**（只读）：报告 `mode`、`workspaceRoots`、
  `writableRoots`（追加的可写根）、`configuredWritableRoots`、`tempWritable`、
  `protectedPaths`、`network`、`grants`、`grantBudget`/`grantBudgetLeft`、
  `canRequestWritablePath` 与通用 `guidance`。模型据此知道"地板在哪"。
- **`request_sandbox_access`**：参数只有 `path`（绝对目录）与 `reason`
  （8–500 字符）。它**不自行审批**——`rig.Gate` 在 `Invoke` 之前就完成了审批，
  默认 gate 会拒绝它，因此开启该能力必须显式配置审批（Ask/Review 或自定义）。
- **校验（fail closed）**：必须绝对路径、禁止 `..`、解析符号链接（取最深的已
  存在祖先）、拒绝文件系统根与敏感系统目录（`/etc`、`/usr`、`Windows`……）、
  拒绝 `$HOME` 本身、拒绝"祖先包含工作区根"的过宽请求、拒绝与保护路径重叠、
  拒绝重复申请。目录可以尚不存在，但父目录必须存在且是目录。
- **预算**：默认 `DefaultGrantBudget = 5` 次（`WithGrantBudget` 可调）；重复申请
  不消耗预算；用尽后工具返回明确错误，提示改为调整配置或切换模式。
- **生效范围**：会话级、进程内、立即生效——所有工具在每次调用时重新读取策略，
  因此无需重新探测 runner。**没有撤销 API**：授权在进程结束前一直有效，宿主若
  需要撤销应重建 Policy 与工具。
- **审计**：`sandboxgrant.WithObserver(func(rig.ToolCall, sandbox.Grant, error))`
  每次申请（含被拒、冗余）回调一次；`full.AllOptions.SandboxGrantObserver` 与
  `kit` 侧同名选项可直接接会话日志。

**工作区元数据保护（opt-in）**：`full.AllOptions.ProtectWorkspaceMetadata` /
`kit.Config.ProtectWorkspaceMetadata` 会为每个策略根追加
`sandbox.WorkspaceMetadataPaths(root)` 的结果——`.git`（是文件时连同 `gitdir:`
指针指向的真实目录）、`.aws`、`.codex`。它防止被约束的 agent 改写自己的守卫与
凭据（改 git hooks、改云凭据），但会让工作区里的 `git commit` 之类写操作被拒绝，
所以**默认关闭**。也可用 `Policy.AddProtectedPaths(...)` 在构造后追加。

**设计边界**：授权是"窄口子 + 可解释 + 可审计"，不替代 `danger-full-access`；
模型只能申请，不能批准；被拒绝时不要重复申请同一路径。

### 6.1 推荐的门禁组合

```go
hostGate := gate.AllowThenAsk(
    gate.SandboxReadOnlyTools,                       // read_file/list_dir/glob/grep/sandbox_status
    gate.Ask(host.Asker, gate.WithAskTimeout(10*time.Minute)),
    gate.WithAskPrompt(gate.SandboxGrantPrompt),     // 弹窗里直接显示"要放行哪个目录、为什么"
)
```

- 只读调用不打断人；每一次**扩大可写面**都会展示具体路径与理由。
- `kit.Config.SandboxGrants` 开启后，`ApprovalAsk` 的默认门禁会自动放行
  `sandbox_status`（只读），`request_sandbox_access` 仍然默认被拒——需要宿主显式
  配置 `Gate`（`Ask`/`Review`/自定义）才能批准。
- `ApprovalAuto` + `SandboxGrants` 等于让模型自行扩大可写面，仅建议在完全信任的
  环境里使用。

### 6.2 `run_code`（PTC）也在围栏内

`run_code` 曾在 SDK 里是 OS 沙箱的旁路（直接 spawn 解释器）。现在：

- `ptc.Config` 接受 `Runner` + `Policy`，`kit.Config.PTC` 会自动透传两者；解释器
  进程与 `bash` 一样先经 `Runner.Confine` 包装。
- `Runner` 非 nil 而 `Policy` 为 nil → fail closed（不 spawn）。
- 程序临时文件写在**策略根内**（`<root>/.rig-run-code-*`）：bwrap 的
  workspace-write 会挂私有 tmpfs `/tmp`，宿主 `/tmp` 里的脚本在沙箱内根本不存在。
- 失败结果同样追加结构化沙箱归因（`[sandbox: denied …]`）。

## 7. 自定义 runner

运维可以用 `Config.RunnerCommand` 覆盖平台链（假定它实现 bwrap 兼容 profile），
并必须提供 `RunnerFailureSignatures`：

```go
runner, err := osrunner.New(osrunner.Config{
    RunnerCommand:           []string{"my-sandbox"},
    RunnerFailureSignatures: []string{"my-sandbox: fatal"},
})
```

自定义命令会收到本包的 bwrap profile 参数、`--`，然后是目标 argv；它跳过功能
探测，因此"它诚实实现了 profile"是运维断言，不是本包证明。

## 8. 限制

- **只治理文件效果**：读、进程可见性、设备访问不在承诺内。网络只在 bwrap 上用
  `--unshare-net` 尽力关闭；Landlock/Seatbelt/Windows 的网络语义见各自平台。
- **same-world**：子进程与宿主共享内核与文件系统；要隔离整个环境请用容器/VM。
- **macOS**：依赖已弃用的 `sandbox-exec`；Apple 移除后只能回退到无 runner（fail
  closed）或宿主自备方案。profile 使用 allow-default + deny `file-write*`，因此
  必须额外关闭**能代理文件写的非 `file-write*` 通道**，当前已关：
  `(deny system-fcntl (fcntl-command 80 110))`（F_MAKECOMPRESSED/F_TRANSFEREXTENTS
  可通过只读描述符改文件，连 deny-default 基线都挡不住）、
  `(deny user-preference-write)`（CFPreferences 由 cfprefsd 代写
  `~/Library/Preferences`）。仍未约束的是 `file-mknod`、`file-mount`、`sysctl-write`、
  `ipc-posix-shm-*` 的写、`process-*`（可枚举/干涉同用户进程）与 mach-lookup
  服务面——这些不在"文件效果"承诺内；若需要，只能改成 deny-default 基线，
  代价是维护一份随 macOS 版本漂移的平台白名单，本项目有意不做。
  pty 只放行 `/dev/ptmx` 与 `(regex #"^/dev/ttys[0-9]+$")`，**不**整目录放行
  `/dev/pts`（否则可写同用户任意 pty）。`sandbox-exec` 固定取
  `/usr/bin/sandbox-exec`（回退 `LookPath` 时拒绝 group/world 可写候选）。
- **Windows 进程树**：子进程运行在 **kill-on-close 的 Job** 里，spawn 走
  `CREATE_SUSPENDED → AssignProcessToJobObject → ResumeThread`（消除"创建到分配"
  之间的逃逸窗口）。顶层命令退出后，**先收掉整棵树并等它真的消失**，再恢复
  per-run 形状——否则脱离的后代（`start /b`、`Start-Process`）会在存活时脚下失去
  授权。被收树时 stderr 有可识别文案；看门狗超时返回固定退出码 124。Job 创建/分配
  失败一律 fail closed（不 spawn），因此不支持 Job 的宿主会在 `osrunner.New()`
  阶段失败而不是每条命令随机失败。helper 被宿主强杀时 Job 句柄关闭同样终止整棵树，
  但 per-run 形状要等下一次运行被认领/恢复。
- **Windows**：`partial`。只约束写/删，读与网络不受限。`ProtectedPaths` **已强制**：
  helper 校验每条 `--protect`（必须是已存在的目录，否则 fail closed；授权面之外的
  路径跳过并记一行诊断），然后给保护目录**及其既有子树**的每个对象加一条针对
  capability SID 的继承 deny ACE（拒绝写/删/改元数据，读仍允许）。Windows 的继承
  ACE 不会追认既有子对象，所以必须逐对象加——只加目录 ACE 等于没保护。
  该 deny ACE 作为缓存资产在 helper 退出后仍保留（只对沙箱 capability SID 生效，
  不影响宿主用户）；`Everyone` 的删除子项拒绝与 Low 完整性标签仍然每次运行恢复，
  否则会破坏用户在自己工作区里的删除能力并让后来的文件继承 Low 标签。
- **Linux Landlock**：`partial`。它没有 setattr 类访问权，**无法治理
  chmod/chown/utimensat/setxattr**；read-only 下这些操作仍可作用于可访问文件
  （内容读改仍被拦）。老内核 ABI 只治理它支持的访问类别。
- **Landlock 保护路径的代价**：Landlock 的 path-beneath 规则是**并集**语义（不是
  最深匹配），所以"根 `--rw` + 子树 `--ro`"实际不生效。实现改为把写授权下移
  （根只读 + 已存在子项逐个授权）。副作用：含保护路径的目录**不能新建/删除条目**
  （已存在的子文件仍可写）。默认没有保护路径，因此默认行为不受影响。
- **`WithDeniedGlobs` 只在进程内生效**：`**/.env`、`**/*.pem` 这类 glob 由内置工具
  的路径检查执行，**OS profile 不表达 glob**（bwrap/Landlock/Seatbelt 都能表达
  目录，但 glob 需要"每个匹配项一条规则"或 SBPL regex 翻译）。被 runner 包住的
  `bash` 可以 `cat` 到被隐藏的文件。`rigdoctor` 在配置了 glob 时会打印
  `warning: denied globs are enforced by the in-process policy only …`；需要 OS 层
  强制时请把敏感文件移出可读面，或改用逐路径的 `ProtectedPaths`（只读语义）。
- **保护路径可以指向尚不存在的目录**：`ProtectedPaths` 会保留缺失项（"会话中途
  `git init` 出来的 `.git`"是真实场景）。Seatbelt 的 `subpath` 规则按访问时求值，
  缺失项同样生效；bwrap/Landlock 只能渲染既存路径，缺失项进
  `Profile.MissingProtected`，`rigdoctor` 会以 `⚠ declared but NOT protected` 报出。
- **默认关闭**：不调用 `New` 就没有任何行为变化。

## 8.5 Windows 编码约定（中文路径与中文输出）

Windows 的默认活动代码页不是 UTF-8（简体中文系统是 **936/GBK**），所以"命令跑起来了
但中文是乱码"通常不是沙箱的问题，而是**编码边界**的问题。本项目的约定分三层：

### 8.5.1 路径：全程 UTF-16，不做转码

- Go 的 `os`/`os/exec` 在 Windows 上用 `CreateProcessW` 传 **UTF-16** 命令行，
  `os.Args` 也是 UTF-16 → UTF-8 转换后的结果；本项目**没有任何** `GetCommandLineA`/
  `*A` 系列 ANSI 入口，因此中文路径在 `read_file`/`write_file`/`glob`/`grep`、
  `os.Stat`/`MkdirAll`、以及沙箱 helper 的 ACL 授权（`grantDirectory`）里都是
  原样的 Unicode。
- 因此**不要**为中文路径做"转 GBK"或"URL 转义"之类的补偿——那会引入新的 bug。
- 沙箱 helper 的 argv 来自 `os.Args`（宿主 `main` 最开始的 `MaybeRunHelper`），
  与工具进程同一条 UTF-16 通道。

### 8.5.2 输出：强制 UTF-8（Windows 上的 `pwsh`）

`pwsh` 工具在 **Windows 上**会给用户脚本前置一段 UTF-8 语句（其它平台原样不动）：

```powershell
$__rig_sbx_lec = $global:LASTEXITCODE;
$__rig_sbx_enc = $null;
try { $__rig_sbx_enc = New-Object System.Text.UTF8Encoding($false) } catch { };
if ($__rig_sbx_enc) {
  try { [Console]::OutputEncoding = $__rig_sbx_enc } catch { };
  try { $global:OutputEncoding = $__rig_sbx_enc } catch { } };
try { $null = & chcp.com 65001 } catch { };
$global:LASTEXITCODE = $__rig_sbx_lec;
```

三件事缺一不可：`[Console]::OutputEncoding` 决定 PowerShell **自己写 stdout** 的编码，
`$OutputEncoding` 决定**管道传给原生子进程**的编码，`chcp.com 65001` 把**控制台代码页**
也切到 UTF-8——只设前两项时，`cmd /c echo 中文` 仍按 936 输出、被 PowerShell 误解释。

细节：

- **BOM-free**：用 `New-Object System.Text.UTF8Encoding($false)` 而不是
  `[System.Text.Encoding]::UTF8`。后者在 Windows PowerShell 5.1 上会把 `EF BB BF`
  写进输出开头，污染模型看到的内容；`$false` 关掉前导（且 `New-Object` 写法在 5.1
  与 7+ 上都可用，`::new()` 在 5.1 上不存在）。
- **退出码不变**：`chcp` 会改写 `$LASTEXITCODE`，所以前置语句先保存、末尾恢复；
  "纯 cmdlet 脚本 → 0、原生命令 → 透传它自己的码"这两条语义都保持原样。
- **不破坏语法**：`param(...)`、`using module|namespace|assembly`、`#requires` 只能是
  命令的第一条语句——这类脚本**不插入**前置语句（否则整条命令变成语法错误），
  它们退回下面的 U+FFFD 兜底。
- **只在 Windows 注入**：Unix 上的 `pwsh` 默认就是 UTF-8，注入语句只会污染用户的
  变量与退出码。

后台任务（`job_start`/`run_in_background`）的 **argv** 与前台同源：`pwsh` 工具
把（已带前置语句的）argv 交给任务管理器，因此后台 `pwsh` 也按 UTF-8 输出。但
**后台输出的解码**由任务管理器自己的缓冲负责，目前**还没有**接入 8.5.3 的兜底
（见"已知限制"）。

### 8.5.3 兜底：非 UTF-8 输出 → U+FFFD + 明确告警

如果命令无视代码页、直接写出 GBK 字节（老程序、硬编码 `printf`、`Write-Host` 到
非 UTF-8 流等），我们**不猜编码**、也不做 GBK 转码（仓库只允许一个第三方依赖，而且
猜错会把乱码变成"看起来正常"的错误文本，更难排查）。处理方式：

1. 每段连续非法字节替换成一个 `U+FFFD`（`（替换字符）`）；
2. 结果末尾追加一行：

   ```
   [warning: output was not valid UTF-8; the command may have used a non-UTF-8 console code page]
   ```

3. 按字节截断时尾巴上"半个字符"会被静默丢弃——那是**截断**，不是编码错误，不该
   凭空多一个 `U+FFFD`。

所以"非 UTF-8 输出会变成什么"的答案是：**变成 U+FFFD 并附带告警，原文不保留**。

已知限制：

- **兜底只覆盖"明显坏掉"的那部分**（实测）：真实中文句子的 GBK 字节里，绝大多数
  会被 UTF-8 解码器判为非法（→ U+FFFD + 告警），但有一批字节对**恰好构成合法
  UTF-8**：例如 GBK 的 `一`（`D2 BB`）按 UTF-8 是合法的 `һ`（西里尔字母）。这类
  输出**没有非法字节**，因此既不替换也不告警，结果是"看起来正常"的错误字符。
  这是信息论边界（字节层面无法区分 GBK 文本与恰好长这样的 UTF-8 文本），所以
  **强制 UTF-8 必须在源头做**（8.5.2），兜底不能替代它；
- 老程序忽略活动代码页（例如直接 `fwrite` GBK 字节）时只能得到 U+FFFD；
- 控制台代码页是**进程级**状态：`chcp.com 65001` 对**已经启动**的父进程无影响，
  只影响当前 PowerShell 会话及其子进程；
- **后台任务输出**（`job_output`/`job_wait`）目前仍是"原始字节 → 字符串"，没有
  UTF-8 兜底：非 UTF-8 的后台输出会以无效字节形式进入结果。前台 `bash`/`pwsh`
  已覆盖（见 8.5.3）。补齐方式是让任务管理器的读出口复用同一个解码函数。
- 编码强制只在 `pwsh` 工具的 argv 上做：`bash` 工具在 Windows 上本来就直接拒绝
  （"use the pwsh tool"），所以没有 cmd.exe / git-bash 的注入点；由任务管理器
  直接用 `cmd.exe` 起的后台任务需要宿主自己切代码页（`chcp 65001`）。
- `tools/terminal` 的 PTY 会话有独立的会话模型，不在本约定覆盖范围内。

## 9. 诊断（`cmd/rigdoctor`）

出问题时先看实际生效的 profile，不要凭猜测改策略：

```bash
go run ./cmd/rigdoctor -root .                       # 三个模式各自的可写面与 argv 前缀
go run ./cmd/rigdoctor -root . -protect .git -json   # 机器可读
go run ./cmd/rigdoctor -root . -writable ~/.some-tool -mode workspace-write
```

它输出：选中的 runner（或用不了的错误）、workspace/追加可写根/保护路径、临时目录
开关、授权预算，以及每个模式的 `Enforcement`、`read-only`/`read-write` 集合与
argv 前缀。配置了但被丢弃的可写根会以 `warning:` 行报出来。

## 10. 在 Linux 上验证 Windows 后端

**不要用 Wine。** 实测（Wine 9.0，探针见 `scripts/wine-probe/`）：

| 探针 | Wine 9.0 | Windows 上应为 |
| --- | --- | --- |
| `Get/SetNamedSecurityInfoW(LABEL_SECURITY_INFORMATION)` | 失败 | 成功 |
| `CreateRestrictedToken(WRITE_RESTRICTED)` | 返回成功 | 成功 |
| `SetTokenInformation(TokenIntegrityLevel=Low)` | 返回成功 | 成功 |
| **受限令牌（restricting SID 只留 Everyone）写文件** | **成功** | **必须失败** |
| `AdjustTokenPrivileges` | 失败 | 成功 |
| `wine icacls` | 静默无输出、无 ACL 持久化 | 正常 |

Wine 让 API "返回成功"，但 restricting SID 交集、Low 完整性、ACL 继承/传播都不
生效——在它上面跑出"通过"是**假阳性**。正确做法：

1. **真实 Windows**：CI 的 `windows-sandbox` job（GitHub `windows-latest`）跑
   `RIG_WINDOWS_SANDBOX_TEST=1 go test ./sandbox/osrunner -run TestWindowsRuntime`；
2. **纯逻辑**：`windows_plan.go`/`windows_protect.go`/`windows_layout.go`/
   `windows_acl_state.go` 的参数解析、掩码、SID 派生、状态机、ACE 字节解析在
   任意平台都能真跑（`go test ./sandbox/osrunner`）；
3. **手工核对**：§8 的 `icacls` / `(Get-Acl).Sddl` 清单。

## 11. 测试策略

- 纯逻辑（profile 构造、`BuildProfile` 三模式 × 四后端渲染、argv 拼装、拒绝方言、
  平台选择）在所有平台单测。
- Linux 的真实强制测试在本机运行（bwrap 与 Landlock 各自）：read-only 宿主写拒绝
  / 私有 tmpfs / `$TMPDIR` 绑定 / 保护路径 / Landlock 元数据不受治理；内核不支持
  时 `t.Skip`，绝不失败。
- macOS/Windows 的运行时测试带 `//go:build` 标签，在对应平台执行；CI 目前只做
  cross-compile + `go vet`（仓库没有 macOS/Windows runner）。
- helper 子进程用测试二进制自身扮演（`TestMain` 调 `MaybeRunHelper`），不依赖
  外部安装。