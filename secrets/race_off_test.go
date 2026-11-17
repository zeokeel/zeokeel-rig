//go:build !race

package secrets

// raceEnabled 报告当前是否为 -race 构建（见 race_on_test.go）。
const raceEnabled = false
