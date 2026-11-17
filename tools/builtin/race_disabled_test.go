//go:build !race

package builtin

// raceEnabled 报告当前是否为 -race 构建（见 race_enabled_test.go）。
const raceEnabled = false
