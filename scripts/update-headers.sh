#!/usr/bin/env bash
# 拉取 Cline 官方最新客户端版本，热更新网关请求头（并落盘，重启不丢）。
#
# 版本来源（官方仓库 cline/cline@main）：
#   apps/vscode/package.json        -> User-Agent / X-CLIENT-VERSION / X-PLATFORM-VERSION
#   sdk/packages/core/package.json  -> X-CORE-VERSION
# 头名/默认值定义：sdk/packages/llms/src/providers/request-headers.ts
#
# 用法: update-headers.sh [BASE_URL]   默认 http://127.0.0.1:3457
# 退出码: 0 成功(含已是最新) / 非 0 拉取或更新失败
set -euo pipefail

BASE="${1:-http://127.0.0.1:3457}"
RAW="https://raw.githubusercontent.com/cline/cline/main"

ver() { curl -fsS --max-time 25 "$RAW/$1" | python3 -c 'import sys,json;print(json.load(sys.stdin)["version"])'; }

EXT="$(ver apps/vscode/package.json)"
CORE="$(ver sdk/packages/core/package.json)"
if [ -z "$EXT" ] || [ -z "$CORE" ]; then
  echo "[$(date '+%F %T')] ERROR: 拉取官方版本失败 (ext=$EXT core=$CORE)" >&2
  exit 1
fi

# 读当前生效值，仅当变化时才写盘
CUR_JSON="$(curl -fsS --max-time 10 "$BASE/admin/api/config" 2>/dev/null || echo '{}')"
CUR_EXT="$(printf '%s' "$CUR_JSON"  | python3 -c 'import sys,json;print(json.load(sys.stdin).get("data",{}).get("headers",{}).get("X-CLIENT-VERSION",""))' 2>/dev/null || true)"
CUR_CORE="$(printf '%s' "$CUR_JSON" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("data",{}).get("headers",{}).get("X-CORE-VERSION",""))' 2>/dev/null || true)"
if [ "$CUR_EXT" = "$EXT" ] && [ "$CUR_CORE" = "$CORE" ]; then
  echo "[$(date '+%F %T')] up to date: ext=$EXT core=$CORE"
  exit 0
fi

PAYLOAD="$(EXT="$EXT" CORE="$CORE" python3 - <<'PY'
import json, os
ext, core = os.environ["EXT"], os.environ["CORE"]
print(json.dumps({"headers": {
    "User-Agent":         f"Cline/{ext}",
    "HTTP-Referer":       "https://cline.bot",
    "X-Title":            "Cline",
    "X-IS-MULTIROOT":     "false",
    "X-CLIENT-TYPE":      "cline-cli",
    "X-CLIENT-VERSION":   ext,
    "X-PLATFORM":         "terminal",
    "X-PLATFORM-VERSION": ext,
    "X-CORE-VERSION":     core,
}}))
PY
)"

curl -fsS --max-time 25 -X POST "$BASE/admin/api/config/update" \
  -H 'Content-Type: application/json' -d "$PAYLOAD" >/dev/null
echo "[$(date '+%F %T')] updated: ext=$CUR_EXT->$EXT core=$CUR_CORE->$CORE"
