package kit

import "github.com/zeokeel/zeokeel-rig"

// 本文件只给外部测试（package kit_test）用：它把"本次 Run 实际使用的门禁"暴露
// 成测试可见的入口，好在同一份装配上验证并发行为（两个 goroutine 同时走进审批）。
// 编译进测试二进制，不进对外 API。

// GatewayForRun 返回一次 Run 实际使用的门禁（生产路径是 kit.Agent.Run 里同一次
// 调用）。测试用它把并发调用直接压到门禁上：agent 自己是在一轮里串行审批的，
// 走 Run 无法覆盖"两个调用者同时到达"。
func GatewayForRun(a *Agent, req RunRequest) rig.Gate {
	return a.gatewayForRun(req)
}
