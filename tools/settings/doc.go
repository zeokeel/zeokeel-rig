// Package settings 提供两个模型可见的键值设置工具：settings_get 与 settings_set。
//
// 工具本身不持有存储：读写都委托给宿主提供的 rig.Settings 接口（键与值都是字符串）。
// 产品可以注入文件、数据库或远端服务实现；未注入（nil）时两个工具在调用时返回可读的
// “the host did not configure a rig.Settings store” 错误，而不是 panic。
//
// # 模型可见工具
//
//   - settings_get {key 必填, default 可选}：读取一个键的值；键不存在时返回 default，
//     未提供 default 时返回空字符串。
//   - settings_set {key 必填, value 必填}：写入一个键的值；传空字符串表示清空该值
//     （键仍保留），成功后返回 set "<key>"。值是否落盘由宿主实现决定。
//
// 任意非空字符串都可以作为键（点分命名与 UI 作用域的键均可）。
//
// # 组装
//
// 通过 settings.Tools(store) 构造并加入工具注册表：
//
//	reg := tools.NewRegistry(settings.Tools(mySettings)...)
//
// # 依赖、权限与并发
//
// 工具只依赖 rig.Settings；不直接触碰文件系统、网络或 shell，真正的持久化与访问控制
// 属于宿主。审批发生在 agent 层：宿主可用 rig.Gate 按工具名决定是否放行（例如允许读取、
// 拒绝写入）。工具的并发安全性取决于宿主实现（rig.Settings 契约要求其安全并发使用）。
package settings
