#!/usr/bin/env bash
# 產生 SaaSForge CMS 使用手冊 PDF。
# 用 headless Chrome 直接列印,不需要額外安裝任何工具鏈。
set -euo pipefail
cd "$(dirname "$0")"

OUT_HTML=handbook.html
OUT_PDF=SaaSForge-CMS-使用手冊.pdf

{
  echo '<!doctype html><html lang="zh-Hant"><head><meta charset="utf-8">'
  echo '<title>SaaSForge CMS 使用手冊</title><style>'
  cat style.css
  echo '</style></head><body>'
  for f in 00-cover.html 01-intro.html 02-start.html 03-concepts.html \
           04-capabilities.html 05-content.html 06-schema.html 07-agents.html \
           08-api.html 09-trouble.html; do
    cat "$f"; echo
  done
  echo '</body></html>'
} > "$OUT_HTML"

# 依序找一個能用的 Chrome。公開讀者拿到的是各章 .html 與這支腳本(PDF 不進版控),
# 所以這裡是他們產出成品的唯一途徑——不要寫死 macOS 路徑。
CHROME="${CHROME:-}"
if [ -z "$CHROME" ]; then
  for c in "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
           "/Applications/Chromium.app/Contents/MacOS/Chromium" \
           google-chrome google-chrome-stable chromium chromium-browser; do
    if [ -x "$c" ]; then CHROME="$c"; break; fi
    if command -v "$c" >/dev/null 2>&1; then CHROME=$(command -v "$c"); break; fi
  done
fi
[ -n "$CHROME" ] || { echo "✗ 找不到 Chrome/Chromium。裝一個,或設 CHROME=/path/to/chrome" >&2; exit 1; }
"$CHROME" --headless=new --disable-gpu --no-pdf-header-footer \
  --print-to-pdf="$OUT_PDF" "$OUT_HTML" 2>/dev/null

echo "→ $(pwd)/$OUT_PDF"
