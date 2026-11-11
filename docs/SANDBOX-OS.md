# 可选 OS 沙箱 runner（`sandbox/osrunner`）

> 中文文档以 [README.md](../README.md) 与 [docs/zh/](zh/) 为主；本文件是
> `sandbox/osrunner` 的用户与维护者参考。

## 1. 它解决什么问题

`rig/sandbox` 的 `Policy` 是**进程内围栏**：它约束 Rig 内置工具自己的路径/命令
检查，但 shell 命令仍以宿主进程的权限运行。对不可信模型，真正的边界要么是审批
Gate，要么是 OS 层隔离。

`sandbox/osrunner` 提供**可选的、默认关闭的** OS 层文件效果约束。它只做一件事：
把要 spawn 的命令包装成平台原生受限形式，让写入只能命中策略根与平台临时目录。

它**不是**容器/VM 级隔离：网络、进程可见性、设备访问都不在承诺内。

## 2. 平台矩阵

| 平台 | 机制 | Enforcement | 前置条件 | 备注 |
| --- | --- | --- | --- | --- |
| Linux | bubblewrap `bwrap` | full | 已安装 bwrap | 需要 unprivileged user namespace |
| Linux | 自带 Landlock 启动器 | full / partial | 内核支持 Landlock（ABI 1–5） | 纯 Go、raw syscall；老内核按 ABI 降级为 partial |
| macOS | 系统 `sandbox-exec`（Seatbelt/SBPL） | full | `/usr/bin/sandbox-exec` 可用 | Apple 已弃用该私有工具，未来可能移除 |
| Windows | Write-Restricted 受限令牌 + 目录 ACL | partial | 内置 Win32 API | 只约束写/删；读与网络不受限；hard link / AppContainer ACL 有已知边界 |
| 其他 | — | — | — | fail closed：`sandbox.ErrSandboxUnavailable` |

选择顺序由平台链决定，每个候选做一次**功能探测**（真的跑一次受限命令），第一个
可用者被选中并缓存在 runner 内。没有可用候选时 `New` 返回错误，调用方必须拒绝
执行，而不是裸跑。

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
    )
    tools := full.All(full.AllOptions{
        Policy:        policy,
        Root:          workspaceRoot,
        SandboxRunner: runner, // 注入 bash/pwsh、jobs、terminal、worktree
    })
    _ = tools
}
```

`kit` 门面同样支持 `kit.Config.SandboxRunner`（仅在 `Tools == nil`、使用默认工具集时生效）。

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

- `Confine(argv, policy)` 返回最终应 spawn 的 argv、`Enforcement`（`full` /
  `partial`）与 `DenialSignatures`（stderr 拒绝方言）。
- `policy == nil` → `sandbox.ErrSandboxUnavailable`（fail closed）。
- `ModeDangerFullAccess` → 原样返回 argv（不包装）。
- 任何探测/构造失败 → 错误；调用方必须拒绝执行。
- 一次 `Confine` 只对本次调用有效，没有持久 grant。

## 5. 拒绝识别

工具拿到的子进程 stderr 里如果出现 runner 方言，即视为策略拒绝（而不是命令
失败）。内置方言：

| runner | denial signature |
| --- | --- |
| bwrap | `read-only file system` |
| Landlock | `permission denied` |
| Seatbelt | `operation not permitted` |
| Windows ACL | `access is denied` / `拒绝访问` |

## 6. 自定义 runner

运维可以用 `Config.RunnerCommand` 覆盖平台链（假定它实现 bwrap 兼容 profile），
并必须提供 `RunnerFailureSignatures`：

```go
runner, err := osrunner.New(osrunner.Config{
    RunnerCommand:           []string{"my-sandbox"},
    RunnerFailureSignatures: []string{"my-sandbox: fatal"},
})
```

自定义命令会收到本包的 bwrap profile 参数、`--`，然后是目标 argv；它跳过功能
探测，因此“它诚实实现了 profile”是运维断言，不是本包证明。

## 7. 限制

- **只治理文件效果**：读、网络、进程可见性、设备访问不在承诺内。
- **same-world**：子进程与宿主共享内核与文件系统；要隔离整个环境请用容器/VM。
- **macOS**：依赖已弃用的 `sandbox-exec`；Apple 移除后只能回退到无 runner（fail
  closed）或宿主自备方案。
- **Windows**：`partial`。只约束写/删；读与网络不受限；NTFS hard link 可以别名到
  工作区外；被其他 AppContainer 工具 ACL 过的目录可能不可读。
- **Linux Landlock**：老内核 ABI 只治理它支持的访问类别（`partial`）；自带的
  Landlock 启动器是独立进程，需要在宿主 `main` 里接 `MaybeRunHelper`。
- **默认关闭**：不调用 `New` 就没有任何行为变化。

## 8. 测试策略

- 纯逻辑（profile 构造、argv 拼装、拒绝方言、平台选择）在所有平台单测。
- Linux 的真实 Landlock 强制测试在本机运行；内核不支持时 `t.Skip`，绝不失败。
- macOS/Windows 的运行时测试带 `//go:build` 标签，在对应平台执行；CI 目前只做
  cross-compile + `go vet`（仓库没有 macOS/Windows runner）。
- helper 子进程用测试二进制自身扮演（`TestMain` 调 `MaybeRunHelper`），不依赖
  外部安装。
