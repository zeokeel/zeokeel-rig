// Package instructions 发现并渲染发给模型的指令文档（AGENTS.md）与环境上下文。
//
// 它解决两个"每次都要人工喂"的问题：
//
//   - 仓库里已经写了 AGENTS.md，但模型看不到：宿主不得不在每次组装 prompt 时
//     手工读文件、手工拼接、手工处理多层目录。
//   - 模型不知道自己在哪里跑：cwd 是哪个目录、工作区根是什么、今天几号、当前在
//     哪个 git 分支上。
//
// 本包只做两件纯函数式的事（读文件 + 拼字符串），不签发请求、不碰 agent 循环，
// 因此可以被任何宿主复用，也便于测试。它只依赖标准库，不执行外部命令（git 分支
// 只读 .git/HEAD），不写任何文件。
//
// # 指令层级（Load）
//
// Load 从 cwd 向上发现 AGENTS.md，直到某个目录含 .git（把它当作仓库根，含该级）
// 或到达文件系统根。发现走的是 cwd 的**字面路径**：cwd 本身是符号链接时不会走到
// 链接目标那一侧的祖先，真实仓库根的 AGENTS.md 会因此漏掉（见 resolveStartDir 的
// TODO）。需要这种语义的宿主应传真实路径。
//
//	cwd/AGENTS.md              → local
//	cwd/../AGENTS.md           → ancestor
//	…                          → ancestor（祖先从远到近）
//	<repo root>/AGENTS.md      → root
//	~/.config/rig/AGENTS.md    → user
//
// Set.Documents 按优先级从低到高排列（user 最先、local 最后）；Render 用
// `## <相对路径> (source: user|root|ancestor|local)` 分段拼接，段间一个空行，
// 因此同一份 Set 的渲染结果逐字节稳定、可直接拼进 system prompt。
//
// 只认 AGENTS.md 这一个文件名——不发明 AGENTS.override.md、CLAUDE.md 之类的发现
// 规则；需要更多文档时由宿主用 WithExtraPaths 显式追加（它们按用户级文档对待）。
//
// # @import 展开
//
// 文档里独占一行的 `@path`（或 `@"./x y.md"`）会在原位展开为被引入文件的内容。
// 只有"像路径"的目标才算 import：显式相对/绝对路径（`./`、`../`、`/`、`~`）或以
// 常见文档扩展名（.md/.markdown/.mdown/.txt/.rst/.adoc）结尾。正文里一行一个包名
// （`@types/node`）不会被当成 import 吃掉——那是 AGENTS.md 里很常见的写法，误判
// 会把真实内容替换成注释。目标存在（哪怕是个目录）时仍按 import 意图处理。
//
// 相对路径相对**当前文档所在目录**解析（不是 cwd）。仓库内文档的 import 目标默认
// 被限制在"发现到的仓库根"之内（没有 .git 时是 cwd）：目标经 `EvalSymlinks` 后仍在
// 根内才读取，`../`、根外绝对路径、以及指向根外的符号链接都会在原位留下
// `<!-- import not found: … -->`，既不读取也不暗示根外是否存在。宿主用 WithImportRoot
// 可以换根（更窄的子树也行）或传空串关闭限制；由宿主显式传入的 user/extra 文档
// 不受限——它们不是仓库内容。仓库内文档**本身**是符号链接时按文件系统语义读取
// 其目标（是否允许由宿主的文件系统沙箱决定）。
//
// 展开是递归的，受 WithMaxImportDepth（默认 4）限制；循环引用被拦截。import 失败从不报错：失败处
// 留下一行 HTML 注释（`<!-- import not found: x -->`）并记入 Document.Imports，
// 让模型与诊断工具都能看到"这里本该有内容但没有"。
//
// 围栏代码块（``` 与 ~~~，可带 info string、最多缩进 3 列；4 列及以上是缩进代码块，
// 不是围栏）内部不做展开：
// AGENTS.md 常有"如何写 import"的示例代码块，展开示例会凭空多出内容。每个文件的
// 围栏状态独立维护，未闭合的围栏保护到该文件末尾。
//
// # 截断
//
// 单个文档展开后超过 WithMaxBytes（默认 64 KiB）时保留头部、按 rune 边界截断，
// 并在末尾追加 `\n…[truncated: N bytes omitted]`。截断作用于展开后的内容，
// 因此 import 进来的大文件同样计入上限。单文档上限之外还有**总量**上限
// WithTotalBytes（默认 DefaultTotalBytes = 256 KiB）：超预算时按优先级从高到低
// （local → 最近的 ancestor → … → root → user）累计，先保住更具体的文档；被整份
// 跳过的文档在原位留一行 `<!-- document skipped: total budget exceeded -->`，
// Render 末尾再附 `[instructions truncated: N document(s) skipped]`。
//
// 读入的字节先过一遍净化：NUL 与非法 UTF-8 序列替换为 U+FFFD，保证进入 prompt 的
// 文本总是合法 UTF-8（二进制文件不会把控制字节带进去）。非普通文件（目录、FIFO、
// socket、设备）不会被读取：顶层文档报错，import 目标按"找不到"处理——FIFO 上的
// ReadFile 会永久阻塞，必须挡在读取之前。
//
// # 环境上下文（DetectEnvironment）
//
// DetectEnvironment 汇报 workspace/cwd/platform/date/git/temp。它只读 .git/HEAD
// 解析分支（不 exec git）：普通 ref 得到分支名，detached HEAD 得到
// `detached@<sha8>`，不在仓库里则为空（Render 渲染成 `git: not-a-repo`）。HEAD 只
// 取第一行——.git/HEAD 是仓库自带内容，多行内容若原样进入分支名就能在环境块里
// 伪造出额外的行。Render 对进入环境块的每个值做单行化（换行/回车/制表符→空格，
// 其它控制字符→'?'）并限长 512 rune，因此字段值无法伪造新的行或提前闭合
// `</environment>`。
// 脏工作区检测刻意不实现：那需要遍历整个工作区，代价远高于本块的收益。因此
// Environment.GitDirty 是三态 GitDirtyState，零值 GitDirtyUnknown 渲染成
// `dirty=unknown`——**绝不能默认声称工作区干净**（模型会据此跳过 git status）；
// 只有真的检测过的宿主才该设置 GitDirtyNo / GitDirtyYes。
//
// # 典型用法
//
//	set, err := instructions.Load(cwd,
//		instructions.WithUserPath(userPath),
//		instructions.WithImportRoot(repoRoot), // 可选；默认已等于发现到的仓库根
//		instructions.WithTotalBytes(256<<10), // 可选；默认即 256 KiB
//	)
//	if err != nil {
//		// 读不到 AGENTS.md 属于可恢复配置问题；调用方可选择记日志并继续。
//	}
//	env := instructions.DetectEnvironment(workspaceRoot, cwd, time.Now())
//	prompt += set.Render(workspaceRoot) + "\n\n" + env.Render()
//
// 两段文本都以内容开头、以内容结尾，不含尾随空行，因此宿主用 "\n\n" 拼接即可
// 得到稳定的 prompt 片段。
//
// # 并发与副作用
//
// Load、Render、DetectEnvironment 都是无状态函数：不缓存、不加锁、只读文件系统。
// 同一输入（以及同一时刻的 Time 参数）总产生同一输出，便于快照测试。
package instructions
