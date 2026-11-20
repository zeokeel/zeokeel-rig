module github.com/zeokeel/zeokeel-rig/session/sqlite

go 1.27

require (
	github.com/zeokeel/zeokeel-rig v0.26.1
	modernc.org/sqlite v1.62.0
)

require (
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.1.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.49.0 // indirect
	modernc.org/libc v1.80.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)

// replace 仅供**本仓开发**使用：本地与 CI 直接用工作树里的根 module，不必先发 tag。
// 依赖方会忽略它——Go 只应用主 module 的 replace，子模块被 go get 时这条声明是死的。
// 所以它掩盖不了"require 指向的版本不存在"这类问题，那由两条 CI 门禁盯着：
//   - `sqlite-consumer smoke`：临时 module 里 go get 本模块（无 go.work、无 replace）；
//   - `submodule release tags`：require 必须等于最近根 tag，且 session/sqlite/vX.Y.Z tag 存在。
// require 现在是 v0.24.0（= 下一个要发布的版本：本模块 import 的根 module `memory`
// 包在 v0.23.4 里还不存在，实测 v0.23.4 下 `go build` 报
// `module ... does not contain package github.com/zeokeel/zeokeel-rig/memory`）；
// 根 tag 打出后这条 require 才是"已发布"状态。
replace github.com/zeokeel/zeokeel-rig => ../..
