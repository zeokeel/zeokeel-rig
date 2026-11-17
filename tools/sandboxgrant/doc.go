// Package sandboxgrant 提供 request_sandbox_access 工具：让被沙箱约束的模型为
// **一个具体目录**申请会话级可写授权。
//
// 沙箱策略的可写面本来只有"工作区根 + 平台临时目录"这样的小集合；但真实工具经常
// 把状态写在固定路径（凭据、缓存、套接字）。与其为每个工具硬编码目录，不如让模型
// 在遇到写拒绝时提出申请，由宿主审批后把该目录加入可写集。本工具是这次申请的
// 模型侧入口：它**不**做审批，只在校验通过且审批已完成之后调用
// sandbox.Policy.AddWritableRoot 登记授权。
//
// # 模型可见工具
//
//   - request_sandbox_access {path 必填, reason 必填}：path 是要放行的绝对目录，
//     reason 是 8..500 字符的理由（展示给审批人并写审计）。成功返回
//     {"granted":true,"path":"…","budgetLeft":N,"note":"…"}；路径本来就可写时返回
//     {"granted":true,"path":"…","alreadyWritable":true,"budgetLeft":N,"note":"…"}，
//     让模型不要重复申请；被拒绝时以 IsError 结果透传"哪条规则拒绝了它、以及该
//     怎么办"。
//
// 工具刻意没有 scope/mode 之类参数：授权是**会话级、路径作用域**的，批准一次后
// 所有工具都能写该目录。想要"整体放开"应由用户切换权限模式，而不是由模型逐次
// 提权——这是设计边界。
//
// # 组装与权限
//
//	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
//	tool := sandboxgrant.New(policy, sandboxgrant.WithObserver(func(call rig.ToolCall, grant sandbox.Grant, err error) {
//		log.Printf("grant request path=%q reason=%q err=%v", grant.Path, grant.Reason, err)
//	}))
//
// 审批由 rig.Gate 在 Invoke 之前完成，因此宿主必须显式把 request_sandbox_access
// 加入可批准集合（Ask/Review 或自定义 Gate）；kit 的默认只读门禁**不**放行它。
// 授权次数默认上限为 sandbox.DefaultGrantBudget（5），可用
// sandbox.WithGrantBudget 调整。
package sandboxgrant
