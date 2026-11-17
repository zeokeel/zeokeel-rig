# 个人客户端对抗审计 · 第 2D 轮：数据丢不丢 / 撤销骗不骗人

- **对象**：`session/store`（重写版）、`tools/undo`（git 化）、`internal/gitsnap`、`tools/patch`、`internal/fsguard`
- **审计者**：`dsh-sandbox-audit`｜**方式**：只读 + 新增 `audit2_*_test.go`，不改实现、不 commit
- **环境**：linux/amd64，Go 1.27，真实 `git`（临时仓库），`CGO_ENABLED=0`（另跑 `-race` 需要 cgo）
- **时间**：2026-11-18

## 一句话结论

**没有发现数据丢失或撤销不完整**。五个包在"最危险的组合"（真实 git 仓库里五种改动 + `Revert`）下都逐字节正确；撤销甚至比文档承诺的更稳（陈旧 `index.lock` 不影响快照正确性）。共 3 条 P2 级问题：**回合前的"部分暂存"会被快照的工作区内容取代**、**首条消息超过 256 KiB 时标题永久为空**、**跨实例 `Load` 返回 ErrNotFound**（数据不丢，但可用性/文档缺口）。

---

## 0. 结论摘要

| # | 级别 | 一句话 | 结论 |
|---|---|---|:--:|
| **U-S1** | P2 | 回合前 `git add` 了 v2、又把工作区改成 v3（部分暂存）→ 回退后 index 变成 v3，**v2 的暂存状态消失** | ⚠️ 语义边界（文档未写） |
| **S-P1** | P2 | 首条消息 300 KiB（用户粘贴大日志的常见场景）→ 会话标题**永久为空**，不再尝试后续消息 | ⚠️ 可用性 |
| **S-P2** | P2 | 同一目录两个 Store 实例：`Load` 另一个实例新建的会话返回 `ErrNotFound`（文件在、内容完整） | ⚠️ 文档缺口 |
| U-S2 | — | 五种改动（改文件/新建/删除/改权限/改名）→ `Revert` 后工作区**逐字节**回到快照 | ✅ 成立 |
| U-S3 | — | 新建文件被删、`.gitignore` 忽略的文件**不动**、无关文件不动 | ✅ 成立 |
| U-S4 | — | 指定回合回退（5 回合后 `Revert(2)`）准确：回退 turn2–5 的改动，保留 base | ✅ 成立 |
| U-S5 | — | `refs/rig/undo/*` 不泄漏：20 次快照 = 20 个 ref，`Prune(5)` → 5 | ✅ 成立 |
| U-S6 | — | 陈旧 `.git/index.lock` **不会**让快照退化（临时 index 方案），脏工作区回退保留用户未提交内容 | ✅ 成立（超出文档承诺） |
| U-S7 | — | git 缺失 / 非仓库 / merge 中间态 → 报错可读、不动工作区 | ✅ 成立 |
| S-S1 | — | 4 个 goroutine 并发写不同会话（`-race`）→ 每个会话 25 条一条不少 | ✅ 成立 |
| S-S2 | — | kill -9 半行：重开后半行被丢弃、旧字节不被破坏、后续追加正常 | ✅ 成立 |
| S-S3 | — | 中间插入一条坏行 → 其余 10 条完整且**顺序不乱** | ✅ 成立 |
| S-S4 | — | `Delete` 之后不复活（同实例 + 重开） | ✅ 成立 |
| P-S1 | — | 多文件补丁中途失败 → 全部回滚、**零临时文件残留** | ✅ 成立 |
| P-S2 | — | CRLF 保持 CRLF；无尾换行保持无尾换行；二进制被拒 | ✅ 成立 |
| P-S3 | — | 同一补丁 add+update 同一文件 → 碰盘前拒绝；`Move to` 跨目录正确 | ✅ 成立 |
| F-S1 | — | fsguard 原子写失败留旧内容、无残留；并发读写永远读到完整内容 | ✅ 成立 |

---

## 1. `tools/undo` + `internal/gitsnap`（最危险的组合）

**方法**：真实 git 仓库（`git init` + 真提交），`undo.NewTracker` → `Begin()` → 改动 → `Revert(turn)`。
用例：`tools/undo/audit2_revert_test.go`（8 条）。

### U-S2 五种改动 → 逐字节回到快照 ✅

| 改动 | 验证 | 结果 |
|---|---|---|
| 改文件 `modify.txt` | 内容 | ✅ 回到 `"original\n"` |
| 新建 `brand-new.txt` | 存在性 | ✅ 被删除 |
| 删除 `delete-me.txt` | 内容 | ✅ 恢复为 `"will vanish\n"` |
| 改权限 `chmod-me.sh` 0644→0755 | 权限位 | ✅ 回到 0644 |
| 改名 `rename-me.txt`→`renamed.txt` | 两个路径 | ✅ 源恢复、目标删除 |

变更清单：`[add:brand-new.txt delete:delete-me.txt modify:chmod-me.sh modify:modify.txt rename:renamed.txt]` —— 5 条，与五种改动一一对应（chmod 正确归一为 `modify`，因为内容没变）。
回退后 `git status --porcelain` **为空**。`.gitignore` 里的 `ignored/cache`（回合内被 agent 改过）**保持回合内的内容不动** ✅ 与文档承诺一致。

### U-S1【P2】回合前的"部分暂存"会被取代

| 情形 | 实测 |
|---|---|
| A：`git add` 后工作区没再改（index == worktree） | 回退后工作区 = v2，`git diff --cached` 仍是 `+v2 staged` ✅ **暂存状态保留** |
| B：`git add` v2 后工作区又改成 v3（index ≠ worktree） | 回退后工作区 = v3 ✅；但 `git diff --cached` 变成 `v1 → v3`，**v2 的暂存状态消失了** |

原因：`gitsnap` 的快照基线是**工作区**内容（`worktreeTree` 用临时 index 跑 `git add -A` + `write-tree`），恢复时用 `git checkout <base> -- paths`，它会把 index 一起写成 base 内容。要精确恢复"回合前的 index"需要记录 stash 的 index 提交（`git stash create` 的第二父），当前实现没记。

**最小修法**（三选一，按成本递增）：
1. 文档写清楚：「回退把工作区与暂存区都恢复到回合前的**工作区状态**；如果你在回合前做了"部分暂存"，暂存区会被展平成回合前的工作区内容」——零代码；
2. `Snapshot` 额外记录 `git write-tree` 得到的 index 树（或在 `stash create` 的第二父上），回退时对未提交路径 `git read-tree` 恢复 index；
3. 回退时对"回合前 index ≠ worktree"的路径**不动 index**（只恢复工作区），把选择权留给用户。

### U-S4 指定回合 ✅

5 个回连续 `Begin` + 各建一个文件，`Revert(2)` → turn2–5 的 4 个文件全部删除、`base.txt` 保留、返回 4 条变更。语义是"回到第 2 回合开始前"（与工具描述 "undo the changes made since the start of a turn" 一致）。

### U-S5 引用不泄漏 ✅

20 次 `undo.Take` → `refs/rig/undo/*` = **20**；`gitsnap.Prune(ctx, dir, 5)` → **5**。`Snapshot` 里的自动修剪上限是 128（`defaultRefRetain`），因此单用户连续撤销不会无限增长；但**注意**：`undo` 包不导出 `Prune`，宿主想把上限调小只能直接依赖 `internal/gitsnap`（内部包，外部不可用）→ 建议 `undo.Options` 暴露 `RefRetain`。

### U-S6 陈旧 `index.lock` 反而更稳（正面发现）

`worktreeTree` 不碰真实 index：复制真实 index 到临时文件（还特意保留原 mtime 以让 git 的 racy-git 判定正常工作）、设 `GIT_INDEX_FILE` 后再 `git add -A` + `write-tree`。因此：
- 真实 `.git/index.lock` 存在时**快照照样正确**（直测：`git stash create` 在同样条件下会直接失败）；
- 脏工作区 + 陈旧锁：回退后工作区回到**用户回合前的未提交内容**，没有被 HEAD 覆盖（这条用例专门防"锁失败→退化到 HEAD→吃掉用户改动"）。

### U-S7 git 缺失 / 非仓库 / merge 中间态 ✅

| 情形 | 实际 |
|---|---|
| `PATH` 里没有 git | `gitsnap: git rev-parse --is-inside-work-tree: exec: "git": executable file not found in $PATH`，工作区不变 |
| 不是仓库 | `errors.Is(err, undo.ErrNotGitRepo)` ✅，目录不变 |
| `MERGE_HEAD` 存在 | 要么报错可读、要么成功；两种情况 `MERGE_HEAD` 与 `HEAD` 都完好、工作区没被改 |

---

## 2. `session/store`（重写版）

用例：`session/store/audit2_store_test.go`（8 条）+ `audit2_perf_test.go`。

| 项 | 实测结果 |
|---|---|
| 4 个 goroutine 并发写不同会话（两个 Store 实例交叉，`-race`） | 4×25 条，每个会话 `len(messages)==25`、`Meta.Messages==25`、首条消息正确 ✅ |
| kill -9 半行（手工追加 `{"role":"user",...` 无换行） | 重开后 `Load`=3 条（半行丢弃）、`Meta.Messages`=3；续写后 4 条；**已写入的字节前缀逐字节不变** ✅ |
| 中间插入一条完整但非法 JSON 的行 | 其余 10 条完整、**顺序不乱**、`Meta.Messages`=10（坏行不计入）✅ |
| `Delete` 后不复活 | 同实例 `Load`/重复 `Delete` 都是 `ErrNotFound`；重开扫目录也不复活 ✅ |
| 两个实例同目录 | 第二个实例 `List` 看不到（文档承认）；**文件在盘上、内容完整**、新实例能读 ✅ 数据不丢 |

### S-P1【P2】首条消息 300 KiB → 标题永久为空

`titleScanBytes = 256 KiB`：`scanTitle` 遇到超长行时 `tooLong` 直接 `return ""`，**不再看后续记录**。实测：首条 user 消息 300 KiB、第二条 user 消息 `"readable title"` → `List` 的 Title 仍是 `""`。

影响：用户第一次交互就是"粘贴一大段日志/代码"（很常见）时，会话列表里这条会话永远没有标题。数据本身完整（`Load` 拿到 2 条，300 KiB 内容未截断）。

**最小修法**：`tooLong` 分支不要 `return`，而是跳到下一行继续扫描（上限 `titleScanRecords=4` 仍在，扫描字节预算仍受控）；或把预算从"单行上限"改成"累计已读字节上限"。

### S-P2【P2】跨实例 `Load` 返回 ErrNotFound

第二个实例 `Load` 第一个实例新建的会话 → `session store: session not found`。原因：`Load` 先 `lookup(id)` 查内存索引，未命中就返回 `ErrNotFound`，**没有回退读盘**。

影响：同一数据目录开两个 Store（两个窗口 / 宿主与子进程）时，表现是"文件明明在、却读不出会话"。数据不丢。

**最小修法**：① 文档写明"同一目录只应由一个 Store 实例写入，其余实例需自行重扫"；或 ② `Load` 未命中索引但 `sessionPath(id)` 存在时，补索引后继续（`correctMeta` 已有校正逻辑，复用即可）。

---

## 3. `tools/patch` + `internal/fsguard`

用例：`tools/patch/audit2_atomic_test.go`（6 条）、`internal/fsguard/audit2_atomic_test.go`（2 条）。

| 项 | 实测结果 |
|---|---|
| 多文件中途失败（第 1 个文件可改、第 2 个上下文匹配不到） | 整体失败；朴素整树快照对比 **零差异**；目录里**没有**点开头/`.tmp`/`~`/随机名残留 ✅ |
| CRLF 文件 | `line1\r\nLINE2\r\nline3\r\n` —— 行尾保持 CRLF ✅ |
| 无尾换行文件 | 打完仍是 `"one\nTWO"`（没有凭空加换行）✅ |
| 二进制（含 NUL） | 拒绝：`apply_patch: data.bin: binary file (contains a NUL byte); apply_patch only edits text files`，内容不变 ✅ |
| 同一补丁 add+update 同一文件 | 碰盘前拒绝：`dup.txt: appears twice in the same patch (first at line 2); split the changes into separate patches`，工作区零改动 ✅ |
| `Move to` 跨目录 | 目标内容正确、源被移除、**源目录保留**（只移动文件）✅ |
| fsguard 原子写失败（目标是目录） | 旧内容与目录内容都完好；**无临时文件残留** ✅ |
| fsguard 原子写并发读 | 200 次写 + 200 次读并发：每次读到的都是完整内容（无半截）；结束后目录里只有目标文件 ✅ |

---

## 4. 性能

| 场景 | 实测 |
|---|---|
| **1000 行文件 × 20 个小补丁**（串行 `Invoke`） | **总计 14.6 ms，平均 732 µs/补丁**；行数不变、最后一轮生效 |
| `session/store` **追加 10k 条** | 269.9 ms（**平均 27 µs/条**），文件 750 000 字节 |
| `session/store` **Load 10k 条** | **38.0 ms** |
| 关闭后**重开 + Load 10k 条** | **42.2 ms** |

结论：没有性能问题；打开"会话列表 + 选中会话"这条产品路径在 10k 条量级是 ~40 ms。

---

## 5. 本轮新增用例

| 文件 | 用例 |
|---|---|
| `tools/undo/audit2_revert_test.go` | `TestAuditRevertRestoresFiveChangeKinds`、`TestAuditRevertAndPreTurnStagedChanges`（A/B 两情形）、`TestAuditRevertSpecificTurnAfterFiveTurns`、`TestAuditUndoRefsDoNotLeak`、`TestAuditUndoWithoutGitBinary`、`TestAuditUndoOnNonRepo`、`TestAuditUndoDuringMerge`、`TestAuditStaleIndexLockKeepsSnapshotCorrect` |
| `session/store/audit2_store_test.go` | 并发 4 写者（2 实例）、半行恢复、坏行跳过与顺序、Title 五种形态、300 KiB 首行、Delete 不复活、两实例同目录、跨实例 Load |
| `session/store/audit2_perf_test.go` | 10k 条的追加/Load/重开耗时 |
| `tools/patch/audit2_atomic_test.go` | 原子回滚零残留、CRLF、无尾换行、二进制拒绝、add+update 冲突、跨目录 Move |
| `tools/patch/audit2_perf_test.go` | 1000 行 × 20 补丁耗时 |
| `internal/fsguard/audit2_atomic_test.go` | 原子写失败保旧值无残留、并发读写无半截 |

```bash
export PATH=$PATH:/usr/local/go/bin
CGO_ENABLED=0 go test ./session/store/ ./tools/undo/ ./internal/gitsnap/ ./tools/patch/ ./internal/fsguard/ -count=1
CGO_ENABLED=1 go test -race ./session/store/ ./tools/undo/ ./tools/patch/ -count=1
```
全绿（含原有用例）；`-race` 干净。

---

## 6. 未验证（诚实边界）

| 项 | 原因 |
|---|---|
| 真 `kill -9` 进程 | 无法在单进程测试里杀自己；用"手工追加半行"复现同样的磁盘状态（这是恢复逻辑唯一看到的输入） |
| Windows/macOS 上的权限位、大小写、CRLF 自动转换 | 只在本机 linux 跑；`git config core.autocrlf=false` 已显式设置以排除干扰 |
| 超大型仓库（10 万文件）的快照耗时 | 只测了"20 次快照 / 1000 行文件"量级；大仓的 `git add -A` 成本未测 |
| `git stash create` 在 submodule / sparse-checkout 下的行为 | 未覆盖；`Change.Reason` 里已有 submodule 的跳过路径，但没构造真实 submodule |
| 两个进程同时写**同一个**会话 | 只在单进程内用两个 Store 实例测；跨进程并发追加未验证（store 只保证进程内分片锁） |

---

## 7. 三条最严重的（附最小修法）

1. **U-S1｜P2｜回退会把"部分暂存"展平**：回合前 `git add` v2 后又改工作区到 v3，回退后 index 变成 v3，v2 的暂存状态消失（数据对象仍在 git 里，但用户的"暂存意图"丢了）。
   **最小修法**：先写进文档（最省），再考虑快照记录 index 树（`git write-tree`）并在回退时 `read-tree` 恢复。
2. **S-P1｜P2｜300 KiB 首条消息 → 标题永久为空**：`scanTitle` 遇 `tooLong` 直接放弃，不再看后面的 user 消息。用户"粘贴大日志开场"是常见用法，会话列表会一直显示空标题。
   **最小修法**：`tooLong` 时跳过该行继续扫描（`titleScanRecords=4` 仍是上限）。
3. **S-P2｜P2｜跨实例 `Load` 返回 ErrNotFound**：文件在、内容完整，但内存索引没有就报"不存在"。
   **最小修法**：文档写明单实例约束，或 `Load` 未命中索引时回退读盘并补索引（复用 `correctMeta`）。

> 补充建议（非缺陷）：`undo` 包没有导出 `Prune`，宿主无法把 ref 保留数从默认 128 调小；要么在 `undo.Options` 加 `RefRetain`，要么导出 `undo.Prune`。