#!/bin/bash
# ============================================================
# update_etf_names.sh
# 定期从腾讯/东财 API 拉取最新 ETF 名称，更新 rotation.go 中的 Strategy3Pool。
#
# 用法:
#   bash scripts/update_etf_names.sh          # 仅对比差异
#   bash scripts/update_etf_names.sh --apply  # 对比并应用修改
#
# 建议 via cron（每月1号凌晨）:
#   0 4 1 * * cd /path/to/project && bash scripts/update_etf_names.sh --apply
# ============================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
ROTATION_FILE="$PROJECT_DIR/agent/rotation.go"
PY_SCRIPT="/tmp/_update_etf_names.py"

APPLY=false
if [ "${1:-}" = "--apply" ]; then
    APPLY=true
fi

echo "[$(date '+%Y-%m-%d %H:%M:%S')] Fetching latest ETF names..."
echo ""

# ---------- Python: fetch names + diff + optional apply ----------
cat > "$PY_SCRIPT" << 'PYEOF'
import re, subprocess, sys, os

apply_mode = os.environ.get("APPLY", "0") == "1"

# 1) Fetch all codes from rotation.go
codes = []
with open(os.environ["ROTATION_FILE"]) as f:
    for m in re.finditer(r'\{"(\d{6})",\s*"([^"]+)",\s*"([^"]+)"\}', f.read()):
        codes.append(m.group(1))

if not codes:
    print("ERROR: no ETF codes found in rotation.go")
    sys.exit(1)

# 2) Batch fetch via Tencent qt.gtimg.cn
import urllib.request, time
new_names = {}
SZ_PREFIX = {"159", "16"}
BATCH = 50
for b in range(0, len(codes), BATCH):
    batch = codes[b:b+BATCH]
    symbols = [f"sz{c}" if c[:3] in SZ_PREFIX or c[:2] in SZ_PREFIX else f"sh{c}" for c in batch]
    url = "https://qt.gtimg.cn/q=" + ",".join(symbols)
    try:
        req = urllib.request.Request(url)
        req.add_header("User-Agent", "Mozilla/5.0")
        with urllib.request.urlopen(req, timeout=15) as resp:
            text = resp.read().decode("gbk", errors="ignore")
        for line in text.strip().split("\n"):
            m2 = re.search(r'v_s[hz](\d+)="\d+~([^~]+)', line)
            if m2:
                new_names[m2.group(1)] = m2.group(2)
    except Exception as e:
        print(f"[warn] batch query failed: {e}", file=sys.stderr)
    time.sleep(0.5)

print(f"Fetched {len(new_names)}/{len(codes)} ETF names")

# 3) Diff
with open(os.environ["ROTATION_FILE"]) as f:
    content = f.read()

changed = []
for m in re.finditer(r'\{"(\d{6})",\s*"([^"]+)",\s*"([^"]+)"\}', content):
    code, old_name, sector = m.group(1), m.group(2), m.group(3)
    new_name = new_names.get(code, "")
    if new_name and new_name != old_name:
        changed.append((code, old_name, new_name, sector))

if not changed:
    print("All ETF names up to date.")
    sys.exit(0)

print(f"\n{len(changed)} ETF name(s) changed:")
for code, old, new, sec in changed:
    print(f"  {code}  {old:30s} -> {new}")

# 4) Apply
if apply_mode:
    def repl(m):
        code = m.group(1)
        sector = m.group(3)
        new_name = new_names.get(code, m.group(2))
        return f'{{"{code}", "{new_name}", "{sector}"}}'
    updated = re.sub(r'\{"(\d{6})",\s*"([^"]+)",\s*"([^"]+)"\}', repl, content)
    with open(os.environ["ROTATION_FILE"], "w") as f:
        f.write(updated)
    print("\nApplied. Running go build to verify...")
    result = subprocess.run(["go", "build", "./..."], cwd=os.environ["PROJECT_DIR"], capture_output=True, text=True)
    if result.returncode == 0:
        print("Build OK.")
    else:
        print("Build FAILED:\n" + result.stderr)
        sys.exit(1)
else:
    print("\nRun with --apply to apply changes.")
PYEOF

export ROTATION_FILE="$ROTATION_FILE"
export PROJECT_DIR="$PROJECT_DIR"
export APPLY="$APPLY"

python3 "$PY_SCRIPT"
EXIT_CODE=$?

# Cleanup
rm -f "$PY_SCRIPT"

if [ "$APPLY" = "true" ] && [ $EXIT_CODE -eq 0 ]; then
    echo ""
    echo "=== Done. Remember to commit the changes. ==="
fi

exit $EXIT_CODE
