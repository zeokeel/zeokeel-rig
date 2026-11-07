# 发布流程

以下是发布经理切一个 release 时遵循的清单。版本号策略见 [docs/VERSIONING.md](VERSIONING.md)；发布自动化是 [`.github/workflows/release.yml`](../../.github/workflows/release.yml) 加上[`.github/workflows/publish-release.yml`](../../.github/workflows/publish-release.yml) 中的人工发布步骤。

有三件事决定这个流程：

1. **tag 就是 release。** push 一个 annotated、签名的 `vX.Y.Z` tag 才让版本可被 `go get` 解析；tag 及其产生的 module 校验和一旦发布即不可变。
2. **tag 运行完整合并门禁。** `ci.yml` 是可复用工作流（`workflow_call`）：release 工作流调用它，因此打 tag 时跑的正是合并所跑的门禁——包括覆盖率下限、fuzz/bench 冒烟、staticcheck、govulncheck、依赖策略（恰好一个经审查的解析器依赖）、无 cgo 构建/测试门禁与交叉编译。单元测试套件在 Linux 上运行；windows/darwin/freebsd 交叉编译并做 `GOOS` vet（本仓库没有托管 macOS/Windows runner —— 已在 README.md 披露）。门禁定义只有一份，不是两份。
3. **不会自动发布。** tag 工作流验证 tag 并创建**草稿** GitHub Release；维护者在发布后复审（本文 §6）通过后，才用 `publish-release.yml` 发布草稿。

## Release 工作流做什么

由匹配 `v*` 的任意 tag push 触发：

1. **CI job**（`uses: ./.github/workflows/ci.yml`）：完整合并门禁集（gofmt 检查已跟踪文件、build、vet、test、test -race、80% 总覆盖率 / 70% 每包覆盖率下限、每目标 5s 的 fuzz 冒烟、benchmark 冒烟、catalog 与现代面门禁、依赖策略与根包叶子检查、README 漂移、windows/darwin/freebsd 交叉编译 + `GOOS` vet、examples，以及 staticcheck + govulncheck lint；单元测试套件在 Linux 上运行（本仓库没有托管 macOS/Windows runner —— 已在 README.md 披露），bash/jobs/terminal 工具仅支持 Unix）。
2. **verify job**：`fetch-depth: 0` checkout；检查 tag 由 [`.github/allowed_signers`](../../.github/allowed_signers) 中的密钥签名（`git tag -v`）；检查 `version.go` 中的 `const Version` 与 tag 一致（`scripts/sync-version.sh --require-tag`）；检查被打 tag 的提交是 `origin/main` 的祖先。任一步失败都会在 Release 对象存在之前中止发布。
3. **smoke job**：在全新 module 中通过公共 proxy 做干净环境的 `go get github.com/zeokeel/zeokeel-rig@<tag>`，在 proxy 索引新 tag 期间重试；`go mod verify` 用校验和数据库校验模块；一个小程序 import `rig`+`kit` 并断言两者都报告 tag 版本。
4. **publish job**：`permissions: contents: write`、`environment: release`；自行复核 tag（必须属于该次 release 工作流运行），从 `CHANGELOG.md` 提取匹配的 `## vX.Y.Z` 小节（缺失或为空则发布失败），并创建**草稿** GitHub Release（`gh release create --draft --verify-tag`）。它是幂等的：已存在的 Release 不会被改动。

`main` 与 `v*` tag 在 GitHub 上受保护：必须通过必需 CI 检查，force push 与删除被拒绝，tag ruleset 阻止移动/删除已发布 tag。在没有 Release 存在时重跑失败的 release job 是安全的；Release 存在后，用新的 patch 版本向前修复。

## 前置条件

- 该 release 的所有工作已合并到 `main`，且 `main` 在 CI 中为绿。
- 工作树干净（`git status --short` 为空），且在最新 `main` 上（`git fetch origin && git log --oneline -5`）。
- release 范围已冻结；release commit 与 tag 之间不再有新功能落地。
- 你拥有 push tag、编辑 release 与运行 `publish-release` 的维护者权限。
- 你的签名密钥公钥在 `.github/allowed_signers` 中，且本地已配置（`git config gpg.format ssh`、`git config user.signingkey <key>`）。

## 仓库保护状态（单人维护者披露）

仓库配置为“在不要求第二维护者的前提下尽可能强的保护”，剩余限制在此明说而不是暗示：

- `main` 要求 `test` 与 `lint` CI 检查（strict）、签名提交、线性历史与 conversation resolution；force push 与删除被拒绝。`v*` tag ruleset 阻止更新、删除或 force-move 已发布 tag（无 bypass actors）。
- 有意保留 `enforce_admins=false`，以便唯一维护者能从坏门禁中恢复；这也意味着必需检查/签名不会约束该账号。这是安全阀，不是强制的同伴评审。
- 没有必需的 PR 评审人：只有一个维护者时，自评审要求是空文（GitHub 不允许批准自己的 PR）。
- 只有 SSH 签名公钥注册到账号（`admin:ssh_signing_key`）后，GitHub 才会把提交显示为 *Verified*。release 工作流独立对照 `.github/allowed_signers` 验证 tag 签名，这才是项目级信任锚；账号徽章是独立、可选的步骤。
- `release` environment 存在但没有 required-reviewer 规则；发布由独立的 `publish-release` workflow dispatch（`confirm_reviewed=yes`）在流程上把关，而不是环境评审人。

如果项目获得第二维护者，后续动作是：要求 PR 评审、设置 `enforce_admins=true`、在两个账号注册签名公钥、给 `release` environment 加必需评审人。

## 清单

### 1. 准备 release 提交

1. 按 [docs/VERSIONING.md](VERSIONING.md) 决定版本号：纯修复用 patch，pre-1.0 功能线用 minor，只有项目刻意做出稳定性承诺时才用 `v1.0.0`。当前 release line 是 `v0.12.x`；在 docs/VERSIONING.md 中的 API 冻结工作完成前不计划 `v1.0.0`。
2. 更新 `CHANGELOG.md`：
   - 用 `## vX.Y.Z — YYYY-MM-DD`（em dash）替换顶部 `## Unreleased` 小节；每个版本恰好一个小节；
   - 确认每个破坏性变更都标记 `BREAKING` 并有迁移说明，每个弃用都在 `### Deprecated` 下；
   - release 工作流把这个小节原样复制到 GitHub Release。
3. 更新 `version.go`：`const Version = "X.Y.Z"`（无前导 `v`）。
4. 更新文档中的版本引用：
   - `README.md` 安装行（`go get ...@vX.Y.Z`）与 “current release” / 路线图条目；
   - `COMPATIBILITY.md` 的 release-line 行；
   - 工具面变化时更新 `docs/COVERAGE.md` / `docs/TOOLS.md` 计数；
   - 安全相关行为变化时更新 `docs/ARCHITECTURE.md` / `SECURITY.md`。
5. 提交 release 准备：
   ```sh
   git add CHANGELOG.md version.go README.md COMPATIBILITY.md docs
   git commit -m "chore(release): prepare vX.Y.Z"
   ```
6. push 分支 / 把 release PR 合并到 `main`，等待 `test`、`lint` 与 `test-os` 必需检查通过。

### 2. 本地验证 release candidate

在最新 `main`、干净工作树上：

```sh
make ci                            # fmt, build, vet, deps, test, race,
                                   # coverage floors, gate, quickstart drift,
                                   # examples, cross-compile, version
make fuzz FUZZTIME=5s              # fuzz smoke (local-only in the checklist)
make bench                         # every benchmark once (local-only)
go build ./... && go vet ./...     # redundant with make ci, explicit in release log
```

说明：

- `make version` 在这个未打 tag 的提交上打印 `SKIP`，在仍带上一 release tag 的 release-prep 提交上也一样——这是预期的。release 工作流严格执行 tag == version.go。
- 运行 `bash scripts/sync-version.sh vX.Y.Z`，在创建 tag 前证明显式 tag 会与 `version.go` 匹配。
- CI 会在 tag 本身上跑 fuzz 与 benchmark 冒烟；更长的本地运行仍是发布经理对 parser/输入相关变更的职责。

### 3. 创建并 push 签名的 annotated tag

被打 tag 的提交必须是 CI 通过的、`main` 上的 release 提交：

```sh
git switch main
git pull --ff-only origin main
git tag -s vX.Y.Z -m "ZeoKeel Rig vX.Y.Z"
git tag -v vX.Y.Z                 # must print a good signature for an allowed key
git push origin vX.Y.Z
```

- Release tag 是 **annotated 且签名**的；未签名 tag 无法通过 release 工作流对照 `.github/allowed_signers` 的 `git tag -v` 检查。
- tag 必须指向 `main` 上的提交：工作流检查它是 `origin/main` 的祖先，否则拒绝发布。
- **绝不**移动、删除或 force-push 已发布 tag（见 [docs/VERSIONING.md](VERSIONING.md#tag-不可变与撤回) 与仓库的 `v*` tag ruleset）。如果打错了提交，修复方式是新的 patch 版本。
- 只 push 这一个 tag（`git push origin vX.Y.Z`），不要 `--tags`，避免无关本地 tag 溜出去。

### 4. 观察 release 工作流

```sh
gh run list --workflow=release.yml --limit 5
gh run watch <run-id>             # or watch in the Actions tab
```

如果门禁失败，草稿 Release 不会创建。在 `main` 上向前修复并切**新的** patch 版本；绝不 force-push tag。在没有 Release 存在时重跑失败的 job 是安全的。

### 5. 审核草稿 release 与已发布产物

```sh
gh release view vX.Y.Z            # title, tag, notes, draft state
gh release view vX.Y.Z --json isDraft,isPrerelease,tagName,notesUrl
```

检查：

- release 存在、是**草稿**，且 notes 恰好是 `CHANGELOG.md` 的 `## vX.Y.Z` 小节；
- tag 已签名并指向预期提交（`git rev-parse vX.Y.Z^{commit}` 等于 release 提交）；
- smoke job 的 `go get @vX.Y.Z` + `go mod verify` 成功；
- `https://pkg.go.dev/github.com/zeokeel/zeokeel-rig@vX.Y.Z` 可渲染（proxy 可能需要几分钟；release 工作流已对 smoke 做重试）。

### 6. 发布后复审，然后发布草稿

在完成这一步前 release 仍是草稿，这是刻意的：发布才是不可逆的部分。

1. 运行发布后复审清单（发布经理为当前审计轮次维护的 release 门禁；不属于已发布 SDK）：
   - G1 版本一致性：tag ↔ commit ↔ `version.go` ↔ CHANGELOG ↔ README ↔ 草稿 notes；
   - G2 可消费性：干净环境 `go get @vX.Y.Z`、`go mod verify`、sumdb 记录、pkg.go.dev 页面；
   - G3/G4 安全与正确性修复已被各自的回归测试验证；
   - G5–G7 门禁/审批/来源：完整 CI 复用、签名 tag、SHA 固定的 actions、受限 token、干净环境 smoke；
   - G8/G9/G10 信任渠道、弃用记录与文档真话；
   - G11–G13 CI 质量、分支/tag 保护与发布历史披露。
2. 只有每个门禁都通过才发布：

   ```sh
   gh workflow run publish-release.yml -f tag=vX.Y.Z -f confirm_reviewed=yes
   gh run watch <run-id>
   ```

   `publish-release` 工作流会再次验证 tag 签名、`version.go` 匹配与草稿状态，然后用 `--latest` 发布 Release。复审未通过就发布属于发布事故。

### 7. 发布之后

- 让 `version.go` 保持已发布版本：它总是等于最新已发布 tag。不要在 `main` 上提前 bump。
- 在 `CHANGELOG.md` 中开一个新的 `## Unreleased` 小节，为下一条 line 写好标准类别标题（这是 release 提交后续工作的一部分，由复审检查）。
- 验证 `sum.golang.org/lookup/github.com/zeokeel/zeokeel-rig@vX.Y.Z` 返回已发布校验和，且 pkg.go.dev 显示新版本。
- 在项目公布 release 的渠道上公告；GitHub Release 是权威记录。
- 检查 Dependabot 的每周 GitHub Actions PR 保持绿（它们更新 SHA pin）。

## 热修复发布

1. 从 release tag 拉分支：`git switch -c hotfix/X.Y.Z+1 vX.Y.Z`。
2. 应用最小修复加回归测试；更新 `version.go`、`CHANGELOG.md`（`## vX.Y.Z+1 — YYYY-MM-DD`）及受影响的文档。
3. 运行 `make ci` 与针对性测试；向 `main` 开 PR、合并，然后按第 3 步给 merge commit 打签名 annotated tag。同一工作流创建草稿 release；同一复审与发布步骤适用。
4. 不要改写上一版本的 notes；advisory/小节属于新版本。

## 紧急回滚 / 撤回

没有任何能重写 tag 的回滚。如果已发布 release 不能被使用：

1. 尽快发布修复后的 patch release。
2. 在修复版本中向 `go.mod` 为坏版本添加 `retract` 指令（见 `docs/VERSIONING.md` 中的 Go 文档链接）。
3. 如果问题是安全漏洞，发布 GitHub Security Advisory 并遵循 [SECURITY.md](../../SECURITY.md)。
4. 只有 tag 本身必须废弃时才删除草稿 Release（绝不要删除已发布的）；tag 保持不可变，由新 patch 取代。

## 发布清单（可复制）

以下保持英文原文，避免与流程命令/标识符不一致：

```text
[ ] Scope frozen; main green; tree clean; signing key configured
[ ] CHANGELOG: ## Unreleased -> ## vX.Y.Z — YYYY-MM-DD (exactly one section,
    BREAKING markers and migration notes, Deprecated entries)
[ ] version.go const Version = "X.Y.Z"
[ ] README install tag / roadmap, COMPATIBILITY.md, COVERAGE/TOOLS counts, ARCHITECTURE/SECURITY if behavior changed
[ ] Release-prep commit merged to main; required checks green (test, lint, test-os)
[ ] make ci (all gates) on the release commit
[ ] make fuzz FUZZTIME=5s and make bench (local-only release gates)
[ ] bash scripts/sync-version.sh vX.Y.Z passes
[ ] git tag -s vX.Y.Z -m "ZeoKeel Rig vX.Y.Z" on the release commit; git tag -v passes
[ ] git push origin vX.Y.Z (nothing else; never move a published tag)
[ ] release.yml green: full CI + signed-tag verification + tag-on-main + clean-room go get/go mod verify
[ ] draft GitHub Release created with the CHANGELOG notes; gh release view verified
[ ] post-release re-audit passed (pkg.go.dev renders; sumdb record; G1–G13)
[ ] publish-release workflow run with confirm_reviewed=yes; release is no longer a draft
[ ] fresh Unreleased section in CHANGELOG for the next line
```
