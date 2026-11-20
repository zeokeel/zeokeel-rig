// Package sandboxstatus 提供只读的 sandbox_status 工具：把宿主注入的
// *sandbox.Policy（以及可选的 runner 信息）汇报成一份模型可读的 JSON 快照。
//
// # 它解决什么问题
//
// 策略层拒绝的文案只能说明「这个路径被拒绝了」，模型却不知道当前可写面到底有
// 多大：工作区根有哪些、额外放开了哪些可写目录、可写根内还有没有声明为只读的
// 子路径、临时目录是否可写、还能不能再申请一次授权。缺少这份快照时，模型往往反复
// 重试同一条命令，或直接把状态写到另一个同样被拒绝的固定路径。
//
// sandbox_status 只读、无副作用：它不修改策略、不触发授权、不触碰文件系统之外
// 的状态（唯一的外部读取是 os.TempDir()，用于渲染已启用的临时目录）。
//
// # 模型可见工具
//
//	sandbox_status {}
//
// 返回一段 JSON（rig.ToolResult.Content），字段是稳定的：
//
//   - mode：生效的权限模式（workspace-write / read-only / danger-full-access）。
//   - workspaceRoots：策略配置的根（第一个是主根，相对路径的解析基准）。
//   - writableRoots：实际可写面（工作区根 + 追加可写根 + 已启用的平台临时目录
//   - 私有 scratch），只读模式下为空数组（唯一的例外是配了私有 scratch，此时
//     只列 scratch）。
//   - configuredWritableRoots：仅静态配置的追加可写根（不含运行时授权）。
//   - scratch：策略的私有临时目录（WithScratchDir），未设置时为空串；
//     scratchError 只在"目录没能建出来"时出现（说明命令为何会在运行期失败）。
//   - tempWritable：writable 表示此刻**真的**有可写的临时目录（read-only 只有配了
//     scratch 才为 true），writablePath 是那个目录（有 scratch 时是 scratch，不是
//     /tmp），slashTmp / tmpdirEnv 是 workspace-write 下两项平台临时目录开关。
//   - pathProtection：保护路径 / 拒绝 glob 这一层强制**是否启用**。DSH 对齐后
//     恒为 {enabled:false, reason:"disabled: …"}——本 SDK 只做可写根围栏。
//     这里刻意不放一个恒空的数组：空数组会被读成「本次恰好没配保护列表」。
//   - readOnlySubpaths：可写根内部仍声明为只读的子路径（绝对路径）。保护路径
//     停用后这是唯一仍然生效的「根内只读」约束（Policy.AllowsWrite 会拒绝落在
//     其中的写入）；空数组表示根内没有这样的位置。
//   - network：是否允许对外网络访问。
//   - grants：本会话已批准的运行期授权（path + reason），按批准顺序。
//   - grantBudget / grantBudgetLeft：授权次数上限与剩余次数。
//   - canRequestWritablePath：当前是否还能申请新的可写目录（workspace-write 且
//     预算未用尽）。
//   - runner（可选）：挂接了 OS runner 时报告 runner 名称与 enforcement；未提供
//     WithRunnerInfo 时整个字段省略。
//   - guidance：通用处置顺序（读拒绝消息里的路径 → 优先用工具自己的重定向
//     config-dir / XDG_* / HOME → 都不行才申请具体目录 → 不要重复同一条命令）。
//
// 文档与文案刻意保持通用：不出现任何具体产品、厂商或工具目录名，因为可写面的
// 知识来自策略本身，而不是某个工具的约定。
//
// # 组装
//
// 工具不自动注册；宿主把它与其它工具一起传入注册表。policy 为 nil 时工具仍然
// 可安全调用：每次调用返回 IsError=true 的可读错误结果，而不是 panic（这与
// “不知道要强制什么就不能假装知道”的 fail-closed 约定一致）。
//
//	policy := sandbox.New(sandbox.WithRoots(workspaceRoot))
//	tools := []rig.Tool{sandboxstatus.New(policy)}
//
// 需要报告 runner 时用 WithRunnerInfo；provider 在每次调用时求值，因此 runner
// 选择（功能探测）晚于工具构造也没问题：
//
//	status := sandboxstatus.New(policy, sandboxstatus.WithRunnerInfo(func() (string, sandbox.Enforcement) {
//		return runner.Name(), runner.Enforcement()
//	}))
//
// # 权限与并发
//
// 工具只读：它可以在只读模式下正常工作，也不需要审批。Provider 与 Policy 的并发
// 安全性由调用方保证（sandbox.Policy 在 New 之后并发安全，运行时授权是内部加锁
// 的可变状态）。
package sandboxstatus
