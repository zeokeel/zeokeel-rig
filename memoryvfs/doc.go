// Package memoryvfs 定义"记忆上下文数据库"的公共契约。
//
// 它把长期记忆、资源与技能统一到一棵以 `mem://` 为根的虚拟文件系统里：
//
//	mem://user/self/memories/<type>/<slug>   记忆（profile/preferences/entities/...）
//	mem://user/self/peers/<peer>/memories/…  按 peer（工作区/项目）分区的记忆
//	mem://resources/<path>                   导入的文本资源
//	mem://skills/<name>                      技能文档
//	mem://user/self/rules/<slug>             编译出的行为规则
//	mem://sessions/<id>/<ts>                 会话归档
//
// 每个条目都带三层内容：`Summary`（L0，索引与召回用）、`Overview`（L1，导航用）、
// `Content`（L2，按需读取）。目录本身也有 L0/L1 sidecar，因此"先看摘要、再读正文"
// 不需要把整棵树加载进上下文。
//
// 契约放在公开包里，实现放在 `internal/memvfs`（文件后端）。宿主可以只实现自己
// 需要的子接口（FS/Retriever/Sessions/…）来替换存储，也可以实现 Embedder/
// Reranker/Summarizer/Extractor 把模型能力接进来。所有能力都是可选的：没有
// Embedder 时检索退化为 BM25，没有 Summarizer 时用确定性摘要，默认不联网。
//
// 实现必须并发安全；索引类方法只返回元数据，正文只能通过 Read 取。
package memoryvfs
