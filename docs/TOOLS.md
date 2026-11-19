# Rig 工具目录

本文回答三个问题：这个 SDK 到底暴露了哪些工具？每个工具在什么装配条件下存在、受什么权限与平台约束？"39 / 81 / 83" 这几个数字分别在数什么？读者是准备装配 agent 的宿主开发者、需要调用工具的模型侧提示词作者，以及给这个仓库加工具的人。

本文件是**人读**的工具目录。机器可读的等价物是 [docs/ai/tools.json](ai/tools.json)（每个工具的完整 JSON Schema）与 [docs/ai/tools.md](ai/tools.md)（表格速览），二者都由 `go run ./cmd/rigdoc` 从真实构造器生成，**不要手改**。覆盖门禁（"有没有漏掉什么？"）由两条命令执行：

```sh
go test ./tools/full -run TestToolCatalogCoverage -v
go test ./tools/audit/... -v
```

账本本身、门禁细节与"新增工具"清单见 [COVERAGE.md](COVERAGE.md)。

工具名、参数名、模型可见的描述与错误文案都是**接口**：本文用中文解释它们，但不翻译模型实际看到的英文文案。

## 1. 计数口径

这些数字回答不同的问题，混用就会自相矛盾。下表每个数字都标了口径：

| 口径 | 数字 | 回答的问题 |
| --- | --- | --- |
| `kit.New(kit.Config{})` 零配置**注册** | **39** | 装了 SDK、什么依赖都不接，能列出多少个工具？ |
| 同上**初始 active**（`ToolDiscovery` 默认开） | **16** | 第一次请求里实际发给模型多少个 schema？ |
| 同上 + `Config.Presenter` | **40 / 17** | 接了交付面板后注册数与 active 数 |
| 全接线（Linux、git 仓库、PTC、`Presenter`、personal 预设） | **50 / 17** | 宿主把每项能力都接上之后 |
| `full.All(AllOptions{Root, Policy})` | **81** | 完整静态工具集，**不含**两个沙箱授权工具 |
| 同上 + `SandboxGrants: true` | **83** | 与账本对齐的完整静态工具集 |
| `full.All(full.AllOptions{})`（无 `Root`/`Policy`） | **78** | 零值陷阱，见 §1.2 |
| `catalog.Static` / `presets["all"]` | **83** | 账本口径与 `all` 预设 |
| `presets["standard"]` / `presets["personal"]` / `presets["minimal"]` | **73 / 55 / 8** | 名字级过滤后的预设 |
| `catalog.Legacy` / `catalog.Modern` / `catalog.ServerBound` / `catalog.DocumentedGaps` | **58 / 62 / 7 / 0** | 账本的四个审计列表 |
| `docs/ai/tools.json` 条目 | **84** | 83 个静态工具 + opt-in `run_code` |
| 动态名字模式 | `mcp__<server>__<tool>` | 由每个 MCP server 自己的工具表在运行期生成，不计数 |

为什么 `full.All` 是 81 而账本是 83：`sandbox_status` 与 `request_sandbox_access` 只在 `AllOptions.SandboxGrants`（或 `kit.Config.SandboxGrants`）打开时构造；`kit` 默认打开它（只注册、不进初始 active 集），覆盖门禁也显式打开它，所以账本按 83 计。`full.All` 的调用方不传这个选项时只有 81。

### 1.1 39 与 16 的关系

零配置注册 39 来自 `personal` 预设 55，按"零配置体检"逐步减出来的（下一节逐项列出）。39 个工具里只有 16 个是初始 active：核心 13 + `tool_search` + `memory` + `skill`。其余 23 个"注册但不 active"，模型需要时用 `tool_search` 自己加载——这是动态工具加载，见 §3。

接上 `kit.Config.Presenter` 后，`present` 不再被摘掉、并立刻进入初始 active 集，于是 40 注册 / 17 active。

### 1.2 零值陷阱：`full.All(full.AllOptions{})` 只有 78

`apply_patch`、`search`、`undo` 是**工作区级**工具，构造时必须知道工作区根；既不传 `Root` 也不传带 `Roots` 的 `Policy` 时，这三个工具**不会构造**（不是报错，是静默缺席），其余文件工具每次调用都返回 `sandbox: denied: no roots configured`。所以组装 `full.All` 时**必须**传 `Root` 或带 `Roots` 的 `Policy`。

## 2. 零配置体检：personal 的 55 如何变成 39

`kit.New` 的默认预设是 `personal`（55 个名字）。它先被剪两刀，再按宿主依赖逐项加回。

### 2.1 先减：55 → 39

| 步骤 | 数量 | 工具 |
| --- | --- | --- |
| `personal` 预设 | 55 | — |
| 没有回合快照 tracker | −1 | `undo` |
| 零配置体检 | −15 | 见下表 |
| **零配置注册** | **39** | |

零配置体检的原则是"默认工具集里每个工具都必须真的能用"：装配缺失、调用必然失败的工具不留给模型（否则只会诱导它反复重试）。被摘掉的 15 个：

| 工具 | 摘除原因（缺失的装配条件） |
| --- | --- |
| `web_search` | `Config.Searcher` |
| `session_search`、`session_trace`、`session_event_read`、`session_event_search`、`session_event_trace` | `Config.SessionLog`（共 5 个） |
| `get_goal`、`create_goal`、`update_goal`、`plan_review`、`exit_plan_mode` | `kit` 没有 `GoalStore` / `PlanController` 的注入入口（共 5 个，恒被摘） |
| `present` | `Config.Presenter`（`rig.FilePresenter`） |
| `ask_user` | `Config.Asker` |
| `web_fetch` | `AllowNetwork=true`（默认 `false`） |
| `pwsh` | 平台过滤：`kit` 只在 Windows 的默认集里保留它（其他平台按"没有 PowerShell"处理） |

### 2.2 再加：宿主每接一项就加回

| 宿主接上 | 加回的工具 | 累计注册 |
| --- | --- | --- |
| （零配置起点） | — | 39 |
| `Config.Asker` | `ask_user` | 40 |
| `Config.Searcher` | `web_search` | 41 |
| `Config.SessionLog` | 5 个 `session_*` | 46 |
| `AllowNetwork: true` | `web_fetch` | 47 |
| `Config.Presenter` | `present`（同时进初始 active 集） | 48 |
| `GitUndo: true` 且工作区是 git 仓库 | `undo` | 49 |
| `PTC: true` | `run_code`（不在 83 个静态工具里） | 50 |

全接线公式：`personal` 55 − 5（`kit` 无 goal/plan 注入入口）− 1（非 Windows 无 `pwsh`）= 49，再加 `PTC` 的 `run_code` = **50**（Linux，含 PTC；active 仍为 17）。Windows 上 `pwsh` 加回：含 PTC 是 **51**，不含 PTC 是 **50**。`undo` 只有在 `GitUndo`（或宿主持有 `UndoTracker`）且工作区确实是 git 仓库时才计入；工作区不是 git 仓库时该开关被静默忽略，工具不出现。

> 宿主自己传 `Config.Tools` 时，预设不再生效（工具集是宿主的选择）；`ToolDiscovery` 也默认关闭（宿主挑出来的工具就该直接能用），需要时显式写 `ToolDiscovery: kit.Bool(true)`。

## 3. 动态工具加载

默认开启（`kit.Config.ToolDiscovery`，`nil` = 开）。思路是"默认只发常用的十几个，其余注册但不发；模型需要时自己加载"。

### 3.1 初始 active 的 16 个

核心 13：

`read_file`、`write_file`、`edit_file`、`apply_patch`、`list_dir`、`glob`、`grep`、`search`、`bash`、`todo_write`、`working_directory`、`read_image`、`str_replace_editor`

另有三个**无条件常驻**：`tool_search`（发现入口）、`memory`（用户的长期记忆）、`skill`（技能清单，schema 很小且"有哪些技能"是开工前就该知道的信息）。它们也不会出现在 `tool_search` 的清单里。

### 3.2 `tool_search` 的用法

| 调用 | 行为 |
| --- | --- |
| `{}` | 列出**尚未激活**的工具（默认最多 40 条）；每项只有名字与一句话摘要 |
| `{"query": "pdf"}` | 按名字/描述子串筛选（大小写不敏感） |
| `{"activate": ["a", "b"]}` | 激活给定名字，返回 `activated` / `unknown`；激活是**纯增量**，已激活的名字重复请求会被忽略 |

schema 刻意只有两个可选字段（`query`、`activate`），数组元素是字符串——它自己每轮都要占 token。

### 3.3 三条缓存契约

1. **纯增量、只追加**：激活只把新工具追加到 `Request.Tools` 末尾（初始集按注册顺序，之后按激活顺序）；已激活部分逐字节稳定，**激活到全集也不会重排**。
2. **按会话**：active 集合是 per-session 的，同一个 agent 可以同时服务多个会话而互不影响。宿主侧用 `(*agent.Agent).ActivateTools(sessionID, ...)` 或 `(*kit.Agent).ActivateTools` 激活；事件 `tool.activated` 的 `trigger` 区分 `model` 与 `host`。
3. **不重建 prompt**：system prompt 里不含工具描述，激活只改 `Request.Tools` 这一个字段。不激活的轮次 tools 数组逐字节稳定，prefix cache 持续命中；激活那一轮前缀断一次，之后恢复。

### 3.4 怎么关

```go
off := false
kit.New(kit.Config{Model: "…", ToolDiscovery: &off})
```

关闭后所有已注册工具都是 active（回到 v0.20 的工具面），并且 `tool_search` **不注册**——不加载新工具了，发现入口也不该占位。

### 3.5 自定义初始集合

`kit.Config.InitialActiveTools` 指定自己的初始活跃集合；`tool_search` / `memory` / `skill` 会被**强制并入**（否则模型无法再发现工具，或者丢掉常驻能力）。宿主也可以直接调用 `Agent.ActivateTools` 从外部激活（例如 UI 上的"启用这个工具"开关）。

## 4. 静态工具目录（83 个）

下面按类别列出 `catalog.Static` 的全部 83 个名字。`availability` 一列的含义：

- `static`：`full.All` 总会构造（需要 `Root`/`Policy` 的已注明）。
- `alias`：旧版兼容别名，与某个现代工具同义；`standard`/`personal` 预设剔除，只有 `all` 预设保留。
- `opt-in`：只在宿主显式开启某项配置时出现；唯一的例子是 `kit.Config.PTC` 的 `run_code`，它不在 83 个静态工具里，见 §5.3。
- `server-bound`：需要宿主注入外部 client 才构造（本表不出现，见 §5）。

权限列的"审批"指 `rig.Gate` 的判定，不是工具自己弹窗。各权限模式对整类工具的影响集中在 §6。

### 4.1 文件与编辑（11）

| 工具 | 包 | availability | 作用 | 关键参数 / 上限 / 权限影响 |
| --- | --- | --- | --- | --- |
| `read_file` | `tools/builtin` | static | 读 UTF-8 文本文件并返回带行号的内容 | `path`；`offset`（1 基行）、`limit`（最大行数）。上限 2000 行、单行 2000 B、正文 256 KiB。只读，免审批；根外路径拒绝 |
| `write_file` | `tools/builtin` | static | 创建或覆盖文本文件，自动创建缺失的父目录 | `path`、`content`。`read-only` 模式直接拒绝；写文件需审批；不跟随根外符号链接 |
| `edit_file` | `tools/builtin` | static | 精确替换文件中的一处文本 | `path`、`old_string`、`new_string`、`replace_all`。`old_string` 默认必须恰好出现一次，缺失/歧义都报错；>8 MiB 的文件拒绝编辑；`read-only` 拒绝 |
| `str_replace_editor` | `tools/editor` | static | 查看/创建/精确替换/插入文件的编辑器 | `command`（view/create/str_replace/insert）、`path`；view 的 `view_range` 是 1 基闭区间，`-1` 表示文件末尾；str_replace 默认唯一匹配。经策略解析路径，`read-only` 拒绝写命令 |
| `apply_patch` | `tools/patch` | static | 原子地应用多文件补丁（统一 diff 或 `*** Begin Patch`） | `patch`、`dry_run`。补丁 ≤1 MiB、单目标文件 ≤16 MiB、单补丁 ≤200 文件；全有或全无；上下文三级回退匹配，歧义拒绝；更新写穿符号链接，删除/移动作用于链接本身；**纯插入 hunk 不幂等**；必须传 `WithPolicy` 才遵守只读与可写根围栏；无 `Root`/`Policy` 时不构造 |
| `list_dir` | `tools/builtin` | static | 列出一个目录（目录在前、文件在后） | `path` 可选（默认工具根）；最多 500 项；被策略拒绝的路径直接隐藏 |
| `glob` | `tools/builtin` | static | 按 glob 模式找文件 | `pattern`、`path`；`*`/`?` 单段匹配，`**` 跨段；结果按路径排序，最多 500 条 |
| `grep` | `tools/builtin` | static | 用 Go 正则逐行搜索文件内容 | `pattern`、`include`（文件名 glob）、`path`；返回 `file:line:text`，最多 200 行、单行截断 500 B；>8 MiB 的文件跳过 |
| `search` | `tools/search` | static | 按相关性检索工作区（文件名模糊 / BM25 正文 / `@提及`） | `query`；`path`、`mode`（files/text/mentions/auto）、`max_results`（默认 20、上限 200）。跳过 `.git`、`node_modules`、`vendor`、`dist`、`build`、二进制与 >1 MiB 文件；单次预算 10s；无 `Root` 时不构造 |
| `read_image` | `tools/image` | static | 读取图片并以多模态工具结果返回 | `path`；支持 PNG/JPEG/GIF/WebP/BMP（按魔数识别）；默认 ≤10 MiB，超限拒绝而不截断；只读，`read-only` 下仍可用 |
| `undo` | `tools/undo` | static | 回退本回合的文件改动（面向用户的"撤销按钮"） | 无参数；只在 `GitUndo` 或宿主持有 `UndoTracker` 时进入 `kit` 默认集；git 隐藏快照（`git stash create` 语义，不动 HEAD/分支/暂存区），保留最近 128 份；模型只应在用户明确要求撤销时调用 |

### 4.2 shell / 进程 / 终端（13）

| 工具 | 包 | availability | 作用 | 关键参数 / 上限 / 权限影响 |
| --- | --- | --- | --- | --- |
| `bash` | `tools/builtin` | static | 用 `/bin/sh -c` 执行命令，合并 stdout/stderr | `command`；`timeout_ms`/`timeoutMs`、`workdir`、`run_in_background`、`description`、`sandbox_permissions` + `justification`。默认超时 30s、上限 10min；输出上限 32 KiB；`ask` 档下每次调用都问人（**不做**命令风险分类，`rm -rf /` 也一样），已沙箱化的命令免审批（§6.3）；`read-only` 禁用 shell；需要 unix `/bin/sh` |
| `pwsh` | `tools/builtin` | static | 用 PowerShell `-NoProfile -NonInteractive -Command` 执行脚本 | 参数同 `bash`；二进制按 PATH 探测 `pwsh` 再 `powershell`。`full.All` 总会构造；`kit` 只在 Windows 的默认集里保留它（非 Windows 上要用手动组装，并自行确保系统装有 PowerShell） |
| `terminal_open` | `tools/terminal` | static | 打开一个持久 shell 会话并返回 session id | `cwd`、`shell`（默认 Unix 为 `/bin/sh`、Windows 为 `powershell.exe`）、`type`（shell/pty/pipe）、`name`。Unix 默认真实 PTY，不可用时回退 pipe；`read-only` 拒绝打开 |
| `terminal_send` | `tools/terminal` | static | 向持久终端写文本并等待就绪 | `sessionId`、`text`；`submit`（默认 true，追加 Enter）、`timeout_ms`（默认 30s、上限 10min）、`run_in_background`（需后台管理器）。PTY 用静默期判定结束，pipe 用标记；每次发送都经策略校验该行 |
| `terminal_read` | `tools/terminal` | static | 读取保留的终端输出，不发送输入 | `sessionId`；`offset`/`count`（默认 500、上限 2000）分页，或 `wait_ms` 等待新输出 |
| `terminal_signal` | `tools/terminal` | static | 向终端前台进程组发一个允许的信号 | `sessionId`、`signal`（`SIGINT`/`SIGTERM`/`SIGKILL`/`SIGTSTP`/`SIGHUP`，也接受 `INT`/`TERM`/`KILL`/`HUP`/`QUIT` 与编号）；PTY 上 `INT` 以 Ctrl-C 字符交付 |
| `terminal_close` | `tools/terminal` | static | 关闭会话：杀进程组、回收 shell、释放缓冲 | `sessionId` |
| `terminal_list` | `tools/terminal` | static | 列出本管理器的终端会话 | 无参数；报告 session_id、transport（pty/pipe）、cwd、是否仍在运行、退出码 |
| `job_start` | `tools/jobs` | static | 启动后台命令并立即返回 job id | `command`、`cwd`、`timeout_ms`（正数到点杀死）。`read-only` 拒绝启动；命令与 cwd 经策略校验；任务输出进环形缓冲（默认每任务 1 MiB）。前台 `bash`/`pwsh` 超时会杀**整个进程组**（POSIX）；`!unix` 平台仍只杀直接子进程，需要进程树治理请注入 `sandbox/osrunner` |
| `job_output` | `tools/jobs` | static | 读后台任务输出（增量 / 尾部 / 字节偏移） | `job_id`；`wait`、`timeout_ms`（等待默认 30s、上限 10min）、`tail`、`offset`；默认返回自上次读取以来的新输出。响应头带 `output_offset`（本次起点）与 `next_offset`（续读起点，仅在达到单次 32 KiB 上限时出现，同时带 `output_truncated: true`），按 `next_offset` 连续读可拼回完整输出；环形缓冲已经丢掉更早字节时另带 `[truncated: N earlier bytes discarded]` |
| `job_kill` | `tools/jobs` | static | 取消后台任务并返回最终状态与输出尾部 | `job_id`、`reason`、`signal`（符号名或 POSIX 编号，缺省 KILL） |
| `job_list` | `tools/jobs` | static | 列出本管理器保留的后台任务 | 无参数；id、kind、state、退出码、时长、命令；已结束任务保留到宿主 `CloseAll` |
| `job_wait` | `tools/jobs` | static | 等待后台任务结束 | `job_id`、`timeout_ms`；超时返回当前状态且任务继续运行 |

### 4.3 web（2）

| 工具 | 包 | availability | 作用 | 关键参数 / 上限 / 权限影响 |
| --- | --- | --- | --- | --- |
| `web_fetch` | `tools/web` | static | 抓取 http(s) URL 并返回文本（或原始 HTML） | `url`；`format`（text/html）、`maxChars`（默认 20000、上限 200000）。总超时 30s、响应体 ≤2 MiB、重定向 ≤5 跳；内置私网/回环/链路本地/云元数据地址防护；需要 `AllowNetwork=true`，否则 `kit` 摘除该工具 |
| `web_search` | `tools/web` | static | 通过宿主注入的搜索后端检索网页 | `query` 或 `queries`（1–4 个）、`limit`（默认 5、上限 20）；未注入 `Searcher` 时返回配置错误，`kit` 会摘除该工具 |

### 4.4 会话与诊断（6）

| 工具 | 包 | availability | 作用 | 关键参数 / 上限 / 权限影响 |
| --- | --- | --- | --- | --- |
| `session_search` | `tools/session` | static | 搜索会话历史事件（大小写不敏感） | `query`、`limit`（默认 20、上限 100）；匹配会话 id、事件类型与载荷；只读，需 `SessionLog` |
| `session_trace` | `tools/session` | static | 按 seq 区间输出一个会话的有序事件轨迹 | `session_id`、`from`、`to`（`to=0` 表示直到结尾）；最多 100 条事件 |
| `session_event_read` | `tools/session` | static | 读取一条完整事件及其载荷 | `session_id`、`seq`；载荷 >4 KiB 时截断并提示 |
| `session_event_search` | `tools/session` | static | 与 `session_search` 类似，但每条命中附带载荷摘录 | `query`、`limit`；用于先定位再 `session_event_read` |
| `session_event_trace` | `tools/session` | static | 显示目标事件前后的上下文事件 | `seq`；`session_id` 可选（省略时按 seq 跨会话尽力解析）、`before`/`after`（默认 2、上限 10） |
| `inspect` | `tools/inspect` | static | 只读地诊断 agent 可见环境 | `action`（sessions/events/workspace/stats）、`session_id`、`limit`；单响应上限 32 KiB、载荷摘录 ≤200 B、workspace 最多列 12 个顶层条目 |

### 4.5 宿主交互 / 计划 / 目标（8）

这些工具依赖宿主接口；没有对应实现时返回可读的"宿主未提供"工具错误，不会 panic。

| 工具 | 包 | availability | 作用 | 关键参数 / 上限 / 权限影响 |
| --- | --- | --- | --- | --- |
| `ask_user` | `tools/hosttools` | static | 向人类提一个问题并返回答案 | `question`、`options`（可选的选项列表）。需要 `rig.Asker` 且 `Interactive=true`；非交互运行返回 "no interactive UI"；**默认不设超时**（`Config.AskTimeout>0` 才有上界） |
| `present` | `tools/hosttools` | static | 把已存在的文件声明为最终交付物 | `files`（1–8 个，每项 `path` + 可选 `description`）；相对路径按 `Root` 解析，只做存在性/类型检查，不复制内容；需要 `rig.FilePresenter`（`Config.Presenter`） |
| `plan_review` | `tools/hosttools` | static | 把计划提交给宿主评审并返回结论 | `content`；需要 `rig.PlanController`；`kit` 没有注入入口，零配置恒被摘 |
| `exit_plan_mode` | `tools/hosttools` | static | 批准/取消/查询 plan 模式状态 | `action`（approve/cancel/status）；需要 `rig.PlanController` |
| `goal` | `tools/hosttools` | alias | 旧版目标状态工具（set/get/clear/update） | `action`；`update` 支持 complete/blocked/pause/resume/edit（需 store 实现 `rig.GoalUpdater`）。由 `get_goal`/`create_goal`/`update_goal` 取代，`standard` 预设剔除 |
| `get_goal` | `tools/hosttools` | static | 读取当前目标（含 `update_goal` 需要的 id 与 revision） | 无参数；无目标时返回 `{"goal":null}`；需要带修订号的 `GoalRevisionStore` |
| `create_goal` | `tools/hosttools` | static | 创建一个跨续跑轮次持久化的目标 | `objective`、`max_goal_rounds`（默认 256） |
| `update_goal` | `tools/hosttools` | static | 按精确 revision 做 CAS 更新目标 | `goal_id`、`revision`、`action`；可选 `objective`、`max_goal_rounds`、`blocked_reason`。revision 过期会被拒绝而不是覆盖新状态 |

### 4.6 记忆 / 调度 / worktree（8）

| 工具 | 包 | availability | 作用 | 关键参数 / 上限 / 权限影响 |
| --- | --- | --- | --- | --- |
| `memory` | `tools/memory` | static | 按工作区分区的渐进式 wiki 长期记忆 | `action`（index/read/write/append/search/delete）、`name`、`query`、`summary`、`content`、`text`。`index` 只回 name/summary/年龄（≤60 页、绝不回正文），`read` 单页 ≤8 KiB，`search` 只回 meta（≤20 条）；`name` 限 `[a-z0-9-]{1,64}` 且静默不改写；默认文件 wiki 在用户状态目录（0700/0600），不写进仓库 |
| `schedule_create` | `tools/schedule` | static | 创建定时任务/提醒 | `prompt`；`name` 或 `title`（≤120 字符）、`enabled`、`schedule` 或一个类型化选择器（`after_seconds`/`at`/`every_seconds`/`daily`/`weekly`/`cron`）。工具只做 CRUD，不计算 next-run、不触发任务 |
| `schedule_list` | `tools/schedule` | static | 列出全部定时任务 | 无参数；ID/NAME/SCHEDULE/ENABLED/NEXT RUN |
| `schedule_update` | `tools/schedule` | static | 原地更新一个定时任务 | `id`；至少给一个 `name`/`title`/`prompt`/`enabled`/时间选择器；空 `schedule` 字符串表示清除 |
| `schedule_delete` | `tools/schedule` | static | 删除一个定时任务 | `id`；未知 id 报错 |
| `enter_worktree` | `tools/worktree` | static | 基于当前 HEAD 新建分支与 git worktree | `path`（必须尚不存在，省略时 `<repo>/.rig/worktrees/<branch>`）、`branch`。主沙箱根必须是 git 仓库根且至少有一个提交；未提交修改留在主检出；`read-only` 拒绝 |
| `exit_worktree` | `tools/worktree` | static | 移除由 `enter_worktree` 创建的 worktree | `path`、`keep_branch`（默认保留分支）；`read-only` 拒绝 |
| `list_worktrees` | `tools/worktree` | static | 列出仓库的 git worktree | 无参数；PATH/BRANCH/HEAD/STATE；主检出标 `main`，`.rig/worktrees` 下的标 `managed`，并标 detached/bare/locked/prunable；只读 |

### 4.7 编排与评审（28）

`personal` 预设不含任何多智能体编排工具（团队、subagent、workflow、ralph 都在 `personalExcluded` 里）；需要用 `standard`/`all` 预设 + 注入 `rig.SubagentRunner`。下表标 `alias` 的 8 个名字是旧版兼容别名，`standard` 预设剔除、`all` 保留。

| 工具 | 包 | availability | 作用 | 关键参数 / 上限 / 权限影响 |
| --- | --- | --- | --- | --- |
| `subagent` | `tools/team` | static | 异步启动一个子代理并立即返回 target id | `description`、`prompt`；可选 `name`、`model`、`max_iterations`。完成通知经 `rig.NoticeSource` 注入下一步；需要 `AllOptions.Runner`，`nil` 时调用报配置错误 |
| `subagent_fork` | `tools/team` | static | 同上，但子代理继承父会话已完成历史的副本 | `description`、`prompt`（+同上可选参数）；runner 需配置父会话 |
| `spawn_teammate` | `tools/team` | static | 创建具名队友，后台开始工作 | `name`、`prompt`（`task` 是旧别名）；可选 `description`、`model`。默认最多 8 个活跃成员；只有活着的 Lead 应创建队友 |
| `list_agents` | `tools/team` | static | 列出全部团队成员的 target、名称、可用状态与最近结果状态 | 无参数 |
| `wait_agent` | `tools/team` | static | 等待下一次团队变化并报告哪个 agent 变了 | `timeout_ms`（默认 30s、上限 10min）；没有成员在运行时立即返回 `noProgress` |
| `team_spawn` | `tools/team` | static | 启动一名成员，首次运行在后台继续 | `name`、`prompt`；可选 `model`、`system_prompt`；立即返回成员 id |
| `team_list` | `tools/team` | static | 列出存活成员 | 无参数；id、名称、状态、uptime；被中断的成员保留到 `team_close` |
| `team_send` | `tools/team` | static | 给一个成员发后续消息并等它本轮回复 | `id` 或 `name`、`message`；同一成员上的运行串行化 |
| `team_wait` | `tools/team` | static | 等待指定成员的当前/首次运行并返回结果 | `id` 或 `name`、`timeout_ms`（默认 30s、单次上限 10min）；运行已结束时重复调用返回同一结果 |
| `team_last` | `tools/team` | static | 返回成员最近一次已完成的结果（不等待） | `id` 或 `name` |
| `team_close` | `tools/team` | static | 停止并移除成员 | `id` 或 `name`；名字随之释放给 `team_spawn`；关闭已移除成员是幂等的 |
| `team_interrupt` | `tools/team` | static | 停止成员当前运行但保留成员记录 | `id` 或 `name`；成员状态变 `interrupted`，仍占活跃名额直到 `team_close` |
| `spawn_team` | `tools/team` | alias | 旧名版 `team_spawn` | `name`、`prompt`（`task` 别名）；返回成员 id |
| `list_teams` | `tools/team` | alias | 旧名版 `team_list` | 无参数 |
| `team_status` | `tools/team` | alias | 旧名版成员状态视图（含最近一次运行输出） | 无参数列全部，或给 target 看单个 |
| `close_team` | `tools/team` | alias | 旧名版 `team_close` | target（id 或唯一名称） |
| `wait_team` | `tools/team` | alias | 旧名版 `team_wait` | target、`timeout_ms` |
| `send_message` | `tools/team` | alias | 旧名版 `team_send`（DSH 兼容名） | `target`（id 或唯一名称）、`message`；接受后立即返回，回复用 `team_last`/`team_wait` 读 |
| `interrupt_agent` | `tools/team` | alias | 旧名版 `team_interrupt` | target |
| `list_subagent_models` | `tools/team` | alias | 列出宿主允许子代理使用的模型名 | 无参数；空列表表示子代理继承父模型 |
| `team_task_create` | `tools/team` | static | 在共享任务板上创建一个任务 | `subject`；可选 `description`、`owner`、`blocked_by`、`write_scopes`。依赖必须引用已存在的任务 id；默认状态 `pending`（被阻塞时除外） |
| `team_task_list` | `tools/team` | static | 按创建时间列出共享任务 | 可选 `status`、`owner`；列 ID/STATUS/OWNER/SUBJECT/BLOCKED BY |
| `team_task_get` | `tools/team` | static | 查看一个任务的完整详情 | `id`；含 subject、状态、owner、依赖、write scopes、时间戳与完整描述 |
| `team_task_update` | `tools/team` | static | 通过 action 变更一个任务 | `id`、`action`（claim/complete/reopen/release/edit/set_dependencies/reassign）；按 action 附带 `owner`、`subject`、`description`、`write_scopes`、`add_blocked_by`/`remove_blocked_by`、`status` |
| `workflow` | `tools/workflow` | static | 按步骤运行多子代理工作流并返回每步输出 | `steps` 或 `tasks`（每步 `name`、`prompt`、`parallel`）、`max_parallel`（默认 4、上限 8）；`{{prev}}`/`{{steps.<name>}}` 模板；并行组看不到同组兄弟的输出；无 JS 引擎 |
| `ralph` | `tools/ralph` | static | 反复迭代同一个子代理直到它报告完成 | `prompt`；`completion_prompt`（可引用 `{{output}}`/`{{round}}`）、`max_rounds`（默认 8、上限 32）、`name`；出现独立 `DONE` 或"完成"标记即停止 |
| `code_review` | `tools/review` | static | 用宿主 `rig.Reviewer` 评审代码/diff/计划 | `target`、`kind`（code/diff/plan）、`prompt`、`content`；返回 verdict、摘要与 findings（位置、严重级别、消息）；`nil` reviewer 报配置错误 |
| `auto_review` | `tools/review` | static | 评审并给出"是否应阻塞进度"的结论 | `target`、`prompt`、`severity_threshold`（info/warning/error，默认 warning）；verdict 为 fail/block 或存在达到阈值的发现时标记为阻塞 |

### 4.8 skill / 工具发现（2）

| 工具 | 包 | availability | 作用 | 关键参数 / 上限 / 权限影响 |
| --- | --- | --- | --- | --- |
| `skill` | `tools/skill` | static | 列出、搜索、加载技能文档（Markdown 的 `SKILL.md`） | `action`（list/search/load）、`query`、`name`；只给 `name` 视为 load，什么都不给视为 list；输出上限 32 KiB；只读，不写文件、不联网、不跑 shell；默认搜索 `$ZEOKEEL_SKILLS`、`<root>/.zeokeel/skills`、`~/.zeokeel/skills` |
| `tool_search` | `tools/discovery` | static | 发现并激活"已注册但未发送"的工具 | `query`、`activate`；清单默认最多 40 条；`kit` 用带回调的实例替换 `full.All` 里的未接线版本，关闭 `ToolDiscovery` 时干脆不注册 |

### 4.9 沙箱（2）

| 工具 | 包 | availability | 作用 | 关键参数 / 上限 / 权限影响 |
| --- | --- | --- | --- | --- |
| `sandbox_status` | `tools/sandboxstatus` | static | 只读地报告当前策略：模式、可写面、根内只读子路径、网络、已授权路径与剩余预算 | 无参数；返回稳定字段的 JSON（`mode`、`workspaceRoots`、`writableRoots`、`configuredWritableRoots`、`tempWritable`、`pathProtection`（`enabled` 恒 `false` + 固定 `reason`）、`readOnlySubpaths`、`network`、`grants`、`grantBudget`/`grantBudgetLeft`、`canRequestWritablePath`、可选 `runner`、`guidance`）；`toolchainCaches`/`maskedSockets`/`networkCoverage` 等历史字段保留但恒为空；无副作用；只在 `SandboxGrants` 下构造（`kit` 默认开） |
| `request_sandbox_access` | `tools/sandboxgrant` | static | 为一个具体目录申请会话级、路径作用域的可写授权 | `path`（绝对目录）、`reason`（8–500 字符）。校验 fail-closed：拒绝 `..`、符号链接逃逸、系统目录、`$HOME` 本身、工作区根的祖先；预算默认 5（`sandbox.DefaultGrantBudget`）。**工具自身不审批**，由 `rig.Gate` 在调用前决定：`ask` 档问你、`auto` 档放行、无审批通道则拒绝 |

### 4.10 工作区状态与待办（3）

| 工具 | 包 | availability | 作用 | 关键参数 / 上限 / 权限影响 |
| --- | --- | --- | --- | --- |
| `working_directory` | `tools/misc` | static | 查询或切换会话当前目录 | `path` 可选；相对路径按当前目录解析，绝对路径必须在根内；不允许 `..`/符号链接逃逸。注意：`full.All`/`kit` 只构造这个工具本身，文件工具默认**不**跟随它；需要跟随相对路径时由宿主用 `builtin`/`editor`/`image` 的 `WithWorkingDirectory` 显式接线 |
| `todo_write` | `tools/misc` | static | 用新列表整体替换会话待办清单并渲染为 `[ ]/[~]/[x]` | `todos`（每项 `id`、`content`，可选 `status`、`priority`）；最多 200 项；空数组清空；状态是进程内、重启即丢 |
| `todo` | `tools/misc` | alias | `todo_write` 的旧名，同一 store 共享同一份列表 | `todos`；`standard` 预设剔除 |

## 5. 非静态工具

这些名字不属于 83 个静态工具，不能按静态目录对待。

### 5.1 七个 server-bound 名字

账本 `catalog.ServerBound` 恰好 7 个：宿主不注入对应 client，它们就不存在。

| 工具 | 包 | 构造条件 |
| --- | --- | --- |
| `spill_read` | `tools/builtin` | 宿主设置 `rig.SpillPolicy.Store`（`full.All` 读 `AllOptions.SpillStore`）。把超限结果存起来后，模型看到的 `ref:` 句柄靠它读回；单次 ≤8 KiB；可发现但不进初始 active 集 |
| `mcp_list_resources` | `tools/mcp` | `AllOptions.MCPClients` 中至少一个 client |
| `mcp_read_resource` | `tools/mcp` | 同上；`uri` 必填，二进制内容只报告类型与大小 |
| `mcp_list_resource_templates` | `tools/mcp` | 同上 |
| `mcp_list_prompts` | `tools/mcp` | 同上 |
| `mcp_get_prompt` | `tools/mcp` | 同上；`name`、可选 `arguments` |
| `lsp` | `tools/lsp` | `AllOptions.LSP != nil`；旧版单一合并工具（`symbols`/`path`/`line`/`character` 动作路由） |

另外，六个分项 LSP 工具 `lsp_hover`、`lsp_definition`、`lsp_references`、`lsp_implementation`、`lsp_diagnostics`、`lsp_symbols` 也只在配置 LSP client 时构造；它们**不在**上面 7 个账本名字里（账本只登记旧版合并工具 `lsp`），位置参数是 1 基。

### 5.2 动态 MCP 工具

`mcp__<server>__<tool>`：每个 MCP server 的工具由它自己的 `tools/list` 在运行期生成，前缀默认 `mcp__`（`AllOptions.MCPPrefix` 可改）。名字不是静态的，账本用 `catalog.Dynamic` 记录模式。清洗后与已分配名冲突时追加确定性后缀 `__2`、`__3`…（只取决于 `tools/list` 顺序）；要诊断/拒绝这种配置用 `mcp.NewToolsChecked`，多 server 聚合用 `mcp.AggregateTools`。

### 5.3 opt-in：`run_code`（PTC）

`run_code` 只在 `kit.Config.PTC=true` 时出现（`tools/ptc`）。模型写一段 Python 3 或 Node.js 程序，在**系统已安装**的运行时里执行；程序通过 `tools.<name>(...)` 调用 agent 已有工具，内层调用走同一个 `rig.Gate`，解释器进程在接了 `SandboxRunner` 时同样经 `Runner.Confine` 包装。参数：`code`、`language`（python/node）、`timeout_ms`（默认 60s、上限 10min）；输出上限 64 KiB。它不在 83 个静态工具里，但计入 `docs/ai/tools.json` 的 84 条。

### 5.4 内部原语与子代理能力

- `tools/shelf` 是**渐进式加载的库原语**（list/search/load），供宿主自定义工具名后使用；它**不是**静态工具，不在账本与 `tools.json` 里。
- `structured_output` 不是独立工具：它只注入到设置了 `SubagentSpec.OutputSchema` 的子代理上下文，参数按该 schema 校验。

## 6. 权限与平台影响

权限模型分两层：**沙箱策略**（进程内围栏，决定"能碰什么"）与**审批门禁**（决定"要不要问人"）。二者独立，可以任意组合；模式与审批档位的完整字段见 [CONFIG.md](CONFIG.md)。

### 6.1 三个权限模式对各工具的影响

| 模式 | 文件工具 | shell / jobs / terminal | worktree | 其他 |
| --- | --- | --- | --- | --- |
| `read-only` | `write_file`/`edit_file`/`str_replace_editor` 写命令与 `apply_patch` 拒绝；`glob`/`grep`/`list_dir` 隐藏被拒路径；`read_file`/`read_image` 仍可读 | `bash`/`pwsh` 禁用；`job_start` 拒绝；`terminal_open` 拒绝 | `enter_worktree`/`exit_worktree` 拒绝；`list_worktrees` 只读可用 | `sandbox_status` 的 `writableRoots` 为空 |
| `workspace-write`（默认） | 只允许在策略根内写（∪ 追加可写根 ∪ 私有 `/tmp`）；`..`/符号链接逃逸被拒 | 命令 cwd 必须在根内，命令受允许/拒绝名单约束（`kit` 默认不限命令名，靠人工审批） | enter/exit 允许，git 调用经策略检查 | 可写面 = `Roots() ∪ AdditionalWritableRoots() ∪ 私有 /tmp`；`read-only` 只留 `/dev/null` |
| `danger-full-access` | 去掉根围栏（不包装，`Enforcement=none`）；显式拒绝列表仍生效 | 去掉根与允许列表围栏 | 去掉根围栏；显式拒绝列表仍生效 | 没有根内只读子路径的强制（整根可写） |

`full.All` 在 `Policy` 为 nil 时用 `sandbox.New(sandbox.WithRoots(opts.Root))`：`Root` 为空就是"拒绝一切路径"的 fail-closed 策略。

### 6.2 审批门禁

`kit.Config.Approval` 默认 `ask`，**只有两层**（DSH 对齐）：

1. 只读工具**免问**；
2. 其余（写文件、`bash`/`pwsh`、后台任务，**包括 `rm -rf /` 这类危险命令**）问宿主
   注入的 `Config.Asker`；**默认不设超时**（一直等，只有取消本轮才撤回），要上界设
   `Config.AskTimeout`。

`auto` = `gate.PersonalAuto()` = **`AllowAll()`（全放行、不问人）**；`never` =
`gate.PersonalNever()`（全部拒绝）。没接 `Asker` 时用 `gate.PersonalAskWithoutAsker()`：
只读放行、其余拒绝并说明"没有可用的审批通道"。

**默认不含** `CommandRiskGate`：命令风险分类不再参与默认门禁。要用就显式叠在
`AllowThenAsk` **之外**（最外层），否则每条 bash 命令都会先弹窗。

四个产品级档位见 `kit.Presets()`：`read-only` / `workspace-write`（默认）/
`danger-full-access` / `auto`（全权 + `ToolReviewer` 预审）。

### 6.3 沙箱内命令免审批

`kit.Config.AutoAllowSandboxedCommands`（`nil` = 开）让**已沙箱化的 shell 命令**不再弹窗。安全性由围栏保证，而不是由点击次数保证。判据必须全部满足：

- 工具是 shell 类（`bash`/`pwsh`）；
- `Config.SandboxRunner` 非 nil 且自报能强制；
- 策略不是 `danger-full-access`。

**不看命令文本**（DSH 对齐后取消了风险分类这一层判据）。免问仍写一条
`approval.response`（`decision=allow`、`reason=sandboxed`）供 UI 与审计复查；文件工具
（`write_file`/`edit_file`/`apply_patch`…）不经 OS runner，照旧问人。

### 6.4 逐命令提权：`require_escalated`

`bash` 与 `pwsh` 接受 `sandbox_permissions: "require_escalated"` + `justification`（≥10 字符）。适用条件与边界：

- 宿主必须注入 `builtin.Escalator`（`full.AllOptions.Escalator`；`kit` 用 `Asker` + 审批档位自动接线）。没有提权通道时该参数返回"composition 不支持"的兼容错误，行为与 v0.20 一致。
- 必须**人工批准**：`approval=auto` 与 `never` 永不批准；没有 `Asker` 时没有通道，模型会被告知。
- 提权只豁免"这一条命令是否被 OS runner 包装"：策略（可写根、命令允许/拒绝）、workdir、超时、输出上限**一律不变**。
- 适用于"不是路径"的资源：Windows 注册表、DPAPI/keychain/libsecret 凭据、系统 socket——这些地方可写根授权帮不上忙。
- 每次请求（无论批准与否）都会发出审计事件。

注意：这里说的是 `tools/builtin` 的逐命令提权，**不是**已整包删除的 `tools/escalate` / `sandbox/escalate`；后者不要重新引入。

### 6.5 平台

- `bash` 需要 unix 的 `/bin/sh`；Windows 上执行会返回 "/bin/sh is not available on Windows; use the pwsh tool"。
- `job_*` 用**平台 shell**：Unix 是 `/bin/sh -c`，Windows 依次尝试 `pwsh`、Windows PowerShell、`cmd.exe`，因此后台任务在 Windows 也可用。
- `terminal_*` 在 Unix 默认 `/bin/sh` + 真实 PTY；Windows 默认 `powershell.exe` + ConPTY，ConPTY 不可用时回退 pipe（工具仍可用）。
- `pwsh` 在 Windows 上是默认 shell 工具；非 Windows 上 `kit` 会摘掉它（需要系统装有 PowerShell 才会成功），`full.All` 则总会构造。
- `worktree` 工具需要系统 `git` 可执行文件；不是 git 仓库时返回可读的模型错误（`IsError=true`），不是 Go 错误。
- 其余工具与平台无关；`sandbox/osrunner` 的平台差异见 [SANDBOX-OS.md](SANDBOX-OS.md)。

## 7. 新增工具

新增工具不是自动注册的。完整清单（实现包 → `tools/full` 接线 → `catalog.Static` → 预设表 → 测试 → `go run ./cmd/rigdoc` → 两份文档 → `CHANGELOG`）见 [COVERAGE.md](COVERAGE.md#新增工具清单)；本文不重复。

## 相关文档

- [COVERAGE.md](COVERAGE.md) — 账本结构、覆盖门禁、新增工具清单、常见坑
- [docs/ai/tools.json](ai/tools.json) / [docs/ai/tools.md](ai/tools.md) — 机器可读目录与 Schema
- [CONFIG.md](CONFIG.md) — `kit.Config` 全字段与默认值（工具面、权限、发现开关）
- [SANDBOX-OS.md](SANDBOX-OS.md) — opt-in OS runner、沙箱诊断与运行期授权
- [AI-SDK.md](AI-SDK.md) — 给 AI 的 SDK 使用与组装指南