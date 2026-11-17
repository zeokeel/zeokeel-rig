# Wine 安全探针（负结果的可复现记录）

这个目录回答一个问题：**能不能用 Wine 验证 `sandbox/osrunner` 的 Windows 后端？**

答案：**不能。** 下面是 Wine 9.0 的实测输出（`./run.sh` 可复现）：

| 探针 | Wine 9.0 |
| --- | --- |
| `GetNamedSecurityInfoW(DACL)` | 成功 |
| `GetNamedSecurityInfoW(LABEL_SECURITY_INFORMATION)` | **失败**（低完整性标签不可读） |
| `SetNamedSecurityInfoW(LABEL_SECURITY_INFORMATION)` | **失败** |
| `CreateRestrictedToken(WRITE_RESTRICTED)` | 返回成功 |
| `SetTokenInformation(TokenIntegrityLevel=Low)` | 返回成功 |
| `AdjustTokenPrivileges` | **失败**（`ERROR_NOACCESS`） |
| `CreateJobObjectW` | 成功 |
| `SetEntriesInAclW` | 成功 |
| **受限令牌是否真的限制写？** | **不限制**：restricting SID 只留 `Everyone` 的令牌写入文件**仍然成功**（Windows 上必须失败） |
| `wine icacls` | 静默无输出；宿主目录上没有 ACL 持久化痕迹 |

也就是说：Wine 让所有 API "返回成功"，但 **restricting SID 交集、Low 完整性级别、
ACL 继承/传播这三件本后端赖以成立的事情全都不生效**。在 Wine 上跑出"通过"是
**假阳性**，比不测更危险。

## 正确的验证方式

1. **真实 Windows**：`.github/workflows/ci.yml` 的 `windows-sandbox` job 在
   GitHub 托管的 `windows-latest` 上跑 `RIG_WINDOWS_SANDBOX_TEST=1 go test
   ./sandbox/osrunner -run TestWindowsRuntime`（初始 `continue-on-error`，稳定后
   改为阻塞门禁）。
2. **不依赖内核的纯逻辑**：`windows_plan.go` / `windows_protect.go` /
   `windows_layout.go` / `windows_acl_state.go` 的参数解析、掩码、确定性 SID、
   状态机、ACE 字节解析都在 Linux 上真跑（`go test ./sandbox/osrunner`）。
3. **手工核对清单**：`docs/SANDBOX-OS.md` §8 与审计报告里的 `icacls` /
   `(Get-Acl).Sddl` 核对点。

## 运行

```bash
apt-get install -y --no-install-recommends wine64 wine   # 一次性
./run.sh
```
