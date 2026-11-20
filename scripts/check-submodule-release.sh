#!/usr/bin/env bash
# check-submodule-release.sh — 检查"发布面自洽"：一个版本号要在四处一致。
#
#   1. version.go 的 `const Version`（裸 X.Y.Z）；
#   2. 根模块的 release tag vX.Y.Z；
#   3. session/sqlite/go.mod 里 `require <根 module> vX.Y.Z`；
#   4. 子模块的 release tag session/sqlite/vX.Y.Z。
#
# 为什么需要：session/sqlite 是独立子模块，消费者只能靠 tag 解析版本；它的
# `require` 必须指向已发布的根版本，而根版本又由根 tag 决定。三者错位时本地
# `go build` 依然能过（子模块 go.mod 里的 `replace` 指向工作树），消费者却在
# `go get` 之后拿到一个解析不出来的依赖。docs/RELEASE.md 写了步骤但没有可执行
# 检查，这个脚本就是那道检查。
#
# Usage:
#   scripts/check-submodule-release.sh                            # 严格：任一不一致都失败
#   scripts/check-submodule-release.sh --allow-missing-submodule-tag
#                                                                 # 开发自查：缺子模块 tag 只警告
#
# Exit status: 0 一致（打印 ok；宽松模式下可能带警告）；1 不一致（打印具体缺什么）；
# 2 用法错误。
set -euo pipefail

cd "$(dirname "$0")/.."

usage() {
	cat <<'EOF'
usage: scripts/check-submodule-release.sh [--allow-missing-submodule-tag]

Verify that version.go, the root release tag, session/sqlite's `require` of the
root module and the session/sqlite release tag all name the same version.

  (no args)                       fail on any mismatch
  --allow-missing-submodule-tag   only warn when session/sqlite/vX.Y.Z is missing
                                  (development self-check before tagging)
EOF
}

lenient=0
for arg in "$@"; do
	case "$arg" in
		--allow-missing-submodule-tag) lenient=1 ;;
		--help | -h) usage; exit 0 ;;
		*)
			echo "check-submodule-release: unknown argument: $arg" >&2
			usage >&2
			exit 2
			;;
	esac
done

problems=0
note_problem() {
	echo "check-submodule-release: $*" >&2
	problems=$((problems + 1))
}

# 1. 根 module 路径与 version.go 的版本号。
[[ -f go.mod ]] || {
	echo "check-submodule-release: go.mod not found (run this from the module root)" >&2
	exit 1
}
module="$(sed -nE 's/^module[[:space:]]+([^[:space:]]+).*$/\1/p' go.mod | head -n 1)"
[[ -n "$module" ]] || {
	echo "check-submodule-release: cannot read the module path from go.mod" >&2
	exit 1
}

[[ -f version.go ]] || {
	echo "check-submodule-release: version.go not found (run this from the module root)" >&2
	exit 1
}
# version.go 里的声明必须单行：这是机器可读的版本来源（与 scripts/sync-version.sh 同款）。
version="$(sed -nE 's/^const Version[[:space:]]*=[[:space:]]*"([^"]+)".*$/\1/p' version.go | head -n 1)"
[[ -n "$version" ]] || {
	echo "check-submodule-release: cannot find 'const Version = \"...\"' in version.go" >&2
	exit 1
}
if ! [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$ ]]; then
	echo "check-submodule-release: invalid version \"$version\" in version.go (want semver X.Y.Z)" >&2
	exit 1
fi

# 2. 子模块对根模块的 require。
submodule_go_mod="session/sqlite/go.mod"
[[ -f "$submodule_go_mod" ]] || {
	echo "check-submodule-release: $submodule_go_mod not found" >&2
	exit 1
}
# 同时支持 require 块里的裸行与单行 require：去掉缩进与前导 "require " 后，
# 行首必须是 `<module> `（带空格），这样 `<module>/xxx v1` 不会被误认。
require_version="$(awk -v mod="$module" '
	{
		line = $0
		sub(/^[[:space:]]+/, "", line)
		# replace 块里的行也长得像 `<module> vX.Y.Z => ../..`：先排除，
		# 否则"没有 require、只有 replace"的 go.mod 会被读成一个假版本号。
		if (line ~ /=>/ || line ~ /^replace[[:space:]]/ || line ~ /^exclude[[:space:]]/) { next }
		sub(/^require[[:space:]]+/, "", line)
		if (index(line, mod " ") == 1) {
			split(line, field, " ")
			if (field[2] ~ /^v[0-9]/) { print field[2]; exit }
		}
	}
' "$submodule_go_mod")"

if [[ -z "$require_version" ]]; then
	note_problem "missing require of $module in $submodule_go_mod (want 'require $module v$version')"
elif [[ "${require_version#v}" != "$version" ]]; then
	note_problem "version mismatch: $submodule_go_mod requires $require_version but version.go says $version"
fi

# 3. 两个 release tag。
if ! git rev-parse --git-dir >/dev/null 2>&1; then
	echo "check-submodule-release: not a git checkout: cannot list release tags" >&2
	exit 1
fi
root_tag="v$version"
submodule_tag="session/sqlite/v$version"

if [[ -z "$(git tag -l "$root_tag")" ]]; then
	note_problem "missing tag $root_tag"
fi

if [[ -z "$(git tag -l "$submodule_tag")" ]]; then
	if [[ "$lenient" -eq 1 ]]; then
		echo "check-submodule-release: WARNING: missing tag $submodule_tag (allowed by --allow-missing-submodule-tag)" >&2
	else
		note_problem "missing tag $submodule_tag"
	fi
fi

# 4. 结论。逐个缺项已经在上面打印过，这里只给退出码与一行总结。
if [[ "$problems" -gt 0 ]]; then
	echo "check-submodule-release: $problems inconsistency(ies) for v$version; see the messages above" >&2
	exit 1
fi
echo "ok"