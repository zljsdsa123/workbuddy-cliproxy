#!/usr/bin/env bash
set -euo pipefail

PLUGIN_SRC="$(cd "$(dirname "$0")" && pwd)"
PLUGIN_DIR="${CLIPROXY_PLUGIN_DIR:-$HOME/.cli-proxy-api/plugins}"
CONF="${CLIPROXY_CONF:-/opt/homebrew/etc/cliproxyapi.conf}"
PLUGIN_ID="workbuddy"
# 编译架构需与 CPA 实例一致(arm64 / amd64);CGO 必须开启才能产出 c-shared 库
PLUGIN_ARCH="${PLUGIN_ARCH:-$(uname -m)}"

echo "==> Build $PLUGIN_ID (arch=$PLUGIN_ARCH)"
mkdir -p "$PLUGIN_DIR"
(
  cd "$PLUGIN_SRC"
  CGO_ENABLED=1 GOARCH="$PLUGIN_ARCH" \
    go build -buildmode=c-shared -o "$PLUGIN_DIR/${PLUGIN_ID}.dylib" .
  rm -f "$PLUGIN_DIR/${PLUGIN_ID}.h"
)
ls -la "$PLUGIN_DIR/${PLUGIN_ID}.dylib"

echo "==> Update $CONF"
python3 - "$CONF" <<'PY'
from pathlib import Path
import re
import sys

path = Path(sys.argv[1])
text = path.read_text()
block = """  configs:
    workbuddy:
      enabled: true
      priority: 100
"""
if re.search(r"(?m)^\s*workbuddy:\s*$", text):
    print("config already contains workbuddy; leave as-is")
elif re.search(r"(?m)^  configs:\s*\{\}\s*$", text):
    text2, n = re.subn(r"(?m)^  configs:\s*\{\}\s*$", block.rstrip("\n"), text, count=1)
    if n != 1:
        raise SystemExit("failed to replace configs: {}")
    path.write_text(text2)
    print("replaced empty configs: {}")
elif re.search(r"(?m)^  configs:\s*$", text):
    # insert under existing configs map if missing
    text2 = re.sub(
        r"(?m)^(  configs:\s*\n)",
        r"\1    workbuddy:\n      enabled: true\n      priority: 100\n",
        text,
        count=1,
    )
    if text2 == text:
        raise SystemExit("failed to insert under configs:")
    path.write_text(text2)
    print("inserted under configs:")
else:
    raise SystemExit("could not find plugins.configs in config; edit manually")
PY

if command -v brew >/dev/null 2>&1; then
  echo "==> Restart cliproxyapi"
  brew services restart cliproxyapi
else
  echo "brew not found; restart cliproxyapi manually"
fi

echo "==> Done"
echo "插件已安装到 $PLUGIN_DIR/${PLUGIN_ID}.dylib"
echo "到 CPA 面板登录 workbuddy(OAuth 登录页会列出所有支持 OAuth 的插件):"
echo "Open: http://127.0.0.1:8317/management.html   →  左侧菜单「OAuth 登录」→ 找到 ${PLUGIN_ID} 卡片"