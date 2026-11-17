//go:build race

package events_test

// auditRaceEnabled 报告当前是否为 -race 构建：竞态检测让编码/解码慢数倍，吞吐门槛
// 只在非 race 构建下断言（正确性断言两种构建都跑）。
const auditRaceEnabled = true
