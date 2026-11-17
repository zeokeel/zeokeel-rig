//go:build race

package secrets

// raceEnabled 报告当前是否为 -race 构建。竞态检测会让正则匹配慢数倍，性能门槛
// 只在非 race 构建下断言（正确性断言两种构建都跑）。
const raceEnabled = true
