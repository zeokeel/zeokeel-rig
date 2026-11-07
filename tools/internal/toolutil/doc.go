// Package toolutil 汇集 Rig 各工具包共用的内部小工具：参数解码、模型可见结果构造、
// JSON Schema 属性构造器与 UTF-8 安全截断。
//
// 本包刻意声明为 internal：对外公开的工具面位于各 tools 子包与根 rig 包中，这里的函数
// 只是 tools/builtin、tools/editor、tools/web 等包的实现细节，永远不属于 SDK 契约，
// 外部代码不应依赖。
//
// # 主要辅助函数
//
//   - DecodeArgs / DecodeArgsAs：把 ToolCall.Arguments 的 JSON 解码到目标结构；空字符串
//     按空对象处理，格式错误时返回可读错误；As 版本允许附加 "ralph: " 之类的前缀。
//   - TextResult / ErrorResult / ErrorResultf：构造成功或失败（IsError=true）的
//     rig.ToolResult。
//   - ObjectSchema / StringProp / IntegerProp / BooleanProp / StringArrayProp：构造工具
//     参数的 JSON Schema（object、properties、additionalProperties:false、required）。
//   - TruncateBytes：按字节上限截断且不切坏 UTF-8 字符，必要时追加后缀。
//
// 这些函数不做 I/O，不涉及权限；参数校验与审批由工具实现和 agent 层各自负责。
package toolutil
