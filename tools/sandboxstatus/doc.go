// Package sandboxstatus 提供只读的 sandbox_status 工具：把宿主注入的
// *sandbox.Policy（以及可选的 runner 信息）汇报成一份模型可读的 JSON 快照。
//
// # 它解决什么问题
//
// 策略层拒绝的文案只能说明「这个路径被拒绝了」，模型却不知道当前可写面到底有
// 多大：工作区根有哪些、额外放开了哪些可写目录、哪些路径即使在可写根内也保持
// 只读、临时目录是否可写、还能不能再申请一次授权。缺少这份快照时，模型往往反复
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
//   - writableRoots：实际可写面（工作区根 + 追加可写根 + 已启用的平台临时目录），
//     只读模式下为空数组。
//   - configuredWritableRoots：仅静态配置的追加可写根（不含运行时授权）。
//   - tempWritable：两个开关，slashTmp 为字面 /tmp 是否可写、tmpdirEnv 为
//     $TMPDIR（os.TempDir()）是否可写。
//   - protectedPaths：即使在可写根之内也必须保持只读的路径（例如仓库的 .git、
//     宿主自己的状态目录）。
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
