// Package seccomp 手写 Linux seccomp BPF 过滤器：零依赖、可用 CGO_ENABLED=0 构建。
//
// # 为什么不用库
//
// 主流的两个 Go 封装都要额外依赖：`libseccomp-golang` 需要 cgo（本仓库禁止），
// `elastic/go-seccomp-bpf` 会引入新的模块依赖（本仓库只允许 `mvdan.cc/sh/v3`）。
// seccomp 的 ABI 本身很小：一个 `struct sock_filter{code, jt, jf, k}` 数组 + 一次
// `seccomp(SECCOMP_SET_MODE_FILTER)`，自己写比引依赖更可控，也更容易审计。
//
// # 它解决什么问题
//
// OS 沙箱（bwrap / Landlock / Seatbelt / Windows ACL）治理的是**文件效果**，
// 管不到"经宿主守护进程 unix socket 发出的请求"：向守护进程发一个挂载宿主根目录的
// 请求，会在宿主侧执行，等于完全逃逸（实测）。按路径把该 socket 覆盖成
// /dev/null 只能挡住名单里的几个，`SSH_AUTH_SOCK`、dbus、X11、自定义守护进程全都
// 漏。seccomp 直接拦 `connect(2)`：任何 unix socket 连不上，这类请求就发不出去。
//
// # 取舍
//
// 拦 `connect` 会连**回环**一起断（本地 dev server 也用不了），所以过滤器只在
// `Policy.AllowNetwork == false` 时装；允许网络时不装（保持开发流程可用）。
package seccomp
