// Package image 提供 read_image 工具：读取沙箱内的图片文件，并以多模态工具
// 结果返回。
//
// 该工具不解码像素。它通过魔数识别常见图片格式（PNG、JPEG、GIF、WebP、BMP），
// 在可配置的大小上限内读取文件，并把字节以 base64 data: URL 的形式放入
// rig.ToolResult.Parts。agent 会把这些 Parts 作为后续用户消息追加到模型对话，
// 因为 provider 无法把图片直接附加到工具消息本身。
//
// 每个路径都经绑定的 sandbox.Policy 解析（允许根、拒绝 ".."、符号链接约束、
// 拒绝 glob），读取在只读策略下仍然允许。不支持的格式、超大文件、缺失文件、
// 目录以及根外路径都会作为面向模型的错误返回（IsError=true）。
//
// # 参数与上限
//
// 模型可见参数：
//
//   - path（必填）：沙箱根内的图片路径（也接受根内的绝对路径）。
//   - detail（可选）：发送给模型的图像分辨率提示，取值 auto/low/high，默认
//     DefaultDetail（"auto"）。
//
// 文件大小默认上限为 DefaultMaxBytes（10 MiB），可用 WithMaxBytes 调整；超过
// 上限的文件会被拒绝，不会截断后返回。MIME 辅助函数单独导出，宿主可复用它按
// 魔数识别格式。
//
// # 构造与接线
//
//	policy := sandbox.New(sandbox.WithRoots("/srv/workspace"))
//	imageTool := image.New(policy, image.WithMaxBytes(5<<20))
//
// # 权限模式
//
//   - read-only：允许读取图片（本工具不写文件）。
//   - workspace-write（默认）/ danger-full-access：同样只是读取；区别仅在于
//     同一 agent 中其他写工具是否可用。
package image
