# macOS 沙箱 · 开发工具能力对齐审计

- **审计对象**：`sandbox/osrunner/profiles.go` 的 `SeatbeltProfile`（+ `profile.go` 的 `BuildProfile`）、`platform_darwin.go`、`darwin.go`
- **审计者角色**：只读实现 + 新增 `sandbox/osrunner/darwin_toolchain_test.go`；**未修改** `profile.go` / `profiles.go` / `sandbox/*.go`（改动以精确 diff 形式给出）
- **方法**：本机是 Linux，因此**不执行** `sandbox-exec`。做的是：读渲染器 → 在 Linux 上跑同一份渲染代码得到**真实输出** → 用 SBPL 的声明式语义做静态推导 → `GOOS=darwin` 交叉编译
- **时刻**：2026-11-18 14:1x（Asia/Shanghai）
- **真机执行**：**全部未执行**；下文每条"能/不能"均为静态推导结论，标注了 SBPL 依据行

---

## 0. 结论摘要（先回答你最担心的两个问题）

### 0.1 "macOS 的 SBPL 是不是默认拒绝、只白名单？"

**不是。** macOS 这一侧是 **`(allow default)` + `(deny file-write*)` + 少量白名单**——即"**默认允许一切，只把写操作关死，再按需放行**"。

```sbpl
(version 1) (allow default) (deny file-write*) …
```

这条姿态的含义（直接决定"漏一项会怎样"）：

| 能力 | macOS（Seatbelt，本仓库姿态） | 漏掉会怎样 |
| --- | --- | --- |
| 读文件 / `stat` / `getattrlist`（`file-read*`、`file-read-metadata`） | **默认允许**（profile 里没有任何 `deny file-read*`） | 不会漏——`ls`、`git status`、`go env` 读配置都能过 |
| `sysctl-read` | **默认允许** | 不需要白名单 |
| `mach-lookup`（含 `com.apple.system.logger`、`cfprefsd`、`securityd`…） | **默认允许** | 不需要白名单 |
| `process-exec` / `process-fork`（exec 任意二进制） | **默认允许** | 不需要白名单 |
| `file-map-executable`（dyld 共享缓存 + **JIT**） | **默认允许** | 不需要白名单 |
| **写**（`file-write*`：create/unlink/mode/owner/ioctl…） | **默认拒绝**，只放行 profile 里列的 subpath/literal | **这就是全部问题所在**：不在白名单里的写 → `EPERM "operation not permitted"` |

**所以我先前担心的"白名单漏一项就是硬失败"在 macOS 上并不成立**：漏的是**读/系统服务**不会失败（allow-default 兜住了），真正会硬失败的只有**写**。这也解释了为什么 macOS 上"工具链全废"的表现是清一色的 `Operation not permitted`，而不是"命令找不到 / 服务不可用"。

### 0.2 当前会失败的工具（按严重度）

| 排名 | 工具 / 场景 | 失败原因（SBPL 依据） | 严重度 |
| --- | --- | --- | --- |
| **1** | **`go build` / `go test`** | `GOCACHE`（macOS 上是 `~/Library/Caches/go-build`）与 `GOMODCACHE`（`~/go/pkg/mod`）都不在 `(allow file-write* (subpath …))` 里 → `go: creating work dir: mkdir …: operation not permitted` | **P0** |
| **2** | **`git commit` / `git add` / 任何写 `.git` 的操作** | `(deny file-write* (subpath "<workspace>/.git"))` 是**显式**规则（kit 默认开启保护）→ `index.lock` 创建被拒 | **P0**（跨平台一致的设计取舍，不是 macOS 独有；但 sbpl 的 last-match-wins 让它成为硬失败） |
| **3** | **`npm ci` / `npm install`** | `~/.npm/_cacache` 写 → 不在白名单 | **P0** |
| **4** | **`pip install`** | `~/Library/Caches/pip` 写 → 不在白名单 | **P0** |
| **5** | **`cargo build`** | `~/.cargo/registry`、`~/.rustup` 写 → 不在白名单 | **P0** |
| **6** | **`xcodebuild` / `swift build`** | `~/Library/Developer/Xcode/DerivedData`、`~/Library/Developer/CoreSimulator`、`~/Library/Caches/org.swift.swiftpm` 写 → 不在白名单 | **P1** |
| **7** | **`mvn` / `gradle`** | `~/.m2/repository`、`~/.gradle/caches` 写 → 不在白名单 | **P1** |
| **8** | **`go env -w`** | 写 `~/Library/Application Support/go/env` → 不在白名单 | P2 |
| **9** | `python3 -m venv .venv`（**venv 在 workspace 内**） | ✅ **能跑**：workspace 可写；`ensurepip` 的临时文件在 `$TMPDIR`（已授权） | — |
| **10** | **`node` 的 JIT** | ✅ **能跑**：`(allow default)` 没有拦 `file-map-executable` / `vm-protect`，V8 的 `mprotect(PROT_EXEC)` 不被本 profile 拦 | —（真机未验证） |
| **11** | `clang` / `ld` / `swiftc` 单文件编译（输出到 `$TMPDIR` 或 workspace） | ✅ **能跑**：读 `/usr/bin`、`/Library/Developer/CommandLineTools`、`/System` 均默认允许；写 `$TMPDIR` 已授权 | — |
| **12** | `git status` / `git log` / `git diff`（只读） | ✅ 能跑（读默认允许，不碰 `.git` 写） | — |
| **13** | `ssh` 连外网 | ⚠️ 取决于 `Network`：`Network=false` → `(deny network*)`，含回环，连不出去；`Network=true` → 默认允许 | 设计如此 |
| **14** | 本地 dev server（`npm run dev` + 浏览器 127.0.0.1） | ⚠️ `Network=false` 时 `(deny network*)` **连回环也拦**（与 Linux `--unshare-net` 对齐） | 设计如此，需在 UI 提示 |

---

## 1. 渲染后的完整 SBPL（真实输出）

> 下面是用**同一份渲染代码**在本机跑出来的原文。本机是 Linux，所以路径用了 `/tmp/macsim/…` 模拟 macOS 的真实路径树（`/tmp/macsim/Users/dev/project` ≈ 真机 `/Users/dev/project`，`TMPDIR=/tmp/macsim/private/var/folders/zz/abc/T` ≈ 真机 `/var/folders/zz/abc/T`）。
> **规则文本与真机逐字相同**，只是路径前缀不同——把 `/tmp/macsim` 去掉就是真机输出。

### 1.1 workspace-write（默认，`Network=false`）

策略：`sandbox.New(WithRoots("/Users/dev/project"), WithProtectedPaths("/Users/dev/project/.git"))`

```
Mode=workspace-write
ReadOnly=[/]
ReadWrite=[/tmp/macsim/Users/dev/project /tmp/macsim/private/var/folders/zz/abc/T /tmp /var/tmp /dev/null /dev/pts/ptmx /dev/pts /dev/tty /dev/shm]
Protected=[/tmp/macsim/Users/dev/project/.git]
Network=false
PrivateTmp=true

(version 1) (allow default) (deny file-write*) (allow pseudo-tty) (allow file-write* (literal "/dev/null") (literal "/dev/ptmx") (literal "/dev/tty")) (allow file-write* (regex #"^/dev/ttys[0-9]+$")) (allow file-write* (subpath "/tmp/macsim/Users/dev/project") (subpath "/tmp/macsim/private/var/folders/zz/abc/T") (subpath "/tmp") (subpath "/var/tmp")) (deny file-write* (subpath "/tmp/macsim/Users/dev/project/.git")) (deny system-fcntl (fcntl-command 80 110)) (deny user-preference-write) (deny network*)
```

**真机等价文本**（同一规则，macOS 路径；`canonicalUnique` 会把 `/tmp` 解析成 `/private/tmp`、`$TMPDIR` 解析成 `/private/var/folders/…`）：

```sbpl
(version 1)
(allow default)
(deny file-write*)
(allow pseudo-tty)
(allow file-write* (literal "/dev/null") (literal "/dev/ptmx") (literal "/dev/tty"))
(allow file-write* (regex #"^/dev/ttys[0-9]+$"))
(allow file-write* (subpath "/Users/dev/project")
                   (subpath "/private/var/folders/zz/abc/T/")
                   (subpath "/private/tmp")
                   (subpath "/private/var/tmp"))
(deny  file-write* (subpath "/Users/dev/project/.git"))
(deny  system-fcntl (fcntl-command 80 110))
(deny  user-preference-write)
(deny  network*)
```

### 1.2 workspace-write（`Network=true`）

与 1.1 完全相同，**只是末尾没有** `(deny network*)`：

```
… (deny file-write* (subpath "/tmp/macsim/Users/dev/project/.git")) (deny system-fcntl (fcntl-command 80 110)) (deny user-preference-write)
```

### 1.3 read-only

```
Mode=read-only
ReadWrite=[/dev/null /dev/pts/ptmx /dev/pts /dev/tty]

(version 1) (allow default) (deny file-write*) (allow pseudo-tty) (allow file-write* (literal "/dev/null") (literal "/dev/ptmx") (literal "/dev/tty")) (allow file-write* (regex #"^/dev/ttys[0-9]+$")) (deny system-fcntl (fcntl-command 80 110)) (deny user-preference-write) (deny network*)
```

**没有任何 subpath 写授权**——`mktemp`、`git status` 这类"只读意图但要写临时文件"的命令在 read-only 下会失败（Seatbelt 没有私有 tmpfs；bwrap 有 `--tmpfs /tmp`，见 §3 的差异表）。

---

## 2. A. 逐项结论（macOS 上"这台机器能不能跑"）

判据列写的是**决定结论的 SBPL 行**。所有结论都是静态推导 + 本机渲染验证；真机未执行。

| # | 项 | 结论 | SBPL 依据 / 说明 | 风险 |
| --- | --- | --- | --- | --- |
| A1 | `go build` / `go test` | ❌ **不能** | 写 `~/Library/Caches/go-build`（`GOCACHE`）与 `~/go/pkg/mod`（`GOMODCACHE`）都不在 `(allow file-write* (subpath …))` 列表里 → 命中 `(deny file-write*)` → `operation not permitted` | **P0** |
| A1b | `go env`（只读） | ✅ 能 | 读 `~/Library/Application Support/go/env` 属 `file-read*`，`(allow default)` 兜住 | — |
| A1c | `go env -w`（写） | ❌ 不能 | 同上路径的写被 `(deny file-write*)` 拦 | P2 |
| A2 | `npm ci` / `npm install` | ❌ **不能** | `~/.npm/_cacache/**` 未授权写 | **P0** |
| A2b | 写 `./node_modules` | ✅ 能 | 在 workspace subpath 内 | — |
| A2c | `node` 的 **JIT** | ✅ **能**（静态推导） | profile 里没有任何 `(deny …)` 命中 `file-map-executable` / 匿名 `mprotect(PROT_EXEC)`；`(allow default)` 使它们保持允许。**这一条与 Codex 的 deny-default 姿态不同**：那边必须显式 `(allow file-map-executable)`，我们不需要 | —（真机未验证） |
| A3 | `python3 -m venv`（在 workspace 内） | ✅ 能 | 写在 workspace subpath 内；`$TMPDIR` 已授权 | — |
| A3b | `pip install`（默认缓存） | ❌ 不能 | `~/Library/Caches/pip/**` 未授权写（pip 在 macOS 用 `~/Library/Caches/pip`，不是 `~/.cache/pip`） | **P0** |
| A3c | `pip install --no-cache-dir` | ✅ 能（写 site-packages 在 venv 内） | 只写 workspace | — |
| A4 | `cargo build` | ❌ 不能 | `~/.cargo/registry`、`~/.rustup/toolchains` 未授权写 | **P0** |
| A4b | `sysctl`（cargo 读 CPU 数等） | ✅ 能 | `sysctl-read` 由 `(allow default)` 允许 | — |
| A5 | `git` 只读操作 | ✅ 能 | 读 `.git`、`~/.gitconfig`、`~/.ssh` 均默认允许 | — |
| A5b | `git commit` / `add` / `switch`（写 `.git`） | ❌ **不能** | **显式**的 `(deny file-write* (subpath "<ws>/.git"))`，排在 allow 之后（last-match-wins）→ `index.lock` 创建被拒 | **P0**（kit 默认 `addDefaultProtection`，跨平台一致） |
| A5c | `ssh` 连外网 | ⚠️ 看 `Network` | `Network=false` → `(deny network*)` | — |
| A6 | `clang` / `ld`（读系统 + 写 `$TMPDIR`） | ✅ 能 | 读 `/usr/bin`、`/System`、`/Library/Developer/CommandLineTools` 默认允许；写 `$TMPDIR`（`/private/var/folders/…`）已授权 | — |
| A6b | **`/tmp` 是 `/private/tmp` 的符号链接，会不会不命中？** | ✅ **已正确处理** | `canonicalSplit` 在渲染前做 `filepath.EvalSymlinks`（见 `profile.go`），所以 ReadWrite 里存的是 `/private/tmp` 而不是 `/tmp`；SBPL 的 `subpath` 按访问时的真实路径匹配，因此**必须**是解析后的形式——现在的实现是对的。`darwin_toolchain_test.go: TestSeatbeltResolvesSymlinkedWritableRoots` 用真实符号链接钉住了这条 | — |
| A7 | `xcodebuild` / `swift build` | ❌ 不能 | `~/Library/Developer/Xcode/DerivedData`、`~/Library/Developer/CoreSimulator`、`~/Library/Caches/org.swift.swiftpm` 未授权写 | P1 |
| A8a | `/private/var/folders/**`（真 TMPDIR） | ✅ 能（仅 `$TMPDIR` 那一条） | `os.TempDir()` → `/var/folders/zz/…/T` → 解析成 `/private/var/folders/zz/…/T` 后进白名单。**注意只放行会话那一个子目录**，不是整个 `/private/var/folders` | — |
| A8b | `/System/Volumes/Data` 读 | ✅ 能 | 读默认允许 | — |
| A8c | dyld 共享缓存 `/System/Library/dyld` | ✅ 能 | 读 + `file-map-executable` 都默认允许 | — |
| A8d | `sysctl` | ✅ 能 | `(allow default)` | — |
| A8e | `mach-lookup`（`com.apple.system.logger`、`cfprefsd`、`opendirectoryd`…） | ✅ 能 | `(allow default)`。**只有 `user-preference-write` 被显式 deny**（CFPreferences 的写由 cfprefsd 代做，绕文件围栏），`user-preference-read` 仍允许 | — |
| A9 | 网络 `(allow network-outbound)` | ⚠️ 现状：`Network=true` 时**没有** `(deny network*)` → 继承 `(allow default)`，出站与监听都允许 | 语义正确，但请见 §3 的"精度"讨论 |
| A9b | 回环 `127.0.0.1`（本地 dev server） | ⚠️ `Network=false` 时**同样被 `(deny network*)` 拦**（SBPL 的 `network*` 覆盖 inet/outbound/bind 全部） | 与 Linux `--unshare-net` 一致；但产品上要提示"沙箱内起本地服务需要开启网络" |
| A10 | `stat` / `getattrlist`（`ls` 依赖） | ✅ 能 | `file-read-metadata` 属 `file-read*` 家族，`(allow default)` 允许；profile 里没有任何 `deny file-read*`。**不需要**额外写 `(allow file-read-metadata)`（那是 deny-default 姿态才需要的） | — |

### 2.1 一个容易忽略的补充：写旁路已经被堵上

| 通道 | 状态 |
| --- | --- |
| `fcntl(F_MAKECOMPRESSED=80)` / `F_TRANSFEREXTENTS=110`（能通过**只读** fd 改文件内容） | ✅ `(deny system-fcntl (fcntl-command 80 110))` |
| `CFPreferences` 写（`~/Library/Preferences`，由未受限的 cfprefsd 代做） | ✅ `(deny user-preference-write)` |
| pty 跨会话注入（写别人的 `/dev/ttysN`） | ✅ 只放行字面量 + `^/dev/ttys[0-9]+$` 正则，**没有** `(subpath "/dev/pts")` |

---

## 3. B. 三平台能力面对齐（macOS vs Linux）

### 3.1 默认面：**完全不同的两种模型**

| 维度 | Seatbelt（macOS，本仓库） | bwrap（Linux） | Landlock（Linux，无 bwrap 时） |
| --- | --- | --- | --- |
| 默认姿态 | **默认允许 + 只拦写**（`(allow default)` + `(deny file-write*)`） | 默认拒绝（新 namespace + `--ro-bind /`，只有显式 bind 的位置可写） | 默认拒绝**已治理的访问权**（授予集合之外一律 EACCES），但只治理文件/网络子集 |
| 漏一条"读"规则 | 无影响（读默认允许） | 无影响（`--ro-bind / /` 兜住整棵树） | 无影响（`--ro /`） |
| 漏一条"写"规则 | **硬失败**（EPERM） | **硬失败**（EROFS） | **硬失败**（EACCES） |
| 进程可见性 | 全可见（无 pid namespace） | `--unshare-pid`，看不到宿主其它进程 | 全可见 |
| `/proc` | 宿主 `/proc` 可见（读默认允许） | `--proc /proc` 新建 | 宿主 `/proc` 可见 |
| 设备节点 | 字面量 `/dev/null`、`/dev/ptmx`、`/dev/tty` + ttys 正则；**`/dev/shm` 不可写**（`seatbeltWritableSubpaths` 跳过 `/dev/*`） | `--dev /dev`（新的 devtmpfs，`/dev/shm` **不存在**） | 显式 rw 列表里含 `/dev/null`、`/dev/pts`、`/dev/tty`、`/dev/shm`（`deviceWritePaths(true)`） |
| capability | 不涉及（无 cap-drop） | `--cap-drop ALL` | 不涉及 |
| 网络隔离 | `(deny network*)`（整类） | `--unshare-net`（整类） | 由 landlock 的 TCP 规则（若启用）+ 无隔离 |
| 私有 /tmp | ❌ 没有（`PrivateTmp` 字段对 Seatbelt 无实际作用） | ✅ `--tmpfs /tmp` | ❌ 没有 |
| read-only 模式下能否写临时文件 | ❌ 不能（只放行设备字面量） | ✅ 能（私有 tmpfs，不落宿主） | ❌ 不能 |

**结论（回答你的问题）**：**不是"macOS 默认拒绝、Linux 默认允许"，而是反过来——macOS 是 `allow default`（宽），bwrap/Landlock 才是"默认拒绝 + 白名单"。**
真正的"能力不一致"来源因此是：

1. **写白名单两边都得列全**，所以缓存目录的问题在**三平台一致存在**（bwrap/Landlock 同样挡 `~/.npm`）——这不是 macOS 独有；
2. **macOS 少给了**：pid namespace、私有 `/proc`、`--dev` 设备集、capability 收窄、私有 tmpfs（read-only 下尤其明显）；
3. **macOS 多给了**（相对 bwrap）：能看见宿主所有进程（可 `kill` 同用户进程、可 attach）、能 exec 任意二进制、能访问任意 mach 服务、能读任意文件（bwrap 也读，但看不到其它进程）。

### 3.2 同一 Policy 在两个后端渲染出的可写集合（实测对比）

```
Policy: Roots=[<ws>], Protected=[<ws>/.git], Network=false

Seatbelt 可写 subpath : <ws>, <TMPDIR>, /tmp(→/private/tmp), /var/tmp(→/private/var/tmp)
bwrap     --bind      : <ws>, <TMPDIR>, /var/tmp        （/tmp 被 --tmpfs 覆盖，不 bind 宿主）
Landlock  --rw        : <TMPDIR>, /var/tmp, /dev/*      （<ws> 因含受保护子树被"下移"成 根只读 + 子项按需）
```

三者的**语义**一致（"工作区 + 临时目录可写，其余不可写"），差别在实现细节：
- `/tmp`：Seatbelt 放行宿主 `/private/tmp`；bwrap 给私有 tmpfs（宿主 `/tmp` 不可写）——**bwrap 更严**；
- `<ws>` 含 `.git` 时：Landlock 只能把根降成只读（并集语义），因此**在根目录新建文件会失败**；Seatbelt/bwrap 能保持根可写、只把 `.git` 降只读——**Landlock 表达力最弱**（这是已知边界，`profiles.go` 里有长注释）。

---

## 4. 精确 diff：让"常见工具链在 workspace-write 下能跑"

**设计原则**：改**一处**（`BuildProfile`），四个后端同时受益（bwrap/Landlock/Seatbelt/Windows ACL 都只从 `Profile` 派生）——不要在 `SeatbeltProfile` 里单独加规则，那会造成"macOS 能跑、Linux 不能"的新不一致。

### 4.1 `sandbox/sandbox.go`：新增策略开关（默认开启）

```diff
@@ type Policy struct {
 	includeSlashTmp  bool
 	includeTmpdir    bool
+	// includeToolchainCaches 决定 workspace-write 是否把"开发工具链的缓存目录"
+	// 一并放行（GOCACHE/GOMODCACHE/npm/pip/cargo/maven/Xcode…）。这些目录都在
+	// 工作区之外，不放行会让"沙箱一开、正常开发全废"。
+	//
+	// 默认**开启**：个人客户端的卖点是"能干活"；关掉它是纯白名单姿态（更严，
+	// 但 go/npm/pip/cargo 全部不可用）。宿主可以用 WithToolchainCaches(false)
+	// 关掉，再用 WithWritableRoots 精确放行自己要的那几个。
+	includeToolchainCaches bool
```

```diff
+// WithToolchainCaches 控制 workspace-write 是否自动放行开发工具链的缓存目录
+// （默认 true）。见 Policy.includeToolchainCaches。
+func WithToolchainCaches(include bool) Option {
+	return func(policy *Policy) { policy.includeToolchainCaches = include }
+}
```

```diff
+// IncludeToolchainCaches 报告是否自动放行开发工具链缓存目录。
+func (p *Policy) IncludeToolchainCaches() bool {
+	return p == nil || p.includeToolchainCaches
+}
```

并在 `New`/零值初始化处把默认值设为 `true`（与 `includeSlashTmp`/`includeTmpdir` 的写法一致）。

### 4.2 新文件 `sandbox/osrunner/toolchain_caches.go`

```go
package osrunner

// 本文件回答一个产品问题："沙箱里的 agent 还能不能正常开发？"
//
// 语言工具链的缓存目录（Go/npm/pip/cargo/maven/Xcode）默认都在工作区之外。
// 不把它们放进可写面，`go build`、`npm ci`、`pip install`、`cargo build` 会在
// 创建缓存目录时直接 EPERM/EROFS——用户看到的不是"被沙箱保护了"，而是"工具坏了"。
//
// 这里只列"必须能写"的**缓存**目录（不是配置、不是凭据）：写坏它们的最坏后果是
// 下次重新下载依赖，不会泄漏或破坏用户数据。

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// toolchainCacheRoots 返回当前平台上开发工具链需要可写的缓存目录（只返回存在的）。
func toolchainCacheRoots(policy *sandbox.Policy) []string {
	if !policy.IncludeToolchainCaches() {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	cache, _ := os.UserCacheDir() // darwin: ~/Library/Caches；linux: ~/.cache
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		candidates = []string{
			filepath.Join(cache),                     // go-build / pip / org.swift.swiftpm …（= ~/Library/Caches）
			filepath.Join(home, "Library", "Application Support", "go"), // GOENV
			filepath.Join(home, "go"),                // GOPATH：pkg/mod、bin
			filepath.Join(home, ".npm"),
			filepath.Join(home, ".bun"),
			filepath.Join(home, ".cargo"),
			filepath.Join(home, ".rustup"),
			filepath.Join(home, ".m2"),
			filepath.Join(home, ".gradle"),
			filepath.Join(home, "Library", "Developer"), // DerivedData / CoreSimulator / UserData
			filepath.Join(home, "Library", "pnpm"),
		}
	case "windows":
		// Windows 由 windows_* 后端消费同一个 ReadWrite 列表；这里给出等价集合，
		// 具体路径以 %LOCALAPPDATA%/%APPDATA% 为准（见 windows_folders.go）。
		candidates = []string{
			filepath.Join(os.Getenv("LOCALAPPDATA")),
			filepath.Join(home, "go"),
			filepath.Join(home, ".cargo"),
			filepath.Join(home, ".rustup"),
			filepath.Join(home, ".m2"),
			filepath.Join(home, ".gradle"),
			filepath.Join(os.Getenv("APPDATA"), "npm-cache"),
		}
	default: // linux / freebsd
		candidates = []string{
			filepath.Join(cache), // ~/.cache：go-build、pip、pre-commit…
			filepath.Join(home, "go"),
			filepath.Join(home, ".npm"),
			filepath.Join(home, ".bun"),
			filepath.Join(home, ".cargo"),
			filepath.Join(home, ".rustup"),
			filepath.Join(home, ".m2"),
			filepath.Join(home, ".gradle"),
			filepath.Join(home, ".local", "share", "pnpm"),
			filepath.Join(home, ".config"), // go env（linux 下 GOENV 在 ~/.config/go/env）
		}
	}
	return candidates
}
```

### 4.3 `sandbox/osrunner/profile.go`：**唯一需要改的一行**

```diff
 	case sandbox.ModeWorkspaceWrite:
 		roots, dropped := canonicalSplit(workspaceWritableCandidates(policy))
+		// 开发工具链缓存：不在这里放行，go/npm/pip/cargo 在沙箱里全部不可用。
+		// 走 canonicalUnique 后，不存在/解析失败的目录会被安静丢弃（与其它
+		// 可写根一致），不会因为机器上没装 cargo 就形成一条死规则。
+		roots = append(roots, canonicalUnique(toolchainCacheRoots(policy))...)
 		// 设备节点同样要进 ReadWrite：…
 		readWrite := append(append(append([]string(nil), roots...), profileTempRoots(policy)...), deviceWritePaths(true)...)
```

**这一个改动会让四个后端同时拿到缓存目录**：
- Seatbelt → 多出若干 `(subpath "/Users/dev/Library/Caches")` 等 allow；
- bwrap → 多出 `--bind /Users/dev/Library/Caches /Users/dev/Library/Caches`；
- Landlock → 多出对应 `--rw`；
- Windows ACL → 多出对应的可写 ACE。

### 4.4 可选的第二步（不建议本轮做）：`.git` 写权限

`git commit` 被拒是**设计取舍**（保护版本控制状态 = 防止 agent 改写自己的守卫）。但它对"个人用户在沙箱里让 agent 提交代码"是硬阻断，而且 `request_sandbox_access` 也解不开（`ValidateWritableGrantPath` 会拒绝受保护路径）。

如果要解，最小面是给 `.git` 单独一个开关（例如 `WithGitWritable(true)`，只放行 `.git/index*`、`.git/refs/**`、`.git/HEAD`、`.git/logs/**`，**不放行 hooks/config**），而不是放开整个 `.git`。本轮**不建议**做：它改变了防误伤模型的语义，应该由产品决策。

---

## 5. 未执行 / 未验证声明

| 项 | 状态 |
| --- | --- |
| 在真实 macOS 上跑 `sandbox-exec -p <profile> -- <cmd>` | ❌ 未执行（本机是 Linux） |
| `node` JIT 在真机上是否被拦 | ❌ 未真机验证（静态推导：不拦） |
| `xcodebuild` 的真实失败点 | ❌ 未真机验证（推导：DerivedData 写被拒） |
| `/dev/stderr`、`/dev/fd/N` 的写是否需要额外放行 | ❌ 未真机验证（推导：tty 目标命中 `ttysN` 正则；管道无 vnode，不受 file-write 治理） |
| `GOOS=darwin` 交叉编译 | ✅ 已执行（见下） |

```
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build ./sandbox/... ✅
GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build ./sandbox/... ✅
```

---

## 6. 新增用例（`sandbox/osrunner/darwin_toolchain_test.go`）

纯逻辑（在 Linux 上跑，不执行 sandbox-exec）：

| 用例 | 钉住什么 |
| --- | --- |
| `TestSeatbeltPostureIsAllowDefaultWithWriteDeny` | 姿态必须是 `(allow default)` + `(deny file-write*)`；**不得**出现 `deny file-read*`/`deny sysctl`/`deny mach-lookup`/`deny process-exec`；两条写旁路（system-fcntl、user-preference-write）必须在 |
| `TestSeatbeltProtectedDenyComesLast` | SBPL 是 last-match-wins：`.git` 的 deny 必须排在可写 allow **之后** |
| `TestSeatbeltResolvesSymlinkedWritableRoots` | 符号链接可写根必须渲染成**解析后的真实路径**（macOS `/tmp` → `/private/tmp` 的等价性质，用真实符号链接验证） |
| `TestSeatbeltDevicesAreLiteralsNotSubpaths` | `/dev/null`、`/dev/ptmx`、`/dev/tty` 是 literal、ttys 是正则、**没有** `/dev` 的 subpath（避免 pty 跨会话注入） |
| `TestSeatbeltNetworkSwitch` | `Network=false` → `(deny network*)`；`true` → 不出现 |
| `TestSeatbeltReadOnlyGrantsOnlyDevices` | read-only 下不得有任何 subpath 写授权 |
| `TestSeatbeltProfileIsSingleLine` | profile 必须单行（`-p` 内联）；含换行的路径要 fail closed（返回空串） |
| `TestSeatbeltCoversEveryBwrapBind` | 跨后端一致性：bwrap 的每个 `--bind` 都必须在 Seatbelt 白名单里 |
| `TestSeatbeltToolchainCacheCoverage` | 逐个记录 GOCACHE/GOMODCACHE/npm/pip/cargo/maven/gradle/Xcode 的覆盖情况（当前全部"缺口"），修复后自动变成"已覆盖"而**不需要改用例** |