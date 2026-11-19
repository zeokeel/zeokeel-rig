# OS 级沙箱（`sandbox/osrunner`）

本文解决什么问题：**"进程内策略"和"OS 级文件效果约束"差在哪，我该不该接、怎么接、
接上之后哪些东西还是不管？** 读者是准备给 Rig 宿主加系统级围栏的开发者，以及排查
"为什么这条命令在沙箱里跑不起来"的维护者。

一句话：`sandbox.Policy` 管的是**Rig 自己的工具**不去碰不该碰的东西；`osrunner`
管的是**被 spawn 的进程**即使想碰也碰不到。前者是配合，后者是强制。

## 1. 它解决什么问题

`sandbox` 包的 `Policy` 是**进程内围栏**：它约束 Rig 内置工具的路径检查与命令
检查。但 `bash` 里那句 `cat ~/.ssh/id_ed25519` 是**子进程**自己做的系统调用，进程内
检查管不到它——命令以宿主进程的权限运行，能读到什么由内核说了算。

`sandbox/osrunner` 提供**可选的、默认关闭的** OS 层文件效果约束。它只做一件事：把
要 spawn 的命令包装成平台原生的受限形式，让写入只能命中"有效可写集合"：

```text
有效可写集合 = Roots() ∪ AdditionalWritableRoots() ∪ 平台临时目录 − ProtectedPaths()
```

三个必须记住的性质：

- **默认关闭**：不调用 `osrunner.New` 就没有任何行为变化；
- **fail closed**：`Confine` 失败就不执行，绝不退化成"裸跑"；
- **只治理文件效果**：网络、读、进程可见性都不在承诺内（§5）。

## 2. 平台矩阵

| 平台 | 机制 | `Enforcement` | 前置条件 | 备注 |
| --- | --- | --- | --- | --- |
| Linux | bubblewrap（`bwrap`） | `full` | 已安装 bwrap，且能创建非特权 user namespace | 首选后端；`--unshare-net` + seccomp 关网络；私有 tmpfs `/tmp`；屏蔽宿主守护进程 socket |
| Linux | 内置纯 Go Landlock 启动器 | **`partial`** | 内核支持 Landlock（ABI ≥ 1） | raw syscall，无 cgo；ABI 决定**治理哪些访问类别**（内容写从 ABI 1、REFER/TRUNCATE 从 ABI 3、TCP bind/connect 从 ABI 4），但**整体恒报 partial**——它完全不治理 `chmod`/`chown`/`utimensat`/`setxattr` |
| macOS | 系统 `sandbox-exec`（Seatbelt / SBPL） | `full` | `/usr/bin/sandbox-exec` 可用且能通过探测 | Apple 已弃用该工具但仍可用；profile 是 allow-default + deny `file-write*`，并额外关掉两条"能代理文件写"的旁路 |
| Windows | Write-Restricted 受限令牌 + 目录 ACL 的隐藏 helper | **`partial`** | 内置 Win32 API | **只约束写/删**；读与网络完全不受限 |
| 其他平台 | — | — | — | fail closed：返回包装 `sandbox.ErrSandboxUnavailable` 的错误 |

选择顺序由平台链决定：`Config.RunnerCommand` 覆盖 →（Linux）bwrap → Landlock；
（macOS）Seatbelt；（Windows）Windows ACL。每个候选都做一次**真实功能探测**（不是
"文件存在就算可用"），第一个可用者被选中并**缓存在 runner 里**——`New` 只探测一次，
`Confine` 不再探测。全部不可用时错误里带分类原因，便于排查：

- 没装 bwrap；
- 装了但非特权 user namespace 被禁（容器、加固内核、seccomp/LSM 拦截）；
- WSL1（不支持所需 namespace 特性）；
- macOS 上 `sandbox-exec` 缺失 / 不可执行 / profile 被拒 / 探测超时。

探测还会**拒收不可信的沙箱本体**：候选二进制或其目录 group/world 可写、或候选落在
当前工作目录（很可能就是 agent 工作区）之下时一律不信任——从 `PATH` 里挑一个能被
替换的"沙箱本体"，等于把边界交给攻击者。macOS 优先固定 `/usr/bin/sandbox-exec`
（只有 root 可写），只有在它不存在时才回退 `LookPath`。macOS 的探测还包含一次**拒绝
自检**：只读 profile 下尝试写工作区外的临时文件，若竟然成功则判定 Seatbelt 未按预期
强制（"API 还在、行为静默变弱"），直接报 `sandbox.ErrSandboxUnavailable`。

## 3. 接入

### 3.1 三步（宿主的 `main`）

```go
import (
    "github.com/zeokeel/zeokeel-rig/sandbox/osrunner"
    "github.com/zeokeel/zeokeel-rig/tools"
    "github.com/zeokeel/zeokeel-rig/tools/full"
)

func main() {
    // ① 必须是第一条语句：Linux/Windows 的 runner 用"argv 前缀 + 隐藏 helper"
    // 的方式执行受限命令，helper 参数会出现在 os.Args 里。
    if osrunner.MaybeRunHelper(os.Args) {
        return // helper 已经执行并退出
    }

    // ② 探测并选择本机可用后端（可能返回 sandbox.ErrSandboxUnavailable）
    runner, err := osrunner.New(osrunner.Config{})
    if err != nil {
        // 本机无法强制 OS 沙箱：按部署策略拒绝启动，或显式放弃 OS 层。
        // 想把这个原因显示给用户，就把它填进 kit.Config.SandboxUnavailableReason。
    }

    // ③ 注入。NewRuntime 会交出后台句柄，宿主关窗口时才能一起收掉。
    rt, err := full.NewRuntime(full.AllOptions{
        Policy:        policy,
        Root:          root,
        SandboxRunner: runner, // 注入 bash/pwsh、jobs、terminal、worktree
        SandboxGrants: true,   // 可选：追加 sandbox_status / request_sandbox_access
    })
    if err != nil {
        return
    }
    defer rt.Close() // 关掉后台 bash 与 PTY 会话

    reg := tools.NewRegistry(rt.Tools...)
    _ = reg
}
```

`osrunner.MaybeRunHelper` 必须在**任何 flag 解析或服务初始化之前**调用：helper 的
参数是 argv 前缀，不是应用参数。macOS 不需要 helper（直接 exec `sandbox-exec`），该
调用返回 `false`；不支持的后端上同样返回 `false`。

用 `kit` 门面时更简单：`kit.Config{SandboxRunner: runner}`（**仅在 `Tools == nil`、
即使用默认工具集时生效**；`Tools != nil` 与 `SandboxRunner` 同时出现是构造期错误，
因为自建工具集不会经过包装、而用户会以为"还在沙箱里"）。`kit` 还会顺带把逐命令提权
的审批通道接好（见 [CONFIG.md](CONFIG.md) §5）。

### 3.2 手动组装

```go
jobsManager := jobs.NewManager(jobs.WithPolicy(policy), jobs.WithRunner(runner))
terminals := terminal.NewManager(terminal.WithPolicy(policy), terminal.WithRunner(runner))

shellTools := builtin.NewWithPolicyOptions(policy,
    builtin.WithBackgroundJobs(jobsManager),
    builtin.WithRunner(runner),
)
worktreeTools := worktree.Tools(policy, worktree.WithRunner(runner))
```

四个 `WithRunner` 分别覆盖：`tools/builtin`（`bash`/`pwsh`）、`tools/jobs`（后台
任务）、`tools/terminal`（持久 PTY）、`tools/worktree`（git worktree 命令）。`run_code`
（PTC）走 `ptc.Config{Runner, Policy}`，`kit.Config.PTC` 会自动透传——解释器进程与
`bash` 一样先经 `Confine`，不是旁路。

### 3.3 自定义 runner

运维环境已有 bwrap 兼容包装器时可以覆盖平台链（跳过功能探测）：

```go
runner, err := osrunner.New(osrunner.Config{
    RunnerCommand:           []string{"my-sandbox"},
    RunnerFailureSignatures: []string{"my-sandbox: fatal"},
})
```

`RunnerCommand` 必须同时提供至少一条 `RunnerFailureSignatures`（runner 自身失败时
stderr 里的大小写不敏感子串）；它会收到本包的 bwrap profile 参数、`--`，然后是目标
argv。`Config.ProbeTimeout` 调整每个候选的探测超时（默认
`osrunner.DefaultProbeTimeout` = 5s）。

## 4. 行为契约与 fail-closed

`sandbox.Runner` 只有一个方法：

```go
Confine(argv []string, policy *sandbox.Policy) (sandbox.ConfinedCommand, error)
```

返回值 `ConfinedCommand` 含 `Argv`（真正要 spawn 的 argv，前缀式后端已把 runner 与
profile 参数拼好）、`Enforcement`（`full`/`partial`/`none`）、`DenialSignatures`
（stderr 里表示"策略拒绝"的方言子串，用于拒绝归因）。

fail-closed 的四条具体含义：

1. **`Confine` 失败 → 拒绝执行。** 工具返回可读错误，绝不启动未受限进程，也不会
   "先跑起来再补约束"。
2. **没有策略 → `sandbox.ErrSandboxUnavailable`。** 没有策略就没有可强制的效果集；
   实现按约定返回这个哨兵错误而不是裸跑。
3. **`danger-full-access` → 原样透传，`Enforcement` 为 `none`。** 四个后端都是这个
   行为（没有可强制的效果集）；这不是"受限执行"，调用方（与 `sandbox_status`）必须
   如实报告。
4. **保护路径缺失 → 拒绝执行**（`osrunner.ErrMissingProtected`）。策略声明了保护，
   但那个位置此刻还不存在（典型：工作区还没 `git init`）时，bwrap / Landlock /
   Windows ACL **无法**渲染"不存在的路径"，继续执行会让沙箱里新建出来的 `.git`
   在本次运行里完全不受保护——"工具说保护了"与事实不符，属于保护承诺失效。

关于第 4 条的两个细节：

- 错误同时满足 `errors.Is(err, osrunner.ErrMissingProtected)` 与
  `errors.Is(err, sandbox.ErrSandboxUnavailable)`（后者便于调用方按"沙箱不可用"
  统一处理）；错误文案里列出具体路径，并说明"继续执行会让它们不受保护"。
- 只对**显式声明**的保护路径（`WithProtectedPaths`/`AddProtectedPaths` 与整块
  声明的 git 元数据目录）硬失败；git 控制面里的 `config.worktree`、`info/` 这类
  "尽力而为的子路径提示"缺失时**只报告**——正常仓库里它们本来就缺，算成硬失败会让
  每条命令都跑不起来。
- `sandbox.WithProtectMissingPaths(false)` 把第 4 条降级为"只报告"：命令照常执行，
  缺失项进 `Profile.MissingProtected` 与诊断。适合"我知道工作区还没有 `.git`"的宿主。
- **Seatbelt 不受这条影响**：SBPL 的 `subpath` 规则按访问时求值，不存在的路径也能
  渲染成 deny 规则，因此它既不需要 fail closed，也不需要放行。

`Profile`（`osrunner.BuildProfile(policy)` 的产物）是四个后端的**单一真源**：
`ReadOnly` / `ReadWrite` / `Protected` / `Network` / `PrivateTmp` / `Dropped`（以及
`MissingProtected`、`ToolchainCaches`、`MaskedSockets` 等诊断字段）。后端只负责把它
渲染成各自方言，不再各自解释一遍"策略等于什么"。

**首次运行**：可写的语义是"可以创建"，所以 `~/.cache/go-build`、`~/.cargo` 这类
工具链缓存根即使不存在也在可写面里（`Confine` 前按需 `MkdirAll(…, 0o700)`：bwrap 的
`--bind`、Landlock 的 `O_PATH`、Windows 的 ACE 都要求路径真实存在）。只创建目录、
不创建文件；创建失败不阻断（项留在 `MissingToolchainCaches` 里如实上报）。这些缓存根
是 **runner 级追加**，不写进 `Policy.WritableRoots`——它们是给子进程的构建缓存，不是
agent 的文件写入面。

## 5. 能力与限制

| 维度 | 承诺 |
| --- | --- |
| 文件写入/删除 | **管**（按有效可写集合；Windows 只到写/删） |
| 文件读取 | **不管**（所有后端）——`~/.ssh`、`~/.aws`、浏览器 cookie 在沙箱内仍可读 |
| 网络 | 部分管：bwrap 用 `--unshare-net` + seccomp（`connect`/`bind`/`listen`/`accept` 返回 `EPERM`）完全切断；Seatbelt `(deny network*)`；Landlock 从 ABI 4 起治理 TCP bind/connect；**Windows 完全不管** |
| 进程可见性 / 信号 | 不管（Seatbelt 的 `process-*` 面不在承诺内） |
| 设备访问 | 部分管（各后端差异大，`rigdoctor` 会显示实际集合） |
| 元数据（`chmod`/`chown`/`utimensat`/`setxattr`） | Linux 上 **Landlock 不管**（bwrap 通过挂载命名空间管） |
| 容器/VM 级隔离 | **不做**。这是 same-world 约束：子进程与宿主共享内核与文件系统 |

所以：**不可信模型要放进容器或 VM**。`osrunner` 的价值是"本地 agent 不会因为一条
命令就改坏工作区之外的东西"，不是"跑不可信代码"。

另外三条容易误解的边界：

- `WithDeniedGlobs`（`**/.env`、`**/*.pem`）**只在进程内生效**：OS profile 不表达
  glob，被 runner 包住的 `bash` 仍能 `cat` 到匹配文件。需要 OS 层强制时，把敏感文件
  移出可读面，或用逐路径的 `ProtectedPaths`（只读语义）。
- Landlock 的保护路径代价：它的 path-beneath 规则是**并集**语义（不是最深匹配），
  所以"根 `--rw` + 子树 `--ro`"不生效；实现改为把写授权下移到保护链的兄弟项（含保护
  路径的目录本身只读，其已存在子项逐个 `--rw`）。副作用是**不能在那种目录里新建/删除
  条目**（已存在的直接子文件仍可写）。
- Windows 的 `ProtectedPaths` 是**强制**的：helper 给保护目录及其既有子树的每个对象
  加一条针对 capability SID 的继承 deny ACE（拒绝写/删/改元数据，**读仍允许**）。

## 6. 与审批的关系

沙箱与审批是两种**独立**的成本，Rig 让它们各管一段：

- **沙箱内的非危险 shell 命令免审批**：`kit.Config.AutoAllowSandboxedCommands`
  （`nil` = 开）。判据全部满足才免问——工具是 shell 类、`SandboxRunner` 非 nil 且
  自报能强制、策略不是 `danger-full-access`、命令不是 dangerous。免问仍写一条
  `approval.response`（decision=allow、reason=`sandboxed`），UI 与审计都能看到
  "这条没问是因为已隔离"。
- **危险命令永远直接拒绝**：`rm -rf /`、`mkfs`、`curl … | sh` 与本开关无关，在最外层
  的 `gate.CommandRiskGate` 就被拒，规则 id 与理由回给模型。
- **文件工具照旧问人**：`write_file`/`edit_file`/`apply_patch` 不经 OS runner，谈
  不上"已隔离"，因此没有免问这回事。
- **headless 宿主受益最大**：没有 `Asker` 时，已沙箱化的命令现在能跑（此前一律
  "没有可用的审批通道"）。
- **逐命令提权**（`sandbox_permissions:"require_escalated"` + `justification`）是
  反方向的窄口子：人工批准后让**这一条**命令不被 runner 包装。策略、危险命令判定、
  workdir、超时、输出上限全部不变。详见 [CONFIG.md](CONFIG.md) §5。

### 6.1 运行期可写授权（`request_sandbox_access`）

静态配置（`WithWritableRoots`）只能覆盖部署时就知道的目录；真实工具常把凭据/缓存
写在某个固定路径，而那个路径因工具甚至因版本而异。策略因此提供一层**路径作用域、
会话级**的授权：

- **`sandbox_status`**（只读诊断）：报告模式、工作区根、追加可写根、临时目录开关、
  保护路径、网络、已批准的授权与剩余预算，以及 `osrunner.DescribeRuntime` 给的
  运行时视图（工具链缓存、被屏蔽的 socket、网络覆盖度、没生效的保护路径）。
- **`request_sandbox_access`**：参数是 `path`（绝对目录）与 `reason`（8–500 字符，
  展示给审批人并进审计）。它**不自行审批**——审批在调用之前由宿主的 `rig.Gate`
  完成，默认门禁会拒绝它；开启这项能力必须显式配审批（`Ask`/`Review`/自定义）。
- **校验 fail closed**：必须绝对路径、禁止 `..`、解析符号链接（取最深的已存在
  祖先）、拒绝文件系统根与敏感系统目录、拒绝 `$HOME` 本身、拒绝过宽的祖先、
  拒绝与保护路径重叠、拒绝重复申请；目录可以尚不存在，但父目录必须是目录。
- **预算**：默认 `sandbox.DefaultGrantBudget` = 5 次（`sandbox.WithGrantBudget`
  可调）；重复申请不消耗预算。
- **生效范围**：会话级、进程内、立即生效（工具每次调用都重新读策略），因此不需要
  重新探测 runner。**没有撤销 API**——需要撤销就重建 `Policy` 与工具。
- **审计**：`sandboxgrant.WithObserver(func(rig.ToolCall, sandbox.Grant, error))`
  每次申请（含被拒、冗余）回调一次；`full.AllOptions.SandboxGrantObserver` 可直接接
  会话日志。

在 `kit` 里这项能力由 `Config.SandboxGrants`（`nil` = 开）控制：两个工具都注册，但
**都是 discoverable、不进初始 active 集**，默认门禁也仍然拒绝
`request_sandbox_access`——**模型只能申请，不能批准**。

## 7. 未隔离时 `Preflight` 说什么

`Agent.Preflight()`（`kit` 侧）与 `rigdoctor sandbox` 共用 `sandbox.Preflight`
的措辞，因此 UI 与模型不会各说一套。语义（不是逐字文案）：

| 状态 | 结论 | 附带说明 |
| --- | --- | --- |
| `danger-full-access` | 未隔离：可以改任何文件、执行任何命令 | 危险命令仍会被拦，但这不等于安全；文件围栏与命令白名单都不生效 |
| 有 runner 但当前档位没启用它 | 未隔离：检测到了系统沙箱，但它没覆盖当前模式 | 只有应用层校验 |
| 没有 runner（这台机器没有可用后端） | 未隔离：只有应用层校验 | 应用层校验只约束配合的模型，挡不住真的去碰；Linux 上的治法通常是装 bubblewrap，macOS/Windows 自带 |
| `Enforcement == full` | 已隔离：只能改工作区（以及系统临时目录） | 这个后端不管读：agent 读得到的凭据都算它读得到 |
| `Enforcement == partial` | 部分隔离：越界的写入与删除被挡住 | 读与网络没有限制，别把凭据放在 agent 能读的地方 |

网络状态单列一句（"不能联网"/"只能连本机"/"可以联网"/"没有系统级网络围栏"），
因为"为什么拉不到包"最容易被误判。`NextSteps` 只在真的存在"被沙箱挡住"这回事时
才说"点『允许这一次』"；没有围栏时会明确说"不会有『被沙箱挡住』这回事，挡住你的
只有应用层校验"。

`SandboxUnavailableReason` 是把"为什么没有沙箱"接进这份报告的唯一入口：`kit` 不
探测平台（保持纯装配层），宿主在 `osrunner.New` 失败时把错误填进
`kit.Config.SandboxUnavailableReason` 即可。若实际挂了 runner，这句话会被忽略。

## 8. 诊断（`cmd/rigdoctor`）

出问题时先看**实际生效的 profile**，不要凭猜测改策略。

```bash
# 给实现者看：每个后端/模式展开的可写面、保护路径、argv 前缀
go run ./cmd/rigdoctor -root .
go run ./cmd/rigdoctor -root . -mode workspace-write
go run ./cmd/rigdoctor -root . -writable ~/.some-tool -protect .git
go run ./cmd/rigdoctor -root . -deny '**/.env'
go run ./cmd/rigdoctor -root . -json

# 给用户看：我现在安全吗 / 能做什么 / 被挡了怎么办
go run ./cmd/rigdoctor sandbox -root .
go run ./cmd/rigdoctor sandbox -root . -asker=yes
go run ./cmd/rigdoctor sandbox -root . -json
```

默认子命令输出：平台、选中的 runner（或用不了的错误）、workspace 根、追加可写根、
保护路径、`/tmp` 与 `$TMPDIR` 是否可写、是否允许网络、授权预算，以及**每个模式**的
`enforcement`、`read-only` / `read-write` 集合、保护项与 `argv` 前缀。下面几类问题会
以 `warning:` / `⚠` 单独报出来：

- 配置了但**被丢弃**的追加可写根（不存在或无法解析）——宿主的明确意图被忽略时不能
  静默；
- 声明了但**没有生效**的保护路径（`declared but NOT protected: path does not exist`）；
- 配置了 denied glob 时会提醒"这只由进程内策略执行，OS profile 不表达 glob"。

`rigdoctor sandbox` 的输出是 `sandbox.Preflight` 渲染的一屏人话（结论 + 需要知道 +
下一步），`-asker=auto|yes|no` 指定"应用里有没有审批通道"（默认 `auto`：没有可靠
探测手段，按保守假设"没有"并标注这一点；库本身不读 `RIGDOCTOR_ASKER`，那是 CLI 的
环境变量）。它的退出码**总是 0**：这是告知命令而不是门禁——大多数开发机默认没装
bubblewrap，把"没有系统沙箱"变成非零退出会让人以为坏了。

## 9. 测试与验证边界

- **纯逻辑**（profile 构造、`BuildProfile` 三模式 × 四后端渲染、argv 拼装、拒绝方言、
  平台选择、Windows ACL 的掩码/SID 派生/状态机/ACE 字节解析）在所有平台都能单测。
- **Linux 的真实强制**在本机跑（bwrap 与 Landlock 各自）：宿主写拒绝、私有 tmpfs、
  `$TMPDIR` 绑定、保护路径、Landlock 元数据不受治理；内核不支持时 `t.Skip`，不失败。
- **Windows**：CI 的 `windows-sandbox` job 在真实 Windows runner 上跑受限令牌 + ACL
  的运行时测试（阻塞门禁）。**不要用 Wine 验证**——Wine 让 API "返回成功"但不强制
  restricting SID / 完整性级别 / ACL 继承，在它上面跑出"通过"是假阳性。
- **macOS**：真机验证在独立 workflow（`macos-sandbox.yml`），不阻塞 main 与发布；
  `ci.yml` 里对 darwin 做交叉编译 + `go vet`。

## 10. 相关文档

- [CONFIG.md](CONFIG.md) —— `kit.Config` 全字段（含 `SandboxRunner`、
  `SandboxGrants`、`AutoAllowSandboxedCommands`）、审批矩阵、逐命令提权、Preflight
- [TOOLS.md](TOOLS.md) —— `bash`/`pwsh`/`job_*`/`terminal_*`/`worktree` 的权限与
  平台影响
- [ARCHITECTURE.md](ARCHITECTURE.md) —— 分层与"策略层不得依赖平台代码"的边界
- [SECURITY.md](../SECURITY.md) —— 威胁模型与报告渠道
- [COMPATIBILITY.md](../COMPATIBILITY.md) —— 平台支持矩阵与 Go/依赖承诺