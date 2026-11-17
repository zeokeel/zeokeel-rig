// Package search 提供按相关性排序的工作区检索工具 search。
//
// 对标 Codex 的 file-search（文件名模糊匹配）、core/search_tool_bm25（内容 BM25
// 检索）与 core/mentions（@提及解析）。它补的是仓库里 glob（精确模式）与 grep
// （正则逐行）之间的空白：这两个工具回答"哪里匹配"，search 回答"哪里最相关"。
//
//	search {query, path?, mode?, max_results?}
//
// 四个模式：
//
//   - files：文件名模糊搜索。子序列匹配、大小写不敏感；打分由连续匹配、词首匹配、
//     目录段相等、命中文件名与路径长度共同决定（见 FuzzyScore）。查询含 "/" 时按
//     路径段顺序匹配（`src/search` 命中 src/search/deep.go）。结果按相关性排序，
//     每行一个相对路径。
//   - text：内容检索。对文本文件做确定性分词后建 BM25 索引（k1=1.2、b=0.75），
//     多个词是 AND 语义；每条结果是 `path:line:content`（命中行取命中最多的那一行，
//     内容截断到 200 字符）。
//   - mentions：把 query 当成一段文本，解析 @path、@"path with space"、@path:12、
//     @path:12-20，逐条校验存在性；找不到的标注 `(missing)`。
//   - auto（默认）：query 以 @ 开头或含 @ 且像自然语言 → mentions；含空白 → text；
//     否则先 files，没有命中再回退到 text。
//
// # 边界与确定性
//
//   - 所有结果都相对工作区根渲染，且必须落在根内：path 参数逃出根目录会报错，
//     遍历不跟随符号链接，提及解析到根外视为 missing。
//   - 跳过 .git、node_modules、vendor、dist、build；跳过二进制（含 NUL）与超过
//     WithMaxFileBytes（默认 1 MiB）的文件——文件名搜索不读内容，因此不受该上限
//     影响。
//   - 单次预算 WithTimeout（默认 10s）：超时返回可读错误，父 ctx 取消则透传取消。
//   - 输出文本稳定可断言：顺序由分数 + 路径决定，被 max_results（默认 20、上限
//     200）截断时追加 `[truncated: N more matches]`；没有命中返回空串（与
//     glob/grep 的约定一致）。
//   - FuzzyScore、ParseMentions、Tokenize 是导出的纯函数，便于其它包复用同一套
//     打分/解析/分词规则。
//   - **硬链接**：遍历按 lstat 判定"常规文件"，但硬链接是同一 inode 的另一个名字，
//     它可能在根之外——内容检索因此可能读到根外的数据（不是路径逃逸）。无法从
//     lstat 判断另一个名字的位置，所以只提供整类跳过：`WithHardlinkGuard(true)`
//     （默认关）会跳过硬链接数 > 1 的文件。Windows 上拿不到链接数，守卫退化为
//     不跳过。
//   - **内存索引**：内容检索每次调用都在内存里现建 BM25 索引（不分片、不缓存），
//     内存峰值与"入选文件的总字节数"同阶；大仓库请配合 WithMaxFileBytes 与
//     WithMaxResults 收窄，或在宿主侧先做一次粗筛。
//
// 工具不注册自己：模型的工具目录由 tools/catalog、tools/presets 与 tools/full
// 决定，宿主把 search 接进这些地方后模型才可见。
package search
