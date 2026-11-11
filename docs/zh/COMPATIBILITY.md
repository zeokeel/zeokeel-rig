# 兼容性

从 v0.10.0 起，Rig 有**一个依赖**：[`mvdan.cc/sh/v3`](https://pkg.go.dev/mvdan.cc/sh/v3)，即 `sandbox` 策略层使用的 POSIX shell 解析器。其余全部是 Go 标准库；模块根保持仅标准库，所有构建都无 cgo（`CGO_ENABLED=0`）。

| Rig | Go（最低） | 依赖 |
| --- | --- | --- |
| v0.16.x | 1.27 | mvdan.cc/sh/v3（POSIX shell 解析器）；无 cgo。`v0.16.0` 增加 Windows ConPTY terminal（原生 PowerShell；不可用时回退 pipe） |
| v0.15.x | 1.27 | mvdan.cc/sh/v3（POSIX shell 解析器）；无 cgo。`v0.15.0` 修复 Windows 沙箱临时目录/路径/terminal/job_start；`v0.15.1` 改用 Windows 自带 temp（GetTempPath2/Known Folder）并放行 Windows 非敏感环境变量 |
| v0.14.x | 1.27 | mvdan.cc/sh/v3（POSIX shell 解析器）；无 cgo。`v0.14.0` 增加 opt-in、默认关闭的纯 Go `sandbox/osrunner`（无新依赖、无 cgo）；`v0.14.1` 增加 macOS Seatbelt 拒绝自检 |
| v0.13.x | 1.27 | mvdan.cc/sh/v3（POSIX shell 解析器）；无 cgo |
| v0.12.x | 1.27 | mvdan.cc/sh/v3（POSIX shell 解析器）；无 cgo |
| v0.11.x | 1.27 | mvdan.cc/sh/v3（POSIX shell 解析器）；无 cgo |
| v0.10.x | 1.27 | mvdan.cc/sh/v3（POSIX shell 解析器）；无 cgo。`v0.10.0` 还包含在 v0.11.0 中移除的 opt-in OS 沙箱实验 |
| v0.9.x | 1.27 | 无 |
| v0.8.x | 1.27.3（该 tag 中 patch 锁定） | 无 |
| v0.7.x | 1.27.3（该 tag 中 patch 锁定） | 无 |
| v0.6.x | 1.27.3（该 tag 中 patch 锁定） | 无 |
| v0.5.x | 1.27.3（该 tag 中 patch 锁定） | 无 |
| v0.4.x | 1.27.3（该 tag 中 patch 锁定） | 无 |
| v0.3.x | 1.27.3（该 tag 中 patch 锁定） | 无 |
| v0.2.x | 1.27.3（该 tag 中 patch 锁定） | 无 |
| v0.1.x | 1.27.3（patch 锁定） | zeokeel-kernel（v0.2.0 中移除） |

策略：

- 模块根（`package rig`）是稳定契约面：同一 module major 版本内只做增量变更。v1.0.0 之后的破坏性变更通过新 major 版本的 import path（`github.com/zeokeel/zeokeel-rig/v2`）发布，绝不用版本化子目录。
- 消费方产品应锁定精确的 Rig 版本并有意识地升级；`v0.x` 允许破坏性变更。
- 发布 tag 从 v0.9.1 起为 annotated 且 SSH 签名；`.github/allowed_signers` 列出接受的签名公钥，release 工作流会验证。
- 构建与测试不得依赖 cgo：`CGO_ENABLED=0`。race detector 是 Go 工具链的例外（`go test -race` 需要 cgo），不属于库构建或已发布模块。
- 需要编译期模块装配的产品自行使用 kernel（例如 [zeokeel-kernel](https://github.com/zeokeel/zeokeel-kernel)）；Rig 不依赖也不推荐特定运行时。
