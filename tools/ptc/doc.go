// Package ptc 提供面向模型的 run_code 工具：模型编写的程序在系统已安装的
// 运行时（Python 3 或 Node.js）中执行，并通过 stdin/stdout 上的行协议调用
// agent 已有的工具。
//
// 运行时是普通的子进程；Rig 不内嵌 JavaScript 引擎。生成的 SDK 前导代码暴露
// `tools.<name>(...)`：Python 调用 tools.web_search(query="...")，Node 调用
// await tools.web_search({query:"..."})。内层调用经过配置的 rig.Gate，因此
// 在设置了 gate 时，启用 run_code 不会绕过审批。
//
// # 参数与行为
//
// 模型可见参数：
//
//   - code（必填）：程序源码。
//   - language（可选）：python 或 node；默认先尝试 python3，再回退 node。
//   - timeout_ms（可选）：程序超时，默认 DefaultTimeout（60s），上限
//     MaxTimeout（10min）。
//
// 程序源码会连同 SDK 前导代码写入临时目录后再执行，结束后清理。stdout 中
// 形如协议前缀的行被当作工具调用请求，其余输出汇总后返回；stderr 也会附在
// 结果中。合并后的输出与 stderr 以 MaxOutputBytes（64 KiB）为上限，超时以
// 工具错误返回。
//
// # 构造与接线
//
//	tool := ptc.New(ptc.Config{
//		Tools: registry.List(),   // 程序可调用的工具子集
//		Gate:  gate,              // 内层调用沿用同一审批 gate
//		Root:  "/srv/workspace",  // 程序初始工作目录
//	})
//
// NewTool 是 New 的具名等价形式。协议与工具名由 The tool 自己解析，宿主只需
// 提供工具列表与 gate。本工具只在宿主显式组装时出现（kit.Config.PTC=true
// 的路径），默认工具集不包含它。
//
// # 权限模式与信任边界
//
// run_code 本身不是 shell：它不解析 shell 语法，也不受 shell 命令允许列表
// 约束。程序作为宿主进程的子进程运行，继承宿主权限；真正约束内层行为的是
// Gate 与每个被调用工具各自的 sandbox.Policy/权限模式。因此启用 run_code 是
// 信任决策：只应在受控部署中开启，并把 Config.Tools 收窄到必要子集。
package ptc
