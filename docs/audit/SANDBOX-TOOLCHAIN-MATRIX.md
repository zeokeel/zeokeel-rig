# Linux 沙箱 · 开发工具能力对齐审计（实测）

- **审计对象**：`sandbox/osrunner`（Linux 后端：`linux_bwrap.go` + `linux_landlock.go`，共同输入 `profile.go`/`profiles.go`）、`sandbox` 策略层
- **本机**：Ubuntu 24.04.5 / kernel 6.8.0-117（Landlock ABI 4，`/sys/kernel/security/lsm` 含 landlock）/ bubblewrap 0.9.0 / 以 root 运行
- **角色**：Linux 侧负责人。**只读实现 + 新增 `sandbox/osrunner/linux_toolchain_test.go`**；未修改 `profile.go`/`profiles.go`/`sandbox/*.go`（需要的改动以精确 diff 形式给在 §4）
- **真机执行**：**本机就是 Linux，下面 A 组每一条都是真的 spawn 出来的**（`Policy` → `Runner.Confine` → `exec(Confine.Argv)`），不是静态推导。原始输出保留在每个判定后面
- **时刻**：2026-11-18 14:1x–14:5x（Asia/Shanghai）
- **姊妹文档**：[macOS](SANDBOX-TOOLCHAIN-MATRIX-macOS.md)（纯静态推导）、[Windows](SANDBOX-TOOLCHAIN-MATRIX-Windows.md)（纯计划层）

---

## 0. 结论摘要

### 0.1 一句话

**"沙箱一开、开发全废"在 Linux 上是真的**——但不是三个平台都废在同一个地方：Linux 废在**缓存目录不可写**（GOCACHE/GOMODCACHE/`~/.npm`/`~/.cargo`…），而这一条**已经被本轮的工具链缓存白名单修掉了**（我实测确认，见 §0.3）。修完之后 Linux 上 `go build`/`go test`/`go vet`/`npm install`/`cargo build`/`gcc`/`make`/`git commit` 全部可用。

### 0.2 最严重的 5 条能力缺口（按严重度）

| # | 缺口 | 实测证据 | 现状 |
|---|---|---|---|
| **1** | **`go build`/`go test`/`go vet` 全废**：`GOCACHE=~/.cache/go-build`、`GOMODCACHE=~/go/pkg/mod` 在可写面之外 | bwrap：`open …/.cache/go-build/35/35d9…-d: read-only file system`（EXIT=1）；landlock：同路径 `permission denied`。`go mod download` 另有 `mkdir ~/go/pkg/mod/cache: read-only file system` | **已修**（工具链缓存白名单），实测 EXIT=0 |
| **2** | **`git commit`/`git add` 在"kit 默认保护 `.git` + 挂了 OS runner"时必失败** | bwrap：`fatal: Unable to create '<ws>/.git/index.lock': Read-only file system`；landlock（产品 grants）：`…/index.lock: Permission denied`。`git status`（只读）正常 | **未修**（两个默认值互相打架，见 §3.2，需要你裁决） |
| **3** | **`pip install --user` 仍不可用**：用户安装目标是 `~/.local/lib/python3.X/site-packages` + `~/.local/bin`，白名单只给了 `~/.local/share/pnpm` | `cannot create <home>/.local/lib/p-…: Read-only file system`（bwrap）/ `Permission denied`（landlock） | 未修（§4.1 给出 3 行 diff） |
| **4** | **`docker`（以及任何"经由宿主守护进程 unix socket"的东西）是完全逃逸面**：`/var/run/docker.sock` 在三种模式下都能用 | read-only 模式（`--ro-bind / /` + `--unshare-net` + `--cap-drop ALL`）下：`docker run --rm alpine sh -c 'echo INSIDE_CONTAINER; id -u'` → `INSIDE_CONTAINER` / `0`（容器内是 root）；更直接：`docker run --rm -v /:/host:ro alpine cat /host/etc/hostname` 打印出**宿主** hostname。沙箱的 ro 挂载限制不了守护进程 | 未修（设计上不属于"文件效果"，但必须写进文档/诊断） |
| **5** | **Landlock 后端根本不隔离网络**：同一份 `Policy{AllowNetwork:false}` 在 bwrap 上 `curl 1.1.1.1` = `000curl-failed`，在 Landlock 上 = `301`（**真的连出去了**） | 见 §2 网络行：landlock 在 read-only / workspace-write / +net 四种组合下出口网络全部 `301` | 未修（Landlock 的 `LANDLOCK_ACCESS_NET` 没被使用；而且"允许网络"这条在 Landlock 上等于无约束） |

补充两条"Linux 独有"的：

| # | 现象 | 证据 | 影响 |
|---|---|---|---|
| 6 | **bwrap 的私有 tmpfs `/tmp` 会把"工作区在 /tmp 下"整块遮住** | 工作区放 `$TMPDIR`（默认 `/tmp`）时，沙箱内所有探针都是 `No such file or directory`；宿主的 `/tmp` 与被约束进程的 `/tmp` 不是一个目录 | 用户/CI 把项目放在 `/tmp` 下 → 沙箱里看不到自己的仓库（会被误判成"文件不存在"）。这条我踩过一次，测试里已固化 |
| 7 | **Landlock 不治理元数据**：`chmod`/`utimensat` 对**可写面之外**的文件照样成功 | `chmod 666 <home>/.gitconfig` → EXIT=0，宿主侧 `-rw-rw-rw-` 真的变了；`touch -d 2020-01-01` 同样 EXIT=0。bwrap 上这两条都是 DENY | `chmod 000` 任意可读文件 = 对宿主的一次可用性破坏；包文档已承认"不治理"，但**后果**（可改宿主文件元数据）值得进 KNOWN-BOUNDARIES |

### 0.3 修复验证（工具链缓存白名单，已落地）

`BuildProfile` 现在为 workspace-write 追加（Linux）：`~/.cache`、`~/go`、`~/.npm`(+`npmrc` 只读)、`~/.cargo`(+`credentials.toml`/`config` 只读)、`~/.rustup`、`~/.m2`(+`settings.xml` 只读)、`~/.gradle`(+`gradle.properties` 只读)、`~/.local/share/pnpm`。**实测**：

```
# 修复后，默认环境（GOCACHE 默认 ~/.cache/go-build、TMPDIR 默认 /tmp）
bwrap    : go build EXIT=0 / go test ok m 0.004s / go vet EXIT=0
landlock : go build EXIT=0 / go test ok m (cached)
# 首次下载模块（file:// GOPROXY，无外网也能验）：GOMODCACHE 默认 → mkdir ~/go/pkg/mod/cache: read-only file system
#                                               GOMODCACHE 在工作区 → downloading … EXIT=0，缓存真的写进去
# 缓存目录里的凭据文件仍然只读（两个后端一致）：
写 ~/.npm/.npmrc            → DENY（bwrap: Read-only file system / landlock: Permission denied）
写 ~/.cargo/credentials.toml → DENY
写 ~/.m2/settings.xml        → DENY
写 ~/.gradle/gradle.properties → DENY
```

⚠️ 一个**前提条件**（我在实测中踩到）：缓存根是用**宿主进程的 `HOME`/`os.Getenv`** 算出来的，而被约束进程用的是它自己继承的 `HOME`。两者一致时（产品默认情形）没问题；若宿主给子进程换 `HOME`（例如终端工具按会话改环境），白名单会指向错误的目录 → 全部 DENY。建议在 `BuildProfile` 里对"策略根之外"的缓存根加一条 `os.Stat` 存在性校验（已存在，`toolchainCacheCandidates` 做了），并在诊断里输出实际生效的缓存根。

---

## 1. 方法与环境

### 1.1 走的是产品路径

```go
policy := sandbox.New(sandbox.WithRoots(ws), sandbox.WithMode(mode), sandbox.WithNetwork(net))
confined, err := runner.Confine([]string{"/bin/sh","-c",script}, policy) // bwrap / landlock
cmd := exec.Command(confined.Argv[0], confined.Argv[1:]...)                // 真的 spawn
cmd.Dir, cmd.Env = ws, {PATH,HOME(伪),TMPDIR(伪),LANG}
```

- **bwrap**：`osrunner.New(osrunner.Config{})` 选出来的 runner（本机 bwrap 可用，探测通过）
- **Landlock**：本机 `New()` 会优先选 bwrap，因此审计时直接用同一份 helper 参数驱动后端（`osrunner.LandlockGrants(policy)` 算 `--ro/--rw`，`os.Executable()+HelperArgLandlock` 作 argv[0]）——参数与 `landlockRunner.Confine` 逐字相同，helper 也是同一个
- **danger-full-access**：按产品语义透传（`EnforcementNone`），不贴 sandbox 参数

### 1.2 复现

```bash
export PATH=$PATH:/usr/local/go/bin
export GOCACHE=/data/zeokeel-rig/.audit-scratch/plat-linux/gocache
# 必须给一个 /tmp 之外的目录：bwrap 的私有 tmpfs 会遮住 /tmp
export RIG_SANDBOX_TOOLCHAIN_DIR=/data/zeokeel-rig/.audit-scratch/plat-linux/box
RIG_SANDBOX_TOOLCHAIN_TEST=1 go test ./sandbox/osrunner/ -run Toolchain -v
```
默认不跑（`RIG_SANDBOX_TOOLCHAIN_TEST != "1"` 时全部 `t.Skip`），不拖慢常规 CI。

### 1.3 环境（版本钉住结论）

`go 1.27.3`、`node v26.13.0`、`npm 11.20.0`、`python 3.12.3`、`cargo/rustc 1.99.1`、`git 2.43.0`、`gcc 13.3.0`、GNU ld 2.42、`docker 29.1.3`、`bwrap 0.9.0`、Landlock ABI 4。

---

## 2. A 组：逐项实测矩阵

图例：**OK** = 命令成功；**DENY** = 被沙箱拒绝（附原始错误）；**FAIL** = 命令失败（非沙箱原因）；**私有** = 写成功但落在 bwrap 的私有 tmpfs（宿主看不到）。

### 2.1 workspace-write（`AllowNetwork=false`，修复后）

| # | 能力 | bwrap | Landlock | 备注 / 原始错误 |
|---|---|---|---|---|
| 1 | 写工作区 | OK | OK | 基线 |
| 2 | `GOCACHE`（`~/.cache/go-build`）写 | OK | OK | 修复前：`read-only file system` / `permission denied` |
| 3 | `GOMODCACHE`（`~/go/pkg/mod`）写 | OK | OK | 修复前：`mkdir ~/go/pkg/mod/cache: read-only file system` |
| 4 | `~/.npm` 写 | OK | OK | `npm error Log files were not written due to an error writing to the directory: …/.npm/_logs`（修复前） |
| 5 | `~/.cache/pip` 写 | OK | OK | 修复前 DENY |
| 6 | `~/.cargo` 写 | OK | OK | 修复前 DENY |
| 7 | `~/.rustup` 写 | OK | OK | 修复前 DENY |
| 8 | `~/.m2` / `~/.gradle` 写 | OK | OK | 修复前 DENY |
| 9 | `~/.local/lib`（`pip install --user` 目标） | **DENY** | **DENY** | `~/.local/bin` 同样不在白名单 → **剩余缺口 #3** |
| 10 | 自定义 `$TMPDIR` 写 | DENY¹ | DENY¹ | ¹ 仅当该目录 ≠ 宿主进程的 `os.TempDir()`；产品默认继承，故一致 |
| 11 | 字面 `/tmp` 写 | OK（**私有**） | OK（**宿主 /tmp**） | 语义不同！见 §3.1 |
| 12 | `/var/tmp` 写 | DENY² | DENY² | ² 本机宿主 `/var/tmp` 本身只读；profile 里它是有 `--rw` 的 |
| 13 | `/dev/shm` 写 | OK | OK | read-only 模式下不授予（见 §3.1） |
| 14 | `/dev/null` 写 | OK | OK | — |
| 15 | 读 `/etc/hosts`、`/etc/resolv.conf` | OK | OK | `127.0.1.1 dsh` |
| 16 | 读 `/proc/self/exe` | OK | OK | `/usr/bin/readlink`（Go/Java 关键：可用） |
| 17 | 读 `/proc/version`、`/sys/devices/system/cpu/online` | OK | OK | `0-95` |
| 18 | 读 `~/.ssh/id_ed25519`、`~/.gitconfig` | OK | OK | 读凭据**是允许的**（只读围栏管写） |
| 19 | 写 `~/.ssh/evil`、`~/outside.txt` | DENY | DENY | `Read-only file system` / `Permission denied` |
| 20 | `chmod` 工作区内文件 | OK | OK | — |
| 21 | `chmod` 可写面外文件 | DENY | **OK** ⚠️ | landlock 不治理 → 见缺口 #7 |
| 22 | `touch`（utimensat）可写面外文件 | DENY | **OK** ⚠️ | 同上 |
| 23 | 回环 `bind(127.0.0.1:0)` | OK | OK | `BIND_OK`（`--unshare-net` 下 lo 仍然工作） |
| 24 | 回环 `connect`（自建 HTTP 服务） | OK | OK | `hi` |
| 25 | 出口网络（`curl 1.1.1.1`） | **DENY** | **OK** ⚠️ | bwrap `000curl-failed` / landlock `301` → 缺口 #5 |
| 26 | `go build`（无依赖） | OK | OK | 修复后；修复前两条后端都失败 |
| 27 | `go test` / `go vet` | OK | OK | 同上 |
| 28 | 首次下载模块（本地 `file://` 代理） | OK | OK | `GOMODCACHE` 在工作区时成功；默认位置失败 |
| 29 | `npm install <本地目录>` | OK | OK | `added 1 package in 279ms` |
| 30 | `npm cache add`（写 `~/.npm`） | OK | OK | 缓存目录已可写 |
| 31 | `python3 -m venv .venv`（venv 在工作区） | OK | OK | — |
| 32 | `pip install`（venv 内、无网络） | FAIL | FAIL | `Could not find a version…`：无网络/无本地索引，非沙箱原因 |
| 33 | `cargo build`（无依赖 crate） | OK³ | OK³ | ³ 需要 `RUSTUP_HOME` 可**读**（`/root/.rustup` 只读即可）+ `CARGO_HOME` 可写；实测 `Compiling c … Finished` |
| 34 | `gcc` 编译到工作区 / `/tmp` | OK | OK⁴ | ⁴ landlock 下依赖 `$TMPDIR` 被授权（默认 `/tmp` 已授权） |
| 35 | `make` | OK | OK | `MAKE_OK` |
| 36 | `git init/add/commit`（**未保护** `.git`） | OK | OK | `[master (root-commit) …] 1 file changed` |
| 37 | `git init/add/commit`（**保护** `.git`，kit 默认） | **DENY** | **DENY** | `index.lock: Read-only file system` / `Permission denied` → 缺口 #2 |
| 38 | `git status`（只读，`.git` 受保护） | OK | OK | 读不受影响 |
| 39 | `docker ps` / `docker run` | **OK** ⚠️ | **OK** ⚠️ | `/var/run/docker.sock` 可连 → 缺口 #4 |
| 40 | `go env -w`（写 `~/Library/Application Support/go/env` / `~/.config/go/env`） | DENY | DENY | Linux 上 `GOENV` 默认 `~/.config/go/env`，未在白名单；`go env -w` 会失败（`go build` 不受影响） |

### 2.2 read-only / danger-full-access（同一条命令的对照）

| 能力 | read-only（bwrap） | read-only（landlock） | danger-full-access |
|---|---|---|---|
| 写工作区 | DENY（`Read-only file system`） | DENY（`Permission denied`） | OK |
| 写 `GOCACHE` | DENY | DENY | OK |
| `go build` | DENY | DENY | OK |
| `gcc` 到 `/tmp` | **OK**（私有 tmpfs） | **DENY**（无私有 tmpfs，`/tmp` 未授权） | OK |
| 读 `/etc/hosts`、`/proc/self/exe` | OK | OK | OK |
| 出口网络 | DENY | **OK** ⚠️ | OK |
| `docker run` | **OK** ⚠️ | **OK** ⚠️ | OK |

> read-only 下 bwrap 的"gcc 到 /tmp 能成功"不是漏洞：那是**私有 tmpfs**，产物不落宿主（实测宿主 `/tmp` 看不到）。Landlock 没有 tmpfs，所以它连临时文件都写不了——这是两个后端在 read-only 下的**能力差异**，不是 bug（`profile.go` 文档已写明），但对"只读但想跑 `go vet` / `git status`"的用户是真实体验差异。

---

## 3. B 组：四后端语义差异

### 3.1 只读面模型：四种完全不同的姿态

| 后端 | 姿态 | 读的边界 | 写不授权时会怎样 |
|---|---|---|---|
| **bwrap** | 显式 `--ro-bind / /` 后按需 `--bind` 覆盖 | 整根只读可读（含 `/proc`、`/sys`、`/dev`） | `EROFS "Read-only file system"` |
| **Landlock** | `--ro /` + `--rw` 逐路径；规则是**并集**（父 rw 会盖住子 ro，所以写授权必须"下移到兄弟项"） | `--ro /` 给了读；未授予的路径**写被拒** | `EACCES "Permission denied"`；**元数据（chmod/utimensat）不治理** |
| **Seatbelt**（macOS，见姊妹文档） | `(allow default)` + `(deny file-write*)` + 白名单 subpath | 默认允许一切读/系统服务 | `EPERM "operation not permitted"` |
| **Windows ACL**（见姊妹文档） | 受限令牌 + 只给授权目录加 ACE | 读不受限 | 访问被拒（ACL） |

**结论**：四种姿态下"能读什么"差异很大，但**"能写什么"在 Linux/macOS 上语义一致**（都是白名单式），只是错误文案不同（`EROFS` vs `EACCES` vs `EPERM`）——这会影响 `DenialSignatures` 的归因，三方字幕都要在签名表里。

### 3.2 网络：四个后端四种行为（**最大的语义不一致**）

| 策略 | bwrap | Landlock | Seatbelt | Windows ACL |
|---|---|---|---|---|
| `AllowNetwork=false` | `--unshare-net` → 出口全断，**回环仍可用** | **完全不拦**（实测 `curl 1.1.1.1` = 301） | `(deny network*)`（含回环） | 不拦 |
| `AllowNetwork=true` | 不 unshare → 全通 | 全通（本来就没拦） | 允许 | 不拦 |

即：**同一份 `Policy{AllowNetwork:false}` 在 Linux 上"安全度"取决于选了哪个后端**；在 Windows 上根本没有网络语义。建议：`Policy` 层把"网络"降级成**显式 opt-in 能力**并在 `sandboxstatus` 里如实报"本后端的网络覆盖：none/partial/full"，否则用户会以为 `AllowNetwork=false` 是跨平台承诺。

### 3.3 临时目录身份

| 后端 | 字面 `/tmp` | `$TMPDIR` | 私有性 |
|---|---|---|---|
| bwrap | `--tmpfs /tmp`（**私有**，不落宿主；**会遮住宿主 `/tmp` 下的一切**，包括放在 `/tmp` 里的工作区） | 按 `IncludeTmpdirEnv` 授权（bind） | 私有 |
| Landlock | 按 `IncludeSlashTmp` 授权 → **就是宿主 `/tmp`**（写进去宿主可见、多会话共享） | 同上 | **无私有 tmp** |
| Seatbelt | 按授权放行宿主 `/tmp` | 同上 | 无 |

**同一份策略在 bwrap 下"临时文件不落宿主"，在 Landlock/Seatbelt 下会真的落在宿主 `/tmp`** —— 如果用户在意"临时文件不外泄"，这是实质差异，建议文档明确写"私有 tmp 仅 bwrap"。

### 3.4 设备节点与 `/proc/self/*`

| 项 | bwrap | Landlock | 备注 |
|---|---|---|---|
| `/dev/null` | `--dev /dev` 提供 | 显式 `--rw /dev/null` | 两者都可用 |
| `/dev/shm` | workspace-write 授予；read-only 不授予 | 同 | 一致 |
| `/dev/tty` | `--dev` 下有节点，但无控制终端时 `open` 失败（`No such device or address`）；pty 场景由 terminal 工具自带 pty | 授权了路径，同样无控制终端 | 一致（都不是沙箱问题） |
| `/proc/self/exe` | OK（`readlink` 正常） | OK | Go/Java 的自我定位可用 |
| `/proc` 整体 | `--proc /proc` 重新挂载（**只有本进程视角**） | 只读跟随宿主 | 进程可见性不同：bwrap 下看不到宿主其它进程（`--unshare-pid`） |

### 3.5 保护路径（`.git`）的实现差异

| 后端 | 机制 | 实测（保护 `.git` 后 `git commit`） |
|---|---|---|
| bwrap | 后挂载 `--ro-bind` 覆盖前面的 rw bind | **失败**：`index.lock: Read-only file system` |
| Landlock | 写授权下移到兄弟项（不能靠"父 rw + 子 ro"） | **失败**：`index.lock: Permission denied`（用产品 grants 时） |
| Seatbelt | SBPL last-match-wins | 按姊妹文档：失败（`(deny file-write* (subpath "<ws>/.git"))`） |
| Windows ACL | 给保护目录及其子树加 deny ACE | 按姊妹文档：生效 |

**三方一致地把 `.git` 保护住了——于是三方一致地让 `git commit` 在沙箱里跑不了。** 这是"kit 默认开保护"与"挂 OS runner"两个默认值的冲突，不是某个后端的 bug。裁决建议见 §4.2。

---

## 4. 精确 diff（**未落盘**）

### 4.1 补上 `pip install --user` / `~/.local/bin`（P1，最小改动）

`sandbox/osrunner/toolchain_caches.go`，`switch goos` 的 linux 分支（`default:`）里，在 `add(filepath.Join(home, ".local", "share", "pnpm"), …)` 之后追加：

```diff
 		add(filepath.Join(home, ".gradle"), filepath.Join(home, ".gradle", "gradle.properties"))
 		add(filepath.Join(home, ".local", "share", "pnpm"), homeCreds(filepath.Join(home, ".local", "share", "pnpm"))...)
+		// pip install --user 的落点是 ~/.local/lib/pythonX.Y/site-packages 与 ~/.local/bin，
+		// 不是 ~/.local/share。只加这两层，不加整个 ~/.local（那里还有 ~/.local/state、
+		// ~/.local/share/keyrings 等非缓存内容）。
+		add(filepath.Join(home, ".local", "lib"))
+		add(filepath.Join(home, ".local", "bin"))
 	}
```

**安全性**：`~/.local/lib` 里是已安装的 Python 包（可写 = 可以植入包，但沙箱里的进程本来就能写工作区并执行工作区的代码，威胁模型上不增加新面）；`~/.local/bin` 可写 = 可以在用户 PATH 里放一个同名可执行文件（**这一点有持久化风险**，但它与 `~/.local/lib` 的取舍相同）。若你只想修 pip 而不想给 `bin`，只加 `lib` 即可让 `pip install --user --no-warn-script-location` 工作。

### 4.2 `.git` 保护 vs `git commit`（P0，需要决策，不是纯代码 diff）

现状：保护开 → 沙箱里 `git add/commit` 必失败（三方一致）。三个选项：

**A. 在 runner 侧只保护"文件工具的写入面"，不保护 `.git`**（推荐）
```diff
--- a/sandbox/osrunner/profile.go
-		Protected:        canonicalUnique(existingPaths(policy.ProtectedPaths())),
+		// .git 是版本控制工具自己的目录：进程内文件工具（write_file/apply_patch）
+		// 仍然保护它，但沙箱里的 shell 必须能提交。否则"保护默认开 + 挂 runner"
+		// 会让 git 在该工作区完全不可用（实测 index.lock 无法创建）。
+		Protected:        canonicalUnique(existingPaths(withoutGitMetadata(policy.ProtectedPaths()))),
```
（`withoutGitMetadata` 过滤掉 basename 为 `.git` 的项；其余保护路径不变。）

**B. 保持现状，把"沙箱里不能 git commit"写进文档**，用户要提交就 `DisableProtection` 或不开 runner。

**C. 反向：把 `.git` 从 kit 的默认保护里去掉**（只保护主目录凭据位置），把"别改 .git"交给 git 自己的守卫。

我倾向 **A**：它保住了"文件工具不许碰 .git"（真正的误伤面），又让最常用的开发动作可用；`.git` 被 shell 改写本来就是"用户自己的 shell"的正当能力。

### 4.3 网络：让 `AllowNetwork=false` 在 Landlock 上也成立（P1）

Landlock ABI ≥ 4 支持 `LANDLOCK_ACCESS_NET_BIND_TCP/CONNECT_TCP`。在 helper 里当 `!profile.Network` 时：
1. 把 `LANDLOCK_ACCESS_NET_*` 加进 `handled_access_net`（不要混进 `handled_access_fs`）；
2. **不**创建任何 net 规则（handled 且无规则 = 默认拒绝）；
3. 归因签名加 `"connect: operation not permitted"`。
注意：ABI < 4 的内核上无法做到，应在 `Enforcement` 里报 `EnforcementPartial` 并在诊断里写明"本后端的网络隔离不可用"。

### 4.4 诊断：把"实际生效的可写面"暴露出来（P2）

`BuildProfile` 已经算了 `ToolchainCaches`/`Dropped`/`MissingProtected`；建议 `sandboxstatus` 与 `cmd/rigdoctor` 输出新增两行：
```
toolchain_caches: [~/.cache ~/go ~/.npm …]（实际存在并被授权的）
network_coverage: none|partial|full（本后端）
```
否则"缓存白名单生效了没有"只能靠 `go build` 试错——这正是我自己踩过的坑（宿主 HOME ≠ 子进程 HOME 时白名单静默失效）。

---

## 5. 未执行 / 未验证声明

| 项 | 状态 | 原因 |
|---|---|---|
| macOS Seatbelt 的任何一条 | **未执行** | 本机是 Linux；见姊妹文档（静态推导） |
| Windows ACL 的任何一条 | **未执行** | 同上 |
| 真·外网访问（proxy.golang.org、npm registry） | **部分未验证** | 本机出口网络不稳定（`proxy.golang.org` i/o timeout）；用 `file://` 本地模块代理验证了"首次下载要写 GOMODCACHE"这条 |
| `docker run` 实际拉取镜像 | **部分验证** | 本机已有 alpine 缓存 → 容器确实启动；拉取在 `--unshare-net` 下不可行（这本身也是结论：镜像必须在沙箱外准备好） |
| `cargo build` 带真实依赖（需 crates.io） | 未验证 | 无外网；用无依赖 crate + `CARGO_HOME` 在工作区验证了编译路径 |
| `go env -w` | 已验证失败 | 期望失败（GOENV 不在白名单）；若认为它该可用，加 `~/.config/go` 并标 `env` 只读 |

---

## 6. 新增用例

`sandbox/osrunner/linux_toolchain_test.go`（**门控**，默认 `t.Skip`）：

```bash
RIG_SANDBOX_TOOLCHAIN_TEST=1 RIG_SANDBOX_TOOLCHAIN_DIR=/var/tmp/rig-audit \
  go test ./sandbox/osrunner/ -run Toolchain -v
```

| 用例 | 作用 |
|---|---|
| `TestToolchainCapabilityMatrixReport` | 打印 §2 的完整矩阵（`MATRIX\|backend\|mode\|capability\|verdict\|code\|原始首行`），供本文件复核 |
| `TestToolchainWorkspaceWriteAllowsWorkspaceOnly` | 契约：工作区可写、`$HOME` 不可写（两个后端） |
| `TestToolchainReadOnlyDeniesWrites` | 契约：read-only 下工作区也不可写，但读系统路径可用 |
| `TestToolchainDevCacheDirsAreWritable` | **回归**：GOCACHE/GOMODCACHE/`~/.npm`/`~/.cache/pip`/`~/.cargo`/`~/.m2`/`~/.gradle` 必须可写（修复前这条会红，就是缺口 #1 的可执行证据） |
| `TestToolchainCacheCredentialFilesStayReadOnly` | **安全回归**：`~/.npm/.npmrc`、`~/.cargo/credentials.toml`、`~/.m2/settings.xml`、`~/.gradle/gradle.properties` 必须保持只读 |
| `TestToolchainTmpDirIdentity` | 记录 `/tmp` 的私有性差异（bwrap 私有 tmpfs vs landlock 宿主 /tmp） |
| `TestToolchainPrivateTmpMasksHostTmp` | 记录"工作区在 `/tmp` 下会被 bwrap 遮住"（我的实测坑） |
| `TestToolchainNoNetworkByDefault` | 记录网络语义差异（bwrap 断出口 / landlock 不拦） |
| `TestToolchainDangerFullAccessPassesThrough` | danger-full-access = 透传，`EnforcementNone` |
| `TestToolchainNoNetworkByDefault` | 记录网络语义差异（bwrap 断出口 / landlock 不拦；回环两者都通） |
| `TestToolchainToolVersions` | 打印工具链版本，把结论钉在版本上 |

**跑之前必须知道的一件事**：缓存根与临时根都是**用宿主进程的 `HOME`/`TMPDIR` 算的**，而被约束进程用的是它继承的环境。测试里必须 `t.Setenv("HOME"/"TMPDIR"/"USERPROFILE", 伪值)` 把两者对齐——否则白名单指向真实 `$HOME`，用例会**假红**（我第一版就是这样红的）。这同时说明产品侧有一个隐含前提：宿主不要给被约束进程换 `HOME`/`TMPDIR`。

实测汇总（`RIG_SANDBOX_TOOLCHAIN_TEST=1`，两个后端 × 全部子用例）：**全部 PASS**，其中 `TestToolchainDevCacheDirsAreWritable` 在修复前对 8 个缓存目录 × 2 个后端**全红**（`read-only file system` / `permission denied`）——这就是缺口 #1 的可执行证据。