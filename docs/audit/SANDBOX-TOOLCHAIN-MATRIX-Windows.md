# Windows 沙箱"开发工具能力对齐"审计（三平台并行 · Windows 侧）

- **快照**：2026-11-18 14:10–14:25（`sandbox/osrunner` 正被另外两个平台的审计并行修改；本报告中 `sandbox/osrunner/windows_*.go` 的行为均按该快照的代码确认）。
- **方法**：本机是 Linux。**代码审读 + 纯逻辑推导 + `GOOS=windows` 交叉编译**；凡需要真机（Windows 内核/令牌/ACL/Job）才能断言的结论，逐条标注 **`未执行`**。
- **产出**：本文 + `sandbox/osrunner/windows_toolchain_test.go`（6 条纯计划层用例：2 条 Finding 红、4 条 characterization/护栏绿）。**未改任何实现**，需要的改动以 §C 的精确 diff 给出。

---

## 0. 一句话结论

**"沙箱一开、正常开发全废"是真的，但根因不是 Windows 特有的路径映射，而是三平台共同的"工具链缓存不在可写面"。Windows 侧多两个独立缺陷：① `buildWindowsConfinedCommand` 把 `policy.AdditionalWritableRoots()` 整类丢弃（Linux/macOS 都会带上），所以宿主/运行期授权在 Windows 上是空操作；② Windows 后端完全不治理网络（Linux 的 `--unshare-net`、macOS 的 `(deny network*)` 在 Windows 没有对应物），因此**同一个 Policy 在 Windows 上更宽**。**

---

## A. Windows 后端逐条

### A1. 可写面到底包括哪些？`%TEMP%` 怎么解析？

| 项 | 结论 | 证据 |
|---|---|---|
| 工作区写根 | `policy.Roots()` 逐个作为 `--workspace` 下发，helper 给每个根加 capability SID 允许 ACE（`OI|CI` 继承）+ Everyone `FILE_DELETE_CHILD` 拒绝 + Low 标签 | `windows_plan.go:369-389`、`windows_helper.go:285-300` |
| **宿主显式追加的可写根** | **被丢弃**。`buildWindowsConfinedCommand` 只传 `policy.Roots()`，从不传 `policy.AdditionalWritableRoots()` | `windows_plan.go:381,386`（对比 `profile.go:129` 的 `workspaceWritableCandidates` = Roots ∪ AdditionalWritableRoots） |
| 运行期授权（`request_sandbox_access` → `AddWritableRoot`） | **同样被丢弃**（它落在 `AdditionalWritableRoots()` 里） | 同上；`sandbox/sandbox.go:512` |
| 私有临时目录 | **每次运行新建** `<base>\rig-sbx-tmp-*`，基址按 `RIG_SANDBOX_TEMP` → `GetTempPath2W`/`GetTempPathW` + `FOLDERID_LocalAppData\Temp` → `os.TempDir()` 顺序挑，剔除 `SystemRoot` 之下的候选，且必须在所有工作区之外 | `windows_helper.go:108-140`、`windows_plan.go:603-660`、`windows_folders.go:75-108` |
| 用户 `%TEMP%` 本身 | **不可写**。子进程的 `TMP`/`TEMP` 被改写为私有目录，`TMPDIR` 被删除 | `windows_helper.go:396-411`（代码审读；**未执行**） |
| `%LOCALAPPDATA%\Temp` 与 `%TEMP%` 不同时 | 两者都只是**私有目录的候选基址**（不授予写），`GetTempPath2W` 优先；两者都不可用时退回 `os.TempDir()`；全不可用 → helper 失败（不 spawn） | `windows_plan.go:615-660` |

**`%TEMP%` 的解析链**：`GetTempPath2W`（Win11/Server2022+ 才导出，先用 `LazyProc.Find` 探测，否则 Windows 10 上会 panic）→ `GetTempPathW` → Known Folder `LocalAppData\Temp` → `os.TempDir()`。**没有**用 `os.TempDir()` 作唯一来源，这比 Linux/macOS 侧更严谨（那边 `platformTempRoots()` 直接用 `os.TempDir()`）。

风险：**P1**（A1 第 2、3 行）。工具链缓存全部在策略根之外，且宿主**没有任何办法**把它们加进去。

### A2. 驱动器/路径形态、大小写、8.3

| 形态 | 现状 | 是否绕过写限制 | 证据 |
|---|---|---|---|
| `C:\w` vs `C:/w` | `winPathKey` 把 `\`→`/` 再折叠大小写，两者同一个键 | 否 | `windows_plan.go:318-341` |
| `c:\w` vs `C:\W` | 折叠（`winPathKey` 显式 `ToLower`；`filepath.Rel` 在 Windows 上 `sameWord`=EqualFold） | 否 | 同上 + `sandbox/pathcompare.go:24-47` |
| `\\?\C:\w` | **不剥离**。`winPathKey` 得到一个不同的键 ⇒ 同一目录会派生**另一个** capability SID（授权不共享，但安全） | 否（对象级 ACE 仍然生效） | `windows_plan.go:318`（用例 `TestAuditWinToolchain_PathFormNormalization` 钉住） |
| `\\?\C:\w` 作为 workspace + 普通写法作 protect | `winPathWithinFold` 前缀不匹配 ⇒ `winResolveProtectedPaths` 判"落在所有授权工作区之外" ⇒ **跳过保护并只记一行 stderr**，`.git` 变成可写（**fail-open**） | **是**（需宿主把 Root 写成 `\\?\` 形式，罕见但真实） | `windows_plan.go:490-528`、`:662-673` |
| UNC `\\server\share` | 被 `windowsWorkspaceRoots` 接受（只 `filepath.Clean`），但 helper 对 UNC 目录 `SetNamedSecurityInfo` 的成功率取决于共享语义；失败即 fail closed（不 spawn） | 不适用（直接跑不起来） | `windows_plan.go:551-575`（**未执行**） |
| 8.3 短名 `PROGRA~1` | **没有任何归一** | **否**——见下 | 无代码路径依赖路径前缀来授权 |

**为什么 8.3/`\\?\`/UNC 不构成"写绕过"**：Windows 后端的写限制是**对象级**的（受限令牌的 restricting SID 必须同时通过目标对象 DACL 的检查），不靠"路径前缀在白名单里"。8.3 短名指向同一个文件对象，ACE 照样生效。路径集合只用于两件事：① 派生 capability SID（写法不同 → 不同 SID → 只是不复用授权）；② `--protect` 归属判定（**这里的 fail-open 才是真问题**，见上表第 4 行）。

风险：**P3**（`\\?\` fail-open；需要宿主主动用长路径前缀才触发），建议在 `winPathKey`/`winPathWithinFold` 里剥掉 `\\?\` 与 `\\?\UNC\`，并对 UNC workspace 报明确错误。

### A3. 只读系统路径够不够？读有没有被限制？

**读完全没有被限制。** Windows 后端只治理写/删（`WRITE_RESTRICTED` + Low IL），读是"默认允许"：

> "只有写/删受治理，读完全不受限" —— `windows_plan.go:31-33`

因此：
- `%SystemRoot%`、`%SystemRoot%\System32`、`%ProgramFiles%`、`%ProgramData%` 的**读**没问题（不需要额外授权）。
- `%USERPROFILE%\.gitconfig`、`.ssh`、`.cargo` 的**读**也没问题（这正是工具链能读到 git 配置、SSH 配置、cargo 配置的原因）。
- 但**写**只限：工作区根（继承）+ 私有临时目录。
- **对比 Linux/macOS**：三平台的"写限制"语义一致（都是"只给显式集合写"），但 Linux bwrap 用 `--ro-bind /` 让**整棵宿主树**变成只读挂载，Windows 是"对象 ACL + 受限令牌"——安全级别接近，**但 Windows 的读面更宽**（没有 mount 命名空间做遮挡，`\\?\GLOBALROOT\Device\...`、`\\.\` 设备路径不受约束）。

风险：**P2**——读泄露面（`.ssh/id_rsa` 可读）在三平台一致，属"同世界约束"，但 Windows 还多了**设备命名空间**入口（`\\.\PhysicalDrive0`、`\\?\GLOBALROOT`），Low IL 不拦。

### A4. 注册表

- **没有任何显式建模**：`sandbox/osrunner` 里没有 HKCU/HKLM 的 ACL 操作（`grep -i registry` 无命中）。
- 写注册表被拒是 **Low 完整性令牌的副作用**：HKCU 的键默认 Medium 标签，Low IL 进程无法写（no-write-up）；这正是上一轮日志里 `HKCU` 写入被拒的原因。
- **读注册表不受影响**：Low IL 读 Medium 对象允许（no-read-up 只对 High/System 生效，且 HKCU/HKLM\SOFTWARE 对 Users 可读）。
- **后果**：注册表写入的拒绝是"顺带的"，不是策略；`request_sandbox_access` 无法为注册表授权（没有对应实现），会话级也无法放开。开发者常见的注册表写（`dotnet` 首次运行、`npm config`、部分安装器）都会失败，且**没有明确文案**（表现为 ACCESS_DENIED）。

风险：**P2**（行为可接受、不可配置、无诊断）。

### A5. 受限令牌 + Low IL 会让哪些正常开发工具挂掉？

令牌形状：`CreateRestrictedToken(WRITE_RESTRICTED | DISABLE_MAX_PRIVILEGE | LUA_TOKEN)`（`windows_token.go:151-175`），restricting 列表 = `[登录 SID, Everyone, 各工作区 capability SID, 临时目录 capability SID]`（`windows_plan.go:241-286`），随后降 Low IL（`windows_token.go:214`）。

| 工具 | 会不会挂 | 原因 |
|---|---|---|
| `go build` / `go test` / `go vet` | **会**（P1） | 需要写 `GOCACHE`（默认 `%LOCALAPPDATA%\go-build`，不在可写面）。热缓存**可能**侥幸命中，但 `go test` 要写出测试二进制、`go` 启动要更新 `trim.txt` ⇒ 迟早 ACCESS_DENIED。`GOTMPDIR`/`TMP` 已指向私有临时目录（这半边是对的） |
| `go mod download` / `go get` | **会** | 要写 `GOMODCACHE`（`%USERPROFILE%\go\pkg\mod`） |
| `link.exe`（Go 的 link 阶段） | 跟随 `go build` | 与 `SeDebugPrivilege` 无关——**不需要**特权；`DISABLE_MAX_PRIVILEGE` 对它无害 |
| `node` / V8 JIT | 不挂 | JIT 只在本进程内 `VirtualAlloc`，IL/privilege 不拦；启动读 `node_modules` 也 OK |
| `npm install` | **会** | `%LOCALAPPDATA%\npm-cache` 写失败（`npm test` 在依赖已就绪时不写缓存，通常能过） |
| `pip install` | **会**（有绕过） | `%LOCALAPPDATA%\pip\Cache`；`--no-cache-dir` 可绕过 |
| `cargo build`（首次/需要索引） | **会** | `%USERPROFILE%\.cargo\registry`、`.cargo\git` |
| `dotnet restore` / `mvn` / `gradle` | **会** | `.nuget\packages`、`.m2\repository`、`.gradle\caches` |
| `git commit/checkout` | **会**（三平台一致） | `.git` 是默认保护路径（写被 deny ACE 钉死） |
| `pwsh` | 可能降级 | PowerShell 首次运行写 `%LOCALAPPDATA%\Microsoft\PowerShell\…` 分析缓存；失败通常只是警告 |
| `cmd /c start`、`pwsh Start-Process` 等脱管后代 | 不挂但**会被杀** | Job 对象 `KILL_ON_JOB_CLOSE` + 先建 suspended 再入 Job（`windows_helper.go` + `windows_job_object.go`） |

**`SeDebugPrivilege`/写 `%SystemRoot%\Temp` 都不是问题**：Go 工具链不需要调试特权；`%SystemRoot%\Temp` 只在 `TEMP` 缺失时才是 `GetTempPath` 的返回值，而 `winNewTempDir` 显式剔除 `Windows` 目录（`windows_helper.go:118-122`）。

风险：**P1**。

### A6. 符号链接 / 联接点（junction）

- **写授权层面**：junction 不是绕过。授权是目标对象上的 ACE；经 junction 写外部目录时目标对象没有 capability SID ⇒ 拒绝。
- **保护遍历层面（真问题）**：`winEnforceProtectTree` 用 `filepath.WalkDir`，只跳过 `fs.ModeSymlink`（`windows_protect.go:289-325`）。
  - 符号链接：被跳过 ✓（用例 `TestAuditWinToolchain_ProtectWalkSkipsSymlinks` 绿，`Skipped=1`）。
  - **目录 junction**：Go 在 Windows 上**不**把 `IO_REPARSE_TAG_MOUNT_POINT` 报成 `ModeSymlink`（`os.FileMode` 只对符号链接 reparse tag 置位），因此 `WalkDir` 会**当普通目录下钻**。若 `.git`（受保护子树）里有一个指向工作区外的 junction，helper 会给**工作区外的真实对象**逐个加 deny ACE——正是该函数注释里"绝不能碰"的情形。对称的 `winRelaxProtectTree` 也会走进同一棵树去撤销，所以不会永久残留，但**运行期间修改了工作区外对象的 DACL**。
  - **`未执行`**：本机 Linux 无法构造 junction，结论来自 Go 的 reparse-tag 语义 + `WalkDir` 实现推导。建议真机验证：`mklink /J C:\ws\.git\esc C:\Windows\System32` 后跑一条命令，检查 `C:\Windows\System32` 的 DACL 是否被改动。

风险：**P2**（不是越过写边界，但"修改工作区外 ACL"是越权副作用）。

### A7. `%TEMP%` 的 ACL 授给谁？

授给**随机 capability SID**，不是用户 SID：`tempCapability, err = newCapabilitySID()`（`windows_helper.go:254-259`），随后 `grantDirectory(tempDir, tempCapability, …)`（`:301-310`），并且这个 SID 被加入令牌的 restricting 列表（`windows_plan.go:241-286`）与默认 DACL（`windows_helper.go:320-330`）。

**结论：SID 选对了**（`CacheCapability:false` ⇒ 用完即撤）。这一条**没有**"授给用户 SID 却不生效"的经典错误。

风险：无（正确）。

### A8. 网络

**Windows 后端完全不治理网络。** `grep -i "network|WFP|firewall"` 在 `windows*.go` 里零命中；`windowsGrantShape` 没有任何网络字段；`Profile.Network` 在 Windows 路径上**不被消费**。

对比：
- Linux bwrap：`--unshare-net`（`profiles.go:BwrapArgs`）
- Linux Landlock：不自带网络隔离（Landlock ABI 4 起才有 `LANDLOCK_ACCESS_NET_*`，本仓库未接）——**这里也有缺口**
- macOS Seatbelt：`(deny network*)`（`profiles.go:317-319`）
- Windows：**无**

⇒ **同一个 `Policy{Network:false}` 在 Windows 上不是"没有网络能力"，而是"网络完全不被约束"**。这是三平台能力差异里唯一"Windows 比 Linux/macOS 明显更宽"的一条，必须写进产品文档。

风险：**P1（安全语义）**。

---

## B. 三平台对照表

| | Linux（bwrap） | Linux（Landlock） | macOS（Seatbelt） | **Windows（ACL）** |
|---|---|---|---|---|
| **可写面** | 工作区根 + `AdditionalWritableRoots` + `$TMPDIR` + `/tmp` + `/var/tmp`（按策略过滤）+ 设备；`$TMPDIR` 等以 rw bind 挂回 | 同左（`--rw` 集合），但**保护链下的目录本身失去新建/删除权** | 同左（`(allow file-write* (subpath …))`） | **只有工作区根 + 每次运行的私有临时目录**；`AdditionalWritableRoots` **被丢弃** |
| **只读面** | `--ro-bind / /`：整棵宿主树只读 | `--ro /` | `(allow default)` + `(deny file-write*)`：读不限、写按 subpath 放行 | 读**完全不限**；写靠对象 ACL + 受限令牌 |
| **网络** | `--unshare-net`（Network=false 时） | **不隔离**（未接 `LANDLOCK_ACCESS_NET`） | `(deny network*)` | **不隔离** |
| **设备** | `--dev /dev` + `--remount-ro /dev`（read-only 时）；workspace-write 给 `/dev/null`、`/dev/ptmx`、`/dev/pts`、`/dev/tty`、`/dev/shm` | 显式 rw `/dev/null` 等（无 `--dev`） | `(literal "/dev/null" "/dev/ptmx" "/dev/tty")` + `(regex #"^/dev/ttys[0-9]+$")` | **无设备模型**；`\\.\`、`\\?\GLOBALROOT` 不受约束（Low IL 不拦设备） |
| **临时目录** | 私有 tmpfs `/tmp`（不落宿主）+ `$TMPDIR` rw bind | **无私有 tmpfs**；read-only 下完全无临时目录 | 无 tmpfs；`$TMPDIR` 若在可写面内才可写 | **私有目录**（每次新建、用完删除）+ `TMP`/`TEMP` 改写、`TMPDIR` 删除 |
| **注册表/等效物** | 无（`~/.config` 等按路径治理） | 无 | `(deny user-preference-write)`（CFPreferences 经 cfprefsd 的写被显式挡） | **无模型**；HKCU 写被 Low IL 顺带拒绝（读不受限、不可配置、无诊断） |

### 不一致处（按"能力要差不多"的诉求排序）

| # | 不一致 | 影响 | 等级 |
|---|---|---|---|
| 1 | **Windows 丢 `AdditionalWritableRoots`** | 宿主显式可写根 / 运行期授权在 Windows 上静默失效（Linux/macOS 生效） | **P1** |
| 2 | **Windows 无网络隔离** | 同一策略在 Windows 上能联网 | **P1** |
| 3 | **工具链缓存三平台都不给** | `go build`/`npm install`/`cargo build` 在三个平台都会失败（用户只在 Windows 上遇到了，因为那边开发工具最依赖 `%LOCALAPPDATA%`） | **P1（共同）** |
| 4 | Landlock 无网络隔离 + 无私有 tmp | Linux 上若只有 Landlock 可用（无 bwrap），`Network:false` 与临时目录语义弱于 bwrap | **P2** |
| 5 | Windows 读面 + 设备命名空间更宽 | `.ssh` 可读、`\\.\PhysicalDrive0` 可达 | **P2** |
| 6 | Windows 注册表不可配置 | 注册表写失败无明确文案 | **P2** |

---

## C. 精确 diff 建议（**未落盘**，供你统一应用）

### D1（P1，4 行）Windows 补上 `AdditionalWritableRoots`

`sandbox/osrunner/windows_plan.go`，`buildWindowsConfinedCommand`：

```diff
 	case sandbox.ModeWorkspaceWrite:
-		return windowsConfinedCommand(executable, argv, policy.Roots(), policy.ProtectedPaths(), winModeWorkspaceWrite)
+		// 与 profile.go 的 workspaceWritableCandidates 保持一致：
+		// 策略根 + 显式追加的可写根（含运行期授权）。
+		writable := append(append([]string(nil), policy.Roots()...), policy.AdditionalWritableRoots()...)
+		return windowsConfinedCommand(executable, argv, writable, policy.ProtectedPaths(), winModeWorkspaceWrite)
```

**安全性**：不新增"能写哪里"的类别，只是把宿主本来就声明的写根真正下发。副作用：这些目录也会获得 Everyone `FILE_DELETE_CHILD` 拒绝 + Low 标签（per-run，最后一名 helper 退出时恢复），即**运行期间宿主用户删不掉这些目录的子项**——对缓存目录可接受，对宿主自定义目录要在文档里写清。

### D2（P1，约 60 行）工具链缓存子目录进入可写面（三平台共用）

`sandbox/osrunner/profiles.go` 新增候选表 + `profile.go` 的 `workspace-write` 分支把它们并进 `ReadWrite`：

```diff
+// toolchainCacheCandidates 返回"开发工具链只写缓存、不含凭据"的目录候选。
+// 只列**缓存子目录**：绝不列 %APPDATA%、%USERPROFILE%、.cargo 根、.m2 根
+// （那里有 .npmrc / credentials.toml / settings.xml）。
+func toolchainCacheCandidates(policy *sandbox.Policy) []string {
+	if !policy.IncludeToolchainCaches() { // 新选项，默认 true；false 时完全不加
+		return nil
+	}
+	home, _ := os.UserHomeDir()
+	local := os.Getenv("LOCALAPPDATA")
+	switch runtime.GOOS {
+	case "windows":
+		return []string{
+			filepath.Join(local, "go-build"),                 // GOCACHE
+			filepath.Join(home, "go", "pkg", "mod"),          // GOMODCACHE
+			filepath.Join(local, "npm-cache"),                // npm
+			filepath.Join(local, "pnpm-store"),               // pnpm
+			filepath.Join(local, "pip", "Cache"),             // pip
+			filepath.Join(local, "uv", "cache"),              // uv
+			filepath.Join(home, ".cargo", "registry"),        // cargo（不是 .cargo 根）
+			filepath.Join(home, ".cargo", "git"),
+			filepath.Join(home, ".m2", "repository"),         // maven（不是 .m2 根）
+			filepath.Join(home, ".gradle", "caches"),         // gradle（不是 .gradle 根）
+			filepath.Join(home, ".nuget", "packages"),
+		}
+	case "darwin":
+		return []string{
+			filepath.Join(home, "Library", "Caches", "go-build"),
+			filepath.Join(home, "go", "pkg", "mod"),
+			filepath.Join(home, ".npm", "_cacache"),
+			filepath.Join(home, "Library", "Caches", "pip"),
+			filepath.Join(home, ".cargo", "registry"), filepath.Join(home, ".cargo", "git"),
+			filepath.Join(home, ".m2", "repository"), filepath.Join(home, ".gradle", "caches"),
+			filepath.Join(home, ".nuget", "packages"),
+		}
+	default:
+		return []string{
+			filepath.Join(home, ".cache", "go-build"),
+			filepath.Join(home, "go", "pkg", "mod"),
+			filepath.Join(home, ".npm", "_cacache"),
+			filepath.Join(home, ".cache", "pip"),
+			filepath.Join(home, ".cache", "uv"),
+			filepath.Join(home, ".cargo", "registry"), filepath.Join(home, ".cargo", "git"),
+			filepath.Join(home, ".m2", "repository"), filepath.Join(home, ".gradle", "caches"),
+			filepath.Join(home, ".nuget", "packages"),
+		}
+	}
+}
```

在 `BuildProfile` 的 `workspace-write` 分支里：

```diff
-		readWrite := append(append(append([]string(nil), roots...), profileTempRoots(policy)...), deviceWritePaths(true)...)
+		// 工具链缓存：不存在时**创建**（否则 canonicalSplit 会把它们当"丢弃"，
+		// 而 Go/npm/cargo 期望自己创建它们）。
+		caches := toolchainCacheCandidates(policy)
+		for _, dir := range caches {
+			_ = os.MkdirAll(dir, 0o700)
+		}
+		keptCaches, _ := canonicalSplit(caches)
+		readWrite := append(append(append(append([]string(nil), roots...), keptCaches...), profileTempRoots(policy)...), deviceWritePaths(true)...)
```

**安全性评估**
- 只列缓存子目录，凭据文件（`.npmrc`、`pip.ini`、`.cargo/credentials.toml`、`.m2/settings.xml`、`.gradle/gradle.properties`）**都不在**任何被授予的路径里——用例 `TestAuditWinToolchain_CredentialParentsAreNeverGranted` 是这条的护栏。
- `GOMODCACHE` / `.m2/repository` / `.nuget/packages` 里是**第三方依赖源码与二进制**，可写它们等于允许 agent 篡改依赖（供应链风险）。缓解：a) 默认只加 `GOCACHE`/`npm-cache`/`pip Cache`/`cargo registry` 这类**纯缓存**，把 `GOMODCACHE`/`.m2`/`.nuget` 放在 `WithToolchainCaches(Mode)` 的更强档；b) 或者只**读**依赖缓存、把下载目录指到私有 temp（`GOFLAGS=-mod=mod` + `GOMODCACHE=$TEMP/go-mod`）。
- 在 macOS/Linux 上这些目录同时进入 `ReadWrite` ⇒ 与 bwrap/Seatbelt/Landlock 的现有渲染无缝兼容，**不需要改后端**。
- Windows 侧需要把 `toolchainCacheCandidates` 也加进 `--workspace`（D1 已经让 `AdditionalWritableRoots` 生效，最小做法是让 policy 在 `kit` 层把它们 `WithWritableRoots(...)` 进去，**这样连 D1 之外都不用改**）。

### D3（P3）`\\?\` 与 UNC 归一

```diff
 func winPathKey(path string) string {
 	trimmed := strings.TrimSpace(path)
 	if trimmed == "" {
 		return ""
 	}
+	trimmed = stripExtendedPrefix(trimmed) // \\?\C:\x → C:\x；\\?\UNC\s\share → \\s\share
 	...
 }

+func stripExtendedPrefix(path string) string {
+	lower := strings.ToLower(path)
+	switch {
+	case strings.HasPrefix(lower, `\\?\unc\`):
+		return `\\` + path[len(`\\?\unc\`):]
+	case strings.HasPrefix(lower, `\\?\`):
+		return path[len(`\\?\`):]
+	}
+	return path
+}
```
`winPathWithinFold` 用同一个 `stripExtendedPrefix`（否则保护区归属判定仍会 fail-open）。

### D4（P2）保护遍历识别 junction

```diff
-walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
+walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
 	if err != nil { return fmt.Errorf(...) }
-	if entry.Type()&fs.ModeSymlink != 0 {
+	if winIsReparsePoint(path, entry) { // windows 实现用 GetFileAttributes 查 FILE_ATTRIBUTE_REPARSE_POINT
 		stats.Skipped++
 		return nil
 	}
```
（`winIsReparsePoint` 放在 `windows_*.go`；非 Windows 构建退化为 `entry.Type()&fs.ModeSymlink != 0`，因此计划层单测仍可覆盖。）

### D5（文档）网络与读面

`docs/zh/AI-SDK.md` 的沙箱章节补两条：① Windows 后端**不隔离网络**；② 三平台都**不限制读**（`.ssh`/`.aws` 可读），需要保密请用容器/VM。

---

## D. Windows 独有的绕过面（逐条判定）

| 面 | 是不是"越过写边界" | 说明 |
|---|---|---|
| **junction（`mklink /J`）** | **否**（写）；**是**"越权副作用" | 写经 junction 到外部目录：目标对象没有 capability SID ⇒ 拒绝。但保护遍历会把 junction 当普通目录下钻，给**工作区外**对象加 deny ACE（A6） |
| **8.3 短名** | **否** | 授权是对象级 ACE，8.3 指向同一对象；`--protect` 的路径匹配用短名时会判不出归属 → 跳过保护（fail-open，见 A2） |
| **UNC `\\server\share`** | 不适用 | 作为工作区根：ACL 设置失败 → fail closed（不 spawn）。作为写入目标：不在可写面内 ⇒ 拒绝 |
| **NTFS 硬链接** | **否**（写） | 目标对象没有 capability SID ⇒ 拒绝；文档已把"工作区内的硬链接可能指到工作区外"列为已知边界（读面本来就不限） |
| **ADS（`file.txt:stream`）** | 需真机确认（**未执行**） | ADS 是同一文件对象的命名流，ACE 同样生效；但 `--protect` 的子项枚举按文件名，`.git/config:$DATA` 这种写法能否被被 ACE 挡住取决于 deny 掩码是否覆盖 `FILE_WRITE_DATA`（当前掩码含 `FILE_WRITE_DATA|FILE_APPEND_DATA`，**应该**能挡住） |
| **`\\.\` / `\\?\GLOBALROOT` 设备路径** | **是**（读+写设备） | 无设备模型、Low IL 不拦设备写。个人客户端场景风险低（需要显式构造），但属于"Windows 比 Linux/macOS 宽"的一面 |
| **Everyone 保活组** | 部分 | restricting 列表里保留 Everyone 是为了 DLL 初始化不炸；代价是"任何对 Everyone 有写授权的位置都可写"（文档化的部分边界，`windows_plan.go:36-40`） |

---

## E. `未执行` 清单（需 Windows 真机确认）

1. 私有临时目录的 `TMP`/`TEMP` 改写与 `TMPDIR` 删除的**实际效果**。
2. UNC 工作区根的 `SetNamedSecurityInfo` 行为（预期 fail closed）。
3. junction 在 `WalkDir` 下的真实表现（本报告的推导：Go 只对 symlink reparse tag 置 `ModeSymlink`）。
4. ADS 写入是否被 deny 掩码挡住。
5. Low IL 下 `pwsh` / `dotnet` / `mvn` 的具体失败点与文案。
6. `\\?\C:\...` 作为 workspace 时的保护路径归属（纯逻辑推导为 fail-open）。
7. 真实 `go build`/`npm install` 在"仅工作区可写"下的报错文案（用于给用户可行动提示）。

---

## F. 新增用例（`sandbox/osrunner/windows_toolchain_test.go`，纯计划层）

| 用例 | 断言 | 状态 |
|---|---|---|
| `TestAuditWinToolchain_AdditionalWritableRootsAreGranted` | `AddWritableRoot`（运行期授权）与 `WithWritableRoots`（静态）的目录都出现在 `--workspace`；未授权目录不在；每个授权根的 capability SID 都进了 restricting 列表 | **绿**（D1 验收） |
| `TestAuditWinToolchain_ReadOnlySubpathsBecomeProtects` | 根内只读子路径（`.git`、`vendor`）进 `--protect` | **绿**（D1 附带） |
| `TestAuditWinToolchain_ToolchainCachesPendingProfileSide` | 工具链缓存可写面（已移交 profile 侧） | **SKIP**（写明原因） |
| `TestAuditWinToolchain_PathFormNormalization` | 大小写/分隔符/尾斜杠/`\\?\` 折叠；`\\?\UNC\` 归一；`Repo2` 不被误判在 `Repo` 之下 | **绿**（D3） |
| `TestAuditWinToolchain_ExtendedPathPrefixIsNormalized` | 保护路径的归属判定在普通写法与 `\\?\` 写法下一致（fail-open 已修） | **绿**（D3） |
| `TestAuditWinToolchain_ReparsePointsAreSkipped` | `winEntryNeedsSkip` 属性位表 + 遍历接线：reparse point 的**整棵子树**不被访问、`Skipped` 计数正确 | **绿**（D4） |
| `TestAuditWinToolchain_MissingReadOnlySubpathIsNotRendered` | 声明了但**尚不存在**的只读子路径不下发（否则 helper fail closed、所有命令跑不起来），存在的仍然下发 | **绿**（D1 附带） |
| `TestAuditWinToolchain_ProtectWalkSkipsSymlinks` | 符号链接被跳过、不碰链接目标 | 绿 |
| `TestAuditWinToolchain_PrivateTempIsRewritten` | 工作区内的临时根被拒绝、工作区外可用 | 绿 |
| `TestAuditWinToolchain_CredentialParentsAreNeverGranted` | 工具链名单里不得出现凭据父目录（未来改动的护栏） | 绿 |

> **注意**：本仓库当前 `sandbox/osrunner/linux_toolchain_test.go`（另一平台的并行审计，14:14）引用了不存在的 `pathWithinOS`，导致整个包无法编译。为了让本报告的用例跑起来，我在副本 `.audit-scratch/plat-win/oscopy/`（剔除该文件）里验证：**2 红 4 绿**，与上表一致。等那边修好后在 `sandbox/osrunner/` 直接跑即可。

复现：
```bash
export PATH=$PATH:/usr/local/go/bin
export GOCACHE=/data/zeokeel-rig/.audit-scratch/plat-win/gocache
# 直接（等 linux_toolchain_test.go 修好后）
go test ./sandbox/osrunner/ -run 'TestAuditWinToolchain' -v
# 或本报告使用的隔离副本
go test ./.audit-scratch/plat-win/oscopy/ -run 'TestAuditWinToolchain' -v
```

交叉编译验证（本机可做）：
```bash
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./sandbox/...   # 通过
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go vet ./sandbox/osrunner/ # 通过
```

---

## G. 修复记录（本侧已落盘，未 commit）

所有改动都在 `sandbox/osrunner/windows_*.go`（本侧所有权）+ 一个中立的纯函数文件；`profile.go` / `profiles.go` / `sandbox/sandbox.go` **未碰**。

| 项 | 落点 | 内容 | 验收 |
|---|---|---|---|
| **D1** | `windows_plan.go: winWritableSurface` / `winRenderableProtects` | Windows 的可写面改吃 `Policy.WritableRoots()`（策略根 + 静态追加 + **运行期授权**），保护面改吃 `Policy.WriteDenyPaths()`（全局保护 + **根内只读子路径**）——与 `profile.go` 的 `workspaceWriteProfile`、进程内 `Policy.AllowsWrite` **同一份模型**。只过滤"尚不存在"的保护路径（helper 对不存在的 `--protect` 是 fail closed；"声明 `.git` 但还没 git init"会让每条命令都失败），**不**按位置过滤（落在授权面之外的由 helper 记一行诊断，既有契约） | `TestAuditWinToolchain_AdditionalWritableRootsAreGranted`、`..._ReadOnlySubpathsBecomeProtects`、`..._MissingReadOnlySubpathIsNotRendered` |
| **D3** | `windows_plan.go: winStripExtendedPrefix` | `winPathKey` / `winPathWithinFold` 剥离 `\\?\` 与 `\\?\UNC\`（→ `\\srv\share`） | `..._PathFormNormalization`、`..._ExtendedPathPrefixIsNormalized` |
| **D4** | `windows_protect.go`、`windows_attrs_windows.go`、`windows_attrs_other.go`、`windows_plan.go` | 保护遍历改用 `winEntryNeedsSkip(attributes, isSymlink)`（`FILE_ATTRIBUTE_REPARSE_POINT`）；跳过的**目录**返回 `fs.SkipDir`（旧实现返回 `nil`，WalkDir 会继续下钻——junction 场景下仍会给外部对象加 ACE）；判定可注入（`winEnforceProtectTreeWithSkip`）以便任意宿主测 | `..._ReparsePointsAreSkipped` |
| **D5** | `platform_windows.go`、`windows_plan.go`（`winMode*`）、`sandbox/runner.go`（仅注释） | 写明：Windows **不隔离网络**、**读不受限**、**设备命名空间不受限**、**注册表不在模型里**；`EnforcementPartial` 给出六条具体含义 | `go doc` 可读 |

### D1 的证据链（三层）

1. **计划层**：`policy.AddWritableRoot(dir, reason)` → `--workspace <dir>` 出现（用例断言）。
2. **令牌层**：每个授权根派生 capability SID（`winWorkspaceCapabilitySID`），并出现在 `windowsRestrictingSIDStrings` 的结果里（用例断言——ACE 与令牌同源，少一边就是"看起来授权了、实际写不进去"）。
3. **ACL 层**：helper 对每个 `--workspace` 调用 `grantDirectory`（`windows_helper.go`），加 capability 允许 ACE + Low 标签 + Everyone `FILE_DELETE_CHILD` 拒绝（代码审读，**真机未执行**）。

### 仍需真机验证（明确清单）

1. `go build` / `go test` 在"工作区 + 工具链缓存可写"下**真的通过**（缓存根由 profile 侧提供）；
2. `request_sandbox_access` 批准后，模型能写进新授权的目录、且**工作区外其它目录仍写不进**（本侧只有 argv 级证据）；
3. `TMP`/`TEMP` 改写与 `TMPDIR` 删除的实际效果；
4. junction 场景：`mklink /J C:\ws\.git\esc C:\Windows\System32` 后跑一条命令，确认 `C:\Windows\System32` 的 DACL **未**被改动、`Skipped` 计数 +1；
5. `\\?\C:\...` 作为 workspace 时保护仍生效（本侧已按纯函数修，真机再验一次更稳）；
6. UNC 工作区根：预期 fail closed（不 spawn），确认错误文案可行动；
7. ADS（`file.txt:stream`）写入是否被 deny 掩码挡住；
8. Low IL 下 `pwsh` / `dotnet` / `mvn` 的真实失败点与文案。

### 交叉验证（本机可做，已做）

```
go build ./...                                     linux PASS
GOOS=windows/amd64, windows/arm64, darwin/arm64, darwin/amd64  全部 PASS
GOOS=windows go vet ./sandbox/osrunner/             PASS（含我新增的测试文件）
GOOS=windows go test -c ./sandbox/osrunner/         成功产出 8.1 MB 测试二进制
go test ./sandbox/ ./sandbox/osrunner/ -count=1     ok / ok
CGO_ENABLED=1 go test -race ./sandbox/ ./sandbox/osrunner/ -count=1   ok / ok（10.9s）
gofmt -l sandbox/                                    空
staticcheck ./sandbox/ ./sandbox/osrunner/           仅剩两条 dead code，均归 profile 侧：
                                                    profile.go:189 workspaceWritableCandidates
                                                    sandbox/path.go:341 (Policy).protectedAncestorOf
go run ./cmd/rigdoc -check                           PASS
```

### 阻塞修复（不属于本侧所有权，已处理）

`sandbox/osrunner/linux_toolchain_test.go`（另一平台审计）调用了不存在的 `pathWithinOS` → 整个包无法编译。处理方式：新增 `sandbox/osrunner/pathcompare.go`（非测试文件、纯函数、不导出语义与 `sandbox.pathWithinOS` 一致），**未改动任何既有断言**。若该文件作者打算自己提供同名 helper，删掉 `pathcompare.go` 即可（两者语义相同）。
