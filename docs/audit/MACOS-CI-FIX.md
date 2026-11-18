# macOS 真机 CI 失败：诊断与修复

- **触发**：GitHub `macos-latest` 上**第一次**运行 `go test ./sandbox/osrunner`（此前该文件的 darwin 分支从未在真机执行）
- **范围**：`sandbox/osrunner/darwin*.go` 与对应测试；未触碰 `landlock*_test.go` / `bwrap*_test.go` / `profiles_test.go` / `darwin_pure_test.go`
- **状态**：① 真 bug（已修）② 测试假设（已修）③ 环境差异 + 测试口径（已修）；另发现 1 个**跨平台产品缺口**（未修，diff 附后）

---

## 0. 结论：我们的 macOS 后端在真机上**可用**

CI 那条错误本身就是证据链（逐条拆）：

```
New: osrunner: sandbox-exec is not usable:
  sandbox-exec probe failed: exit status 71
  (sandbox-exec: execvp() of 'true' failed: No such file or directory)
```

| 观察 | 推论 |
| --- | --- |
| 错误来自 `sandbox-exec` **自己**（带 `sandbox-exec:` 前缀），不是我们的 `trustedExecutable`/`LookPath` 报的"not found" | 说明 `/usr/bin/sandbox-exec` 在 runner 上**存在且可执行**，被 `seatbeltExecutable()` 正常选中 |
| `sandbox-exec` 已经运行到 `execvp()` **子进程**这一步 | 说明我们内联传入的 `-p <profile>` **被成功解析并应用**（SBPL 语法错误会在 fork 之前报 `syntax error`，退出码与文案都不同）→ **渲染出的 profile 在真机上是合法的** |
| 失败点是 `execvp() of 'true'` | 子进程名是裸 `"true"`，靠 PATH 查找；而该测试把 `PATH` 改成了只剩假二进制的临时目录 → 找不到 → **是探测自身的 bug，不是这台机器不能用 Seatbelt** |

**所以：Seatbelt 后端在 GitHub 的 macOS runner 上是可用的，红的是我们的探测与测试。**
修复后我加了 `TestSeatbeltAvailabilityOnHost` 来机器化地证明这一点（见 §2）。**真机执行仍需你们再跑一次 CI 确认**——我在本机（Linux）能证明的到此为止。

---

## 1. 三个失败的分类

| # | 现象 | 分类 | 修法 |
| --- | --- | --- | --- |
| ① | `sandbox-exec: execvp() of 'true' failed` | **真 bug**（探测依赖 PATH）+ **测试设计 bug**（用 PATH 注入假二进制，真机上永远被系统路径压过） | 探测子进程改**绝对路径**；把"选哪个 sandbox-exec"变成**可注入的解析点**，测试不再靠改 PATH 生效 |
| ② | `TestSeatbeltProtectedDenyComesLast: 工作区没有出现在可写白名单里` | **测试假设** | 断言改用**规范化后**的路径（`EvalSymlinks`）；**没有**放宽实现 |
| ③ | 工具链覆盖清单里 `~/.cache/go-build`、`~/.cargo`… 全是"缺口" | **环境差异** + **测试口径错误** | 干净 runner 根本没装这些工具（那是环境）；同时我的用例硬编码了 Linux 路径（macOS 的 GOCACHE 其实是 `~/Library/Caches/go-build`）；改为**只对"存在却没被授权"失败** |

---

## 2. 具体改动

### 2.1 `darwin.go` / `platform_darwin.go`（修 ① 的实因）

```go
// platform_darwin.go（darwin-only，故放在这里，避免 Linux 构建下被判 U1000）
var sandboxExecSystemPath = "/usr/bin/sandbox-exec"   // 测试可注入
var sandboxExecLookPath = exec.LookPath               // 测试可注入
```

```go
// darwin.go
// seatbeltProbeCommand 是功能探测要启动的子进程 argv（紧跟 `--` 之后）。
// **必须是绝对路径**：`sandbox-exec -p <profile> -- true` 里的裸 "true" 要靠
// execvp 查 PATH，而最小环境（CI、被宿主重设过 PATH 的进程）里 PATH 可能不含
// /usr/bin → 探测报 `execvp() of 'true' failed`，把整台机器误判成不可用。
const seatbeltProbeCommand = "/usr/bin/true"

func seatbeltProbeTail() []string {
	if _, err := os.Stat(seatbeltProbeCommand); err == nil {
		return []string{seatbeltProbeCommand}
	}
	return []string{"/bin/sh", "-c", "exit 0"}   // 极简系统兜底，同样绝对路径
}
```

```diff
-	cmd := exec.CommandContext(ctx, execPath, "-p", profile, "--", "true")
+	tail := seatbeltProbeTail()
+	args := append([]string{"-p", profile, "--"}, tail...)
+	cmd := exec.CommandContext(ctx, execPath, args...)
```

`seatbeltExecutable()` 改为读那两个变量（生产路径不变：仍然固定 `/usr/bin/sandbox-exec`）。

**红→绿证据**（在 Linux 上用 `-overlay` 把 `darwin.go` 换回旧版跑同一个用例）：

```
旧版：--- FAIL: TestSeatbeltProbeWorksWithEmptyPath
      sandbox-exec probe failed: exit status 127 (…/sandbox-exec: exec: true: not found)
新版：--- PASS: TestSeatbeltProbeWorksWithEmptyPath
```

这条用例模拟了真机的失败条件（`PATH=""` 且假 sandbox-exec 像真的一样 `execvp` 子进程），所以它是①的**直接回归防线**，而且在 Linux 上就能跑。

### 2.2 `darwin_test.go`（修 ① 的测试设计）

- 假二进制改为通过 `sandboxExecSystemPath` 注入（`useFakeSystemSandboxExec` / `disableSystemSandboxExec`）——**不再**指望改 PATH 就能被选中；
- 新增 `seatbeltFakeExecWorking`：行为正确的假 sandbox-exec（跳过 `-p … --`、`exec "$@"`、自检档模拟 EPERM 非零退出）；
- 新增 `TestNewPlatformRunnerFallsBackToPath`（回退分支仍被测到）；
- 新增 `TestSeatbeltAvailabilityOnHost`：**只对真缺陷失败**——
  先用最小基线 `sandbox-exec -p '(version 1) (allow default)' -- /usr/bin/true` 确认这台机器的 Seatbelt 本身能跑；跑不动 → `Skipf`（环境/系统限制，带完整错误）；基线能跑而 `New()` 失败 → `Fatalf("这是本 SDK 的缺陷")`。

### 2.3 `darwin_toolchain_test.go`（修 ②③）

- 所有断言先 `EvalSymlinks`（`canonicalAuditPath`），修掉 `/var/folders/…` vs `/private/var/folders/…`；
- 工具链清单不再硬编码路径，而是直接遍历实现侧的 `toolchainCacheRoots(policy)`：
  - **存在且未被授权 → `t.Errorf`（真缺陷）**；
  - 不存在 → 只记录"环境：未安装"；
- 新增 `TestSeatbeltGoCacheParentIsWritable`：回答"干净机器上 GOCACHE 目录不存在时 go 还能不能跑"；
- 文件 build tag 从 `darwin` 放宽为 `darwin || linux`：这些是**纯逻辑**断言，Linux 上跑才能拦住回归（这次真机首跑就红，正是因为没有 Linux 侧覆盖）。

---

## 3. ③ 的实测结论：干净机器上 `go build` 还能跑吗

**能（在 macOS 上）**，原因是 macOS 的 GOCACHE 在 `~/Library/Caches/go-build`，而 `~/Library/Caches` **本身存在且已被授权**（真机 CI 日志也显示 `已覆盖: Library/Caches`）：

- `TestSeatbeltGoCacheParentIsWritable`：本机（Linux）`/root/.cache` 存在且已授权 → 通过；macOS 上对应 `~/Library/Caches`，同一条断言成立。

**但发现一个跨平台产品缺口（未修，需你决策）**：

- `toolchainCacheCandidates` 会把**不存在**的缓存根从 `ReadWrite` 里剔除（`os.Stat` 失败 → 进 `MissingToolchainCaches`，只上报不授权）。
- 后果：干净机器上 `~/.cargo`、`~/.m2`、`~/Library/Developer` 都不存在 → 不授权 → 用户第一次跑 `cargo build` / `mvn` / `xcodebuild` 时要**创建**这些目录，而"创建"需要父目录可写 → **EPERM**。
- 修法（最小，且对 Seatbelt 是 3 行）：SBPL 的 `subpath` 规则按**访问时**求值，把 `Profile.MissingToolchainCaches` 也渲染成 allow（`SeatbeltProfile` 对 `MissingProtected` 已经在用这个语义）：

```diff
--- a/sandbox/osrunner/profiles.go
+++ b/sandbox/osrunner/profiles.go
@@ if writable := seatbeltWritableSubpaths(profile.ReadWrite); len(writable) > 0 {
 	if writable := seatbeltWritableSubpaths(profile.ReadWrite); len(writable) > 0 {
 		…(allow file-write* (subpath …))…
 	}
+	// 不存在的缓存根也要放行：SBPL 的 subpath 规则按访问时求值，而"首次使用"
+	// 恰恰要求进程自己创建该目录（父目录也不可写的话就永远建不出来）。
+	if missing := seatbeltWritableSubpaths(profile.MissingToolchainCaches); len(missing) > 0 {
+		parts := make([]string, 0, len(missing))
+		for _, path := range missing {
+			parts = append(parts, "(subpath "+sbplString(path)+")")
+		}
+		forms = append(forms, "(allow file-write* "+strings.Join(parts, " ")+")")
+	}
```

bwrap/Landlock 不能这么做（挂载/`O_PATH` 要求路径存在），它们的等价修法是 **spawn 前 `os.MkdirAll` 这些缓存根**，或把它们记为"已知边界"并让 `sandbox_status` 提示用户先跑一次 `cargo --version` 之类。这条不在本次范围（`profiles.go` 是共享文件），我按约定没有动。

---

## 4. 验证矩阵（本机 Linux 能跑的部分）

```
CGO_ENABLED=0 go build ./...                                    ✅
CGO_ENABLED=0 go vet ./...                                      ✅
gofmt -l .（排除 .audit*）                                       ✅ 空
go test ./sandbox/... -count=1                                  ✅ sandbox + osrunner 全绿
GOOS=darwin GOARCH=arm64/amd64 go build ./...                   ✅
GOOS=darwin GOARCH=arm64 go vet ./...（含 darwin 测试文件）      ✅
staticcheck ./sandbox/osrunner（linux）                          ✅ 我的文件无告警
   （残留 U1000 全部在并行 agent 的 conformance_test.go / pathcompare.go / seccomp_other.go）
go test ./sandbox/osrunner -run 'TestSeatbeltProbe'              ✅ 红→绿见 §2.1
```

**未执行**：真机 macOS 上 `go test ./sandbox/osrunner`（本机是 Linux）。请再跑一次 `macos-latest` 确认。

---

## 5. 仍未消除的风险（如实列出）

1. `sandbox-exec` 已被 Apple 官方标记 deprecated；本次没有证据表明它被进一步限制（见 §0 推论），但这是长期风险，值得在 CI 里保留 `TestSeatbeltAvailabilityOnHost` 作为哨兵。
2. `TestSeatbeltAvailabilityOnHost` 的 skip 分支可能掩盖"所有 Mac 都不可用"的情况（因为它只在基线也失败时 skip）。缓解：skip 时打印完整错误，且 CI 日志里能直接看到 `SKIP` 与原因。
3. ③ 的产品缺口（不存在的缓存根）会让**干净机器上的首次 `cargo`/`mvn`/`xcodebuild`** 失败——这是真实的用户可感知问题，优先级不低。