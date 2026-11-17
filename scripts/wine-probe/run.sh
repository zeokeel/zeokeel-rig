#!/usr/bin/env bash
# 在 Linux 上用 Wine 跑 Windows 安全 API 探针，用于回答"Wine 能不能验证这个后端"。
#
# 结论（Wine 9.0 实测）：**不能**。见 README.md。这个脚本只用于复现负结果，
# 不作为安全验收；真正的验收在 .github/workflows/ci.yml 的 windows-sandbox job。
set -euo pipefail
cd "$(dirname "$0")"

if ! command -v wine >/dev/null 2>&1; then
  echo "wine 未安装（Debian/Ubuntu: apt-get install -y --no-install-recommends wine64 wine）" >&2
  exit 1
fi

export WINEPREFIX="${WINEPREFIX:-$(pwd)/.wineprefix}"
export WINEDEBUG="${WINEDEBUG:--all}"
export CGO_ENABLED=0 GOOS=windows GOARCH=amd64

if [ ! -d "$WINEPREFIX" ]; then
  echo "== 初始化 wineprefix: $WINEPREFIX"
  timeout 180 wineboot -i >/dev/null 2>&1 || true
fi

mkdir -p run
(cd probe && go build -o ../probe.exe .)
(cd behavior && go build -o ../behavior.exe .)

echo "== API 形状探针"
PROBE_DIR='Z:\'"$(pwd | sed 's#^/#Z:\\#; s#/#\\#g')"'\run' timeout 120 wine ./probe.exe || true

echo
echo "== 强制语义探针（关键：受限令牌是否真的限制访问）"
PROBE_DIR='Z:\'"$(pwd | sed 's#^/#Z:\\#; s#/#\\#g')"'\run' timeout 120 wine ./behavior.exe || true
