//go:build race

package builtin

// raceEnabled 报告当前是否为 -race 构建。竞态检测会把热路径拖慢 5~20 倍，
// 因此**性能门槛只在非 race 构建下断言**（正确性断言两种构建都跑）。
const raceEnabled = true
