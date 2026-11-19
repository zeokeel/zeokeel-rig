# 发布流程

本文是发版手册：从改动合并到 `main`，到打出签名 tag、生成草稿 Release、完成发布后复审，再到真正发布；同时列出常见失败的处理方式。读者是持有 tag 推送与 Release 权限的维护者。版本号怎么定、什么算破坏性变更，见 [docs/VERSIONING.md](VERSIONING.md)。

三条不变量决定了下面所有步骤的顺序与形状：

1. **tag 就是 release。** 推一个 annotated、签名的 `vX.Y.Z` tag 才让这个版本能被 `go get` 解析；tag 及其产生的模块校验和一旦发布就不可变。
2. **没有任何步骤自动发布。** tag 触发的 workflow 只创建**草稿**；正式发布是另一次人工 dispatch。
3. **发布构建跑的是合并门禁。** `release.yml` 通过 `workflow_call` 复用 `ci.yml`，所以 tag 上跑的正是 PR 合入时跑的那一套检查，只有一份定义。

## 角色与前置

**角色**

| 角色 | 职责 | 要求 |
| --- | --- | --- |
| 维护者 | 决定版本号、准备发版提交、打 tag、复审草稿、触发发布 | 有 push tag、编辑 Release、`workflow_dispatch` 的权限 |
| 签名密钥持有者 | 用 SSH 私钥签 annotated tag | 公钥已登记在 [`.github/allowed_signers`](../.github/allowed_signers)；私钥由 `@zeokeel-dev` 持有 |
| CI | 跑全部门禁、验签、干净环境冒烟、生成草稿 | 阻塞 job 全绿（见 [主 CI 的 job](#1-pr--主-ci)） |

**前置条件**

- 本次 release 的全部改动已合入 `main`，且 `main` 上的主 CI 是绿的。
- 工作树干净（`git status --short` 无输出），且基于最新的 `origin/main`。
- 发版范围已冻结：发版提交与 tag 之间不再合入功能改动。
- 本地已配置 SSH 签名：

  ```sh
  git config gpg.format ssh
  git config user.signingkey ~/.ssh/id_ed25519   # 与 allowed_signers 中的公钥对应
  ```

- 环境里有 `gh`（GitHub CLI）且已登录；需要能跑 `workflow_dispatch`。
- 仓库侧设置（不在仓库文件内，由维护者维护）：`main` 需要必需 CI 检查、禁止 force-push 与删除；`v*` tag 受 ruleset 保护，禁止更新/删除/强移已发布 tag；`release` environment 存在。

## 整体流程

| 阶段 | 谁 | 产物 |
| --- | --- | --- |
| 1. PR → 主 CI | 贡献者 / 维护者 | `main` 上的发版候选提交，CI 绿 |
| 2. 版本号准备 | 维护者 | `version.go` + `CHANGELOG.md` 小节 + 子模块 `require` 的同一个发版提交 |
| 3. 签名 annotated tag | 维护者 | 远端 `vX.Y.Z`（以及之后的 `session/sqlite/vX.Y.Z`） |
| 4. release workflow | CI | 全部门禁 + 验签 + 干净环境冒烟 + **草稿** Release |
| 5. 发布后复审 | 维护者 | 对草稿与已发布模块的核对结论 |
| 6. publish-release | 维护者 | 正式发布的 GitHub Release |

## 1. PR → 主 CI

主 CI 只有一份定义：`.github/workflows/ci.yml`。它跑在 `push`（`main`）、`pull_request`、`workflow_dispatch` 与 `workflow_call`（被 release workflow 复用）上。**CI 只验证代码**：文档生成物、README/示例同步、版本号与 tag 一致性都不在 CI 里（版本与 tag 由本文件的发布流程和 `release.yml` 校验）。

阻塞 job：

| job（文件里的名字） | 内容 |
| --- | --- |
| `test` | gofmt、build、vet、`go test ./...`、`-race`、两个 `session/sqlite` 子模块步骤（临时 `go.work`）、覆盖率门禁、fuzz 冒烟（10 个目标 × 5s）、benchmark 冒烟、工具账本覆盖、现代工具面审计、依赖策略、cgo-free 检查、根包 stdlib-only、跨平台编译 + `GOOS` vet（windows/amd64、darwin/arm64、freebsd/amd64）、examples 编译 |
| `windows sandbox (real Windows)` | 在真实 `windows-latest` 上跑 `sandbox/osrunner` 的 Windows 后端：纯逻辑 + 真机 ACL/受限令牌强制（`RIG_WINDOWS_SANDBOX_TEST=1`），失败日志进 artifact |
| `lint` | staticcheck v0.8.2、govulncheck v1.9.0（现场 `go install`，用固定版本） |
| `sqlite-consumer smoke` | 在空模块缓存、临时 module 里 `go get` `session/sqlite` 并编译运行（无 `go.work`、无 `replace`），版本读自子模块 `go.mod` 的 `require` |

**发布面自洽不在 CI 检查**：`require` 与根 tag、子模块 tag 的对齐由发版人自查（`git tag -l 'v*'`、`git tag -l 'session/sqlite/*'`、`make version`），并在 `release.yml` 的 `verify` 与 `published-module smoke` 里再次校验。

合并前的要求：阻塞 job 全绿，PR 模板的检查项逐条落实（见 [CONTRIBUTING.md](../CONTRIBUTING.md#pr-与评审)）。

## 2. 版本号准备

1. **定版本号**：按 [docs/VERSIONING.md](VERSIONING.md) 的规则。当前 release line 是 `v0.23.x`，patch 用于修缺陷，minor 用于带新能力的版本；具体号由维护者决定。
2. **写 `CHANGELOG.md`**：把顶部的 `## Unreleased` 替换为 `## vX.Y.Z — YYYY-MM-DD`（房屋风格用长破折号分隔日期）。要求：
   - 每个版本恰好一个小节，`Unreleased` 最多一个且在顶部；
   - 破坏性变更标 `BREAKING` 并写迁移说明；弃用项放 `### Deprecated`；
   - 小节不能为空——发布 job 抽取到空小节会直接失败。
3. **改 `version.go`**：`const Version = "X.Y.Z"`（不带前导 `v`），**保持单行**（脚本按单行读取）。
4. **同步引用**：`README.md` 的安装行（`go get ...@vX.Y.Z`）、`COMPATIBILITY.md` 的 release line 行；工具面若变了，同批更新 `docs/TOOLS.md`、`docs/COVERAGE.md` 的数字；默认值/行为变了，更新 `docs/CONFIG.md`。
5. **子模块**（如果本次一起发 `session/sqlite`）：把 `session/sqlite/go.mod` 的 `require github.com/zeokeel/zeokeel-rig` 改成本次要发的根版本，在 `session/sqlite` 里跑 `go mod tidy`、`go build ./...`、`go vet ./...`、`go test ./... -count=1`，并与根版本的改动放进**同一个发版提交**。
6. **提交并合入**：

   ```sh
   git add version.go CHANGELOG.md README.md COMPATIBILITY.md docs
   # 若一起发子模块：git add session/sqlite/go.mod session/sqlite/go.sum
   git commit -m "chore(release): prepare vX.Y.Z"
   ```

   推分支、开 PR、合入 `main`，等 `main` 上的阻塞 job 全绿。本地建议先跑一遍：

   ```sh
   make ci                          # fmt/build/vet/deps/test/race/覆盖率/ai-docs-check/gate/quickstart/examples/cross/version
   make fuzz FUZZTIME=5s            # 可选：解析器/输入面有改动时
   make bench                       # 可选：benchmark 冒烟
   bash scripts/sync-version.sh vX.Y.Z   # 证明显式 tag 与 version.go 一致
   ```

   `make version` 在未打 tag 的发版提交上打印 `SKIP` 是正常的；release workflow 才严格比对。

## 3. 签名 annotated tag

在通过 CI 的那个发版提交上打 tag：

```sh
git switch main
git pull --ff-only origin main
git tag -s vX.Y.Z -m "ZeoKeel Rig vX.Y.Z"
git tag -v vX.Y.Z                    # 必须显示 good signature，且键在 allowed_signers 里
git cat-file -t refs/tags/vX.Y.Z     # 必须是 "tag"（annotated），不是 "commit"
git push origin vX.Y.Z               # 只推这一个 tag，不要 --tags
```

要求与陷阱：

- **必须 annotated 且签名**。lightweight tag 会被 `verify` job 的 `git cat-file -t` 拦下；签名键不在 `.github/allowed_signers` 里会被 `git tag -v` 拦下。
- **tag 必须指向 `main` 上的提交**。workflow 会用 `git merge-base --is-ancestor` 检查；打在特性分支的提交上会失败。
- **不要 `git push --tags`**，避免把本地无关 tag 一起推出去。
- 打错了怎么办：已推送的 tag 不要移动。修正方式见[常见失败与处理](#常见失败与处理)。

**子模块 tag（顺序不能反）**

```sh
git tag -s session/sqlite/vX.Y.Z -m "session/sqlite vX.Y.Z"
git tag -v session/sqlite/vX.Y.Z
git push origin session/sqlite/vX.Y.Z
```

- 先根 tag，后子模块 tag：子模块的 `require` 指向主模块版本，根 tag 不存在时下游 `go get session/sqlite@vX.Y.Z` 解析不到依赖。
- 子模块 tag 的名字必须带 `session/sqlite/` 前缀（Go 多 module 仓库规则）；两个 tag 可以指向同一个提交。
- `release.yml` 只监听 `v*` 形式的 tag，**推子模块 tag 不会触发发布工作流**——它也不会给子模块生成 Release，这是已知且可接受的行为。

## 4. release workflow 做了什么

触发条件：推送任何匹配 `v*` 的 tag。并发组是 `release-<ref>`，`cancel-in-progress: false`（发布不能被后来的推送打断）。全局 `permissions: contents: read`，只有创建草稿的 job 提升为 `write`。

| job | 依赖 | 作用 |
| --- | --- | --- |
| `ci` | — | `uses: ./.github/workflows/ci.yml`：与合并完全相同的门禁集合 |
| `verify tag` | — | 验签 + 版本一致性 + 提交在 main 上 |
| `published-module smoke` | `verify` | 干净环境消费已发布模块 |
| `create draft release` | `ci`、`smoke` | 从 CHANGELOG 抽正文，创建**草稿** |

**`verify tag`**（`ubuntu-24.04`，`timeout-minutes: 15`）

1. 全历史 checkout（`fetch-depth: 0`）。
2. 恢复 annotated tag 对象（checkout 会把 `refs/tags/<tag>` 重写成提交 SHA）：`git fetch --force --no-tags origin "+refs/tags/<tag>:refs/tags/<tag>"`；`git cat-file -t` 必须是 `tag`。
3. 设置 `gpg.ssh.allowedSignersFile=.github/allowed_signers` 后 `git tag -v <tag>`；失败即 `::error::release tag ... is not signed by a key listed in .github/allowed_signers`。
4. `bash scripts/sync-version.sh --require-tag`（`version.go` 必须等于 tag）。
5. `git fetch origin main` 后 `git merge-base --is-ancestor "$GITHUB_SHA" origin/main`。

**`published-module smoke`**（`ubuntu-24.04`）

- 在临时目录 `go mod init` 一个新模块，`go get github.com/zeokeel/zeokeel-rig@<tag>`；模块代理索引新 tag 有延迟，所以最多重试 **12 次、每次间隔 10 秒**。
- 写入一个同时 import `rig` 与 `kit` 的小程序，`go mod tidy`（必须在 `main.go` 存在之后跑，否则会把依赖丢掉）、`go mod verify`（对校验和数据库核对），再 `go run .` 并 grep 断言输出为 `rig <版本> kit <版本>`。

**`create draft release`**（`ubuntu-24.04`，`permissions: contents: write`，`environment: release`）

- 从 `CHANGELOG.md` 抽取 `## vX.Y.Z` 小节：小节缺失或只有空白字符都直接失败，并提示"add it before tagging"。
- 再跑一次 `scripts/sync-version.sh --require-tag`，确保发布前 `version.go` 仍与 tag 一致。
- `gh release create <tag> --draft --verify-tag --title <tag> --notes-file <文件>`；幂等：Release 已存在时打印提示并原样退出，不改动。

结论：tag 推送后你得到的是一个**草稿**，此时模块已经能被 `go get` 解析（tag 已推），但 GitHub Release 还没对外发布。

## 5. 发布后复审，然后用 publish-release 发布

对草稿与已发布模块做核对：

```sh
gh release view vX.Y.Z
gh release view vX.Y.Z --json isDraft,isPrerelease,tagName,url
```

检查项：

- Release 存在且仍是**草稿**，正文与 `CHANGELOG.md` 的 `## vX.Y.Z` 小节一致（workflow 是原样抽取的，不一致说明 CHANGELOG 本身有问题）。
- tag 是签名的 annotated tag，且指向预期的发版提交（`git rev-parse vX.Y.Z^{commit}`）。
- smoke 的 `go get` + `go mod verify` 通过；`https://pkg.go.dev/github.com/zeokeel/zeokeel-rig@vX.Y.Z` 能渲染（代理可能要几分钟）。
- `https://sum.golang.org/lookup/github.com/zeokeel/zeokeel-rig@vX.Y.Z` 有该版本的校验和记录。
- 若本次一起发子模块：`session/sqlite/vX.Y.Z` 已推送，`require` 指向该版本，且 `sqlite-consumer smoke` 对 `require` 指向的版本是绿的。

全部通过后，用单独的工作流发布（这是不可逆的一步，所以做成独立的人工 dispatch）：

```sh
gh workflow run publish-release.yml -f tag=vX.Y.Z -f confirm_reviewed=yes
gh run watch <run-id>
```

`publish-release.yml` 的 `publish draft release` job 只有在 `confirm_reviewed == 'yes'` 时才跑，步骤是：checkout 该 tag（`fetch-depth: 0`、`persist-credentials: false`）→ 校验 `refs/tags/<tag>` 可解析 → `bash scripts/sync-version.sh <tag>` → 用 `gh release view --json isDraft` 确认它**仍是草稿**（否则拒绝再次发布）→ `gh release edit <tag> --draft=false --latest` → 打印发布后的 JSON。

## macOS 真机沙箱验证（独立 workflow，不阻塞）

macOS 的 Seatbelt 后端只有真机能验：语法正确但内核不接受的 SBPL、syscall 白名单漏项，都只在真机暴露。这项验证放在**独立的** `.github/workflows/macos-sandbox.yml`，不在 `ci.yml` 里，也不在发布链路里：

- **为什么独立**：`v0.23.4` 之前 macOS job 被系统性取消——`started_at` 到 `completed_at` 恰好 15 分 02 秒且 **steps 为空**（一个步骤都没执行），而同一天早些时候同一个 job 96 秒就跑绿过。这是 runner 侧的调度问题，不是测试挂住（已用 `-timeout` 排除）。**被取消的 job 无法用 `continue-on-error` 中和**（取消不是失败），会连累整个 run 变红，所以移出阻塞链路。
- **触发条件**：`push` 到 `main` 且改动命中 `sandbox/**`、`internal/procattr/**`、`tools/builtin/**` 或这个 workflow 文件本身；或 `workflow_dispatch`。**tag 推送不触发它。**
- **job**：`macos sandbox (real macOS)`，固定 `macos-26` 镜像（不用 `-latest`，避免随退役漂移；曾用 `macos-15-intel`，跑绿过但排期容量不稳定），`timeout-minutes: 30`。步骤：`go build ./...` → `go test ./sandbox/osrunner -count=1 -v -timeout 8m` → `go test ./sandbox/osrunner -run 'TestSeatbelt|TestDarwin|TestSandboxExec' -count=1 -v -timeout 8m`（真机 Seatbelt 强制）→ 上传 artifact `macos-sandbox-test-log`。
- **对发布的影响**：没有。它红不代表发布失败，也没有 macOS 真机结论跟随某次发布；这是[已披露的限制](../SECURITY.md#已披露限制)。runner 恢复稳定后可以把它并回 `ci.yml` 并恢复为阻塞门禁。

## 常见失败与处理

顺序原则始终是：**能向前修就向前修；已发布的 tag 永不移。**

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| `verify` 报 `refs/tags/vX.Y.Z is not an annotated tag object` | 用了 lightweight tag（`git tag vX.Y.Z`） | tag 已推：不要移动已发布 tag；Release 尚未生成且 proxy/sumdb 尚未抓取该 tag 时，可删远端 tag 后重打 annotated tag；否则向前修，发新 patch |
| `verify` 报 `is not signed by a key listed in .github/allowed_signers` | 没签名、签错键，或公钥未登记 | 同上；同时把正确公钥补进 `.github/allowed_signers`（这属于仓库变更，要走 PR） |
| `sync-version: version mismatch: tag ... but version.go has ...` | `version.go` 与 tag 不一致（最常见：忘记改 `version.go`，或 tag 打在了旧提交上） | tag 未推：改 `version.go` 后重新提交、重打 tag。tag 已推：向前修，发新 patch |
| `tag ... does not point at a commit on main` | tag 打在未合入 `main` 的提交上 | 在 `main` 的发版提交上重打（此时 tag 尚未进入发布流程，可删可重）；若已产生 Release，发新 patch |
| `create draft release` 报 `CHANGELOG.md has no '## vX.Y.Z' section` / `section ... is empty` | CHANGELOG 小节缺失、版本号写法不符或为空 | 补小节后必须随**新 tag** 发布（tag 内容不可变）。若只是草稿阶段且 Release 不存在，可直接删 tag 重来——先确认 tag 未被 proxy 抓取 |
| smoke 报 `clean-room go get of vX.Y.Z failed after retries` | 代理尚未索引新 tag、tag 未真正推送、模块路径/版本写错 | 等几分钟重跑该 job（无 Release 时重跑安全）；确认 `git ls-remote --tags origin` 能看到该 tag |
| `published-module smoke` 报 `missing go.sum entry` 类错误 | smoke 脚本在 `main.go` 之前跑了 `go mod tidy` | 这是脚本缺陷，修 `ci.yml` 的 smoke 步骤，不要改消费方工程 |
| 发版前自查：`require` 与 tag 不一致 | 子模块 `require` 不等于最近根 tag、`session/sqlite/<根 tag>` 不存在、或子模块 tag 不含根 tag 提交 | 先打根 tag，再打带 `session/sqlite/` 前缀的子模块 tag，两者指向同一提交；`release.yml` 的 `verify`/`smoke` 会拦下不一致 |
| `sqlite-consumer smoke` 红 | 子模块 `go.mod`/`go.sum` 与真实依赖不符（`go mod tidy` 未跑干净）、或 `require` 指向不存在的版本 | 在 `session/sqlite` 里 `go mod tidy`、提交，重跑；注意它用空 `GOMODCACHE`，本地热缓存绿不算数 |
| macOS job 被取消 / 变红 | runner 侧调度（取消时 steps 为空），或真机 Seatbelt 行为变化 | 与发布无关：独立 workflow、tag 不触发。看 `macos-sandbox-test-log` artifact 与日志；真机失败要当缺陷修 |
| 想改草稿正文 | 草稿已用 CHANGELOG 生成 | 重新生成需要新 tag。发布前发现正文错误：先修 CHANGELOG 并向前发新 patch；已发布 Release 的正文不要改历史，改在下一版说明 |

**重跑规则**

- 只要 **Release 对象还不存在**，重跑失败的 release job 是安全的。
- 一旦 Release 存在（哪怕是草稿），不要重新推 tag；需要改内容就发新版本。
- 已发布（非草稿）的版本只能向前修：新的 patch + `retract`（如需要）+ 安全公告（如适用）。

## 发布后检查清单

```text
[ ] 干净目录里 go get github.com/zeokeel/zeokeel-rig@vX.Y.Z 成功，go mod verify 通过
[ ] https://sum.golang.org/lookup/github.com/zeokeel/zeokeel-rig@vX.Y.Z 有记录
[ ] https://pkg.go.dev/github.com/zeokeel/zeokeel-rig@vX.Y.Z 正常渲染
[ ] Release 已不是草稿，正文与 CHANGELOG 的 ## vX.Y.Z 小节一致
[ ] docs/ai 已生成：go run ./cmd/rigdoc 无 diff（CI 的 rigdoc -check 绿）
[ ] README 安装行、COMPATIBILITY.md 指向新版本；工具面数字与 docs/TOOLS.md、docs/COVERAGE.md 一致
[ ] CHANGELOG 顶部已开新的 ## Unreleased 段
[ ] version.go 停在已发布版本（不要提前 bump）
[ ] 子模块：session/sqlite/vX.Y.Z 已推送，sqlite-consumer smoke 对它绿
[ ] Dependabot 的 GitHub Actions SHA pin PR 仍然绿
```

## 热修复

1. 从出问题的 tag 拉分支：`git switch -c hotfix/X.Y.Z+1 vX.Y.Z`。
2. 做最小修复 + 回归测试；更新 `version.go`、`CHANGELOG.md`（`## vX.Y.Z+1 — YYYY-MM-DD`）与受影响文档。
3. `make ci` + 针对性测试；开 PR 合入 `main`，然后按第 3 步在 `main` 的合并提交上打**新的**签名 annotated tag。
4. 同一个 release workflow 生成草稿，同样的复审与 publish 步骤。
5. 不要改写上一版的 Release 正文；补丁说明属于新版本。

## 相关文档

- [docs/VERSIONING.md](VERSIONING.md) — 版本号规则、tag 不可变、弃用策略
- [CONTRIBUTING.md](../CONTRIBUTING.md) — 本地门禁与 PR 要求
- [SECURITY.md](../SECURITY.md) — 安全修复的披露流程与支持版本
- [CHANGELOG.md](../CHANGELOG.md) — 发布正文的唯一来源
- [COMPATIBILITY.md](../COMPATIBILITY.md) — release line 的 Go 版本与依赖对照
- [`.github/workflows/release.yml`](../.github/workflows/release.yml)、[`ci.yml`](../.github/workflows/ci.yml)、[`publish-release.yml`](../.github/workflows/publish-release.yml)、[`macos-sandbox.yml`](../.github/workflows/macos-sandbox.yml)