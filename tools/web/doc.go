// Package web 提供面向网络的模型内建工具：web_fetch（抓取 URL 并转为文本）与 web_search
// （委托给可插拔的搜索后端）。
//
// 两个工具仅依赖标准库。web_fetch 策略严格：只允许 HTTP(S)，重定向链、总超时、响应体大小
// 与输出字符数都有上限，因此一次调用既不能挂住一次 run，也不能把模型上下文刷爆。哪些 URL
// 可接受不由此包决定：Policy 由调用方提供。
//
// # 模型可见工具
//
//   - web_fetch {url 必填, format 可选(text|html), maxChars 可选}：抓取绝对 http(s) URL；
//     format=text（默认）把 HTML 转成可读纯文本，format=html 返回原始 HTML；maxChars 默认
//     20000、上限 200000（DefaultMaxChars/MaxCharsLimit）。
//   - web_search {query 或 queries, limit 可选}：委托给宿主注入的 Searcher。query 是单查询
//     旧别名，queries 是 DSH 兼容数组（1-MaxSearchQueries=4 个，重复查询只执行一次）；
//     limit 默认 DefaultSearchLimit=5、上限 MaxSearchLimit=20。未注入 Searcher 时返回
//     配置错误。
//
// 默认值：总超时 30s、响应体上限 2 MiB、重定向链上限 5、DefaultUserAgent。
//
// # Policy 与 SSRF 防护
//
// Policy 是单方法接口（CheckURL(*url.URL) error），任何实现了该方法的类型都可满足，包括
// sandbox 包的策略类型——Rig 不 import 该包，接口是结构化（鸭子类型）。nil Policy 允许
// 所有 URL；运行不可信模型时应始终传入 Policy。若 Policy 还实现 HostResolver，fetcher 会
// 自行解析并校验每个目标地址，钉住已验证 IP 以避免 DNS rebinding；其他非 nil 策略也受
// 内置私网/回环/链路本地/云元数据地址防护，WithAllowPrivateHosts 仅供刻意抓取私网目标的
// 宿主关闭该防护。
//
// # 组装与权限
//
//	fetch := web.NewFetcher(policy)               // 或 web.NewHTTPFetcher(options...)
//	search := web.NewSearcher(mySearcher)         // 或 web.SearchTool(mySearcher)
//
// 工具的权限完全由传入的 Policy 决定；web_search 的行为由 Searcher 实现负责。审批在
// agent 层由 rig.Gate 控制。
package web
