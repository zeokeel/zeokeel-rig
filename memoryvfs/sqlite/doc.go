// Package sqlite 是 memoryvfs.Store 的 SQLite 后端。
//
// 条目按 (workspace_id, uri) 存一行，URI 仍是 mem:// 的文件夹式路径；召回在
// SQLite 中按工作区取出候选，再用 Go 侧的词法评分排序。它不引入新的根模块
// 依赖：宿主显式 import 本子模块并使用 modernc.org/sqlite。
package sqlite
