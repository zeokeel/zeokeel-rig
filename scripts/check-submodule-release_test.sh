#!/usr/bin/env bash
# check-submodule-release_test.sh — 自测 scripts/check-submodule-release.sh。
#
# 为什么要有它：发布面自洽（`version.go` / 根 tag / `session/sqlite` 的 `require` /
# 子模块 tag 四者对齐）不进 CI，只靠发版人自查。自查脚本自己错了比没有检查更糟：
# 它会给出一个假的 "ok"。所以这里在临时 git 仓库里造出四种局面，逐一钉住退出码与
# 报错文案。
#
#   Usage: bash scripts/check-submodule-release_test.sh
#   Exit:  0 全部通过（最后打印 ok）；1 有断言失败
set -uo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
checker="$script_dir/check-submodule-release.sh"
if [[ ! -f "$checker" ]]; then
	echo "FAIL: $checker 不存在——没有可测的检查脚本" >&2
	exit 1
fi

# 隔离 git 的全局/系统配置：测试只依赖仓库自身的 tag。
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_SYSTEM=/dev/null
export GIT_TERMINAL_PROMPT=0

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

case_name=""
status=0
out=""

fail() {
	echo "FAIL [${case_name}]: $*" >&2
	if [[ -n "$out" ]]; then
		echo "--- 脚本输出 ---" >&2
		echo "$out" >&2
		echo "----------------" >&2
	fi
	exit 1
}

# new_repo <目录> <session/sqlite/go.mod 里 require 的版本（带 v 前缀）>
#
# 造一个最小的多 module 仓库：根 module 的 go.mod + version.go（0.25.0）+
# session/sqlite/go.mod。脚本按真实仓库的路径与形状读取它们。
new_repo() {
	local dir="$1" require="$2"
	mkdir -p "$dir/scripts" "$dir/session/sqlite"
	cp "$checker" "$dir/scripts/check-submodule-release.sh"
	cat >"$dir/go.mod" <<'EOF'
module example.com/rig

go 1.27
EOF
	cat >"$dir/version.go" <<'EOF'
package rig

// Version 是版本号；必须与 release tag 一致。
const Version = "0.25.0"
EOF
	cat >"$dir/session/sqlite/go.mod" <<EOF
module example.com/rig/session/sqlite

go 1.27

require example.com/rig $require
EOF
	git -C "$dir" init -q -b main
	git -C "$dir" -c user.name=rig-test -c user.email=rig-test@example.com \
		commit -q --allow-empty -m "init"
}

# run_checker <目录> [参数...] —— 运行被测脚本，结果进 $status / $out。
run_checker() {
	local dir="$1"
	shift
	out="$(cd "$dir" && bash scripts/check-submodule-release.sh "$@" 2>&1)"
	status=$?
}

expect_status() {
	if [[ "$status" -ne "$1" ]]; then
		fail "退出码 $status，want $1"
	fi
}

expect_contains() {
	case "$out" in
		*"$1"*) ;;
		*) fail "输出里没有「$1」" ;;
	esac
}

# 1. 全齐：根 tag、子模块 tag、require、version.go 四者一致 → ok / 0。
case_name="all-present"
repo="$work/all"
new_repo "$repo" v0.25.0
git -C "$repo" tag v0.25.0
git -C "$repo" tag session/sqlite/v0.25.0
run_checker "$repo"
expect_status 0
expect_contains "ok"

# 2. 缺子模块 tag → 报缺什么、exit 1；宽松开关只警告不失败。
case_name="missing-submodule-tag"
repo="$work/missing-sub"
new_repo "$repo" v0.25.0
git -C "$repo" tag v0.25.0
run_checker "$repo"
expect_status 1
expect_contains "missing tag session/sqlite/v0.25.0"

case_name="missing-submodule-tag-lenient"
run_checker "$repo" --allow-missing-submodule-tag
expect_status 0
expect_contains "ok"
expect_contains "WARNING"

# 3. require 与 version.go 不一致 → exit 1（即使两个 tag 都在）。
case_name="version-mismatch"
repo="$work/mismatch"
new_repo "$repo" v0.24.0
git -C "$repo" tag v0.25.0
git -C "$repo" tag session/sqlite/v0.25.0
run_checker "$repo"
expect_status 1
expect_contains "version mismatch"

# 4. 缺根 tag → exit 1；宽松开关不掩盖它（它只放宽子模块 tag）。
case_name="missing-root-tag"
repo="$work/missing-root"
new_repo "$repo" v0.25.0
git -C "$repo" tag session/sqlite/v0.25.0
run_checker "$repo"
expect_status 1
expect_contains "missing tag v0.25.0"

case_name="missing-root-tag-lenient"
run_checker "$repo" --allow-missing-submodule-tag
expect_status 1

echo "ok"