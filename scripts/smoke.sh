#!/usr/bin/env bash
# 启动真实二进制并验证最基本的对外行为。
#
# 存在的理由：这个项目初始化时踩到的三个缺陷——配置文件被框架回写、迁移列表为空
# 导致启动 panic、metric 组件重复注册让 /metrics 返回 500——lint 和单元测试全都
# 发现不了，它们只在进程真正跑起来时才暴露。
set -euo pipefail

BIN=${BIN:-./bin/katch}
PORT=${PORT:-18080}
BASE="http://127.0.0.1:${PORT}"

[ -x "$BIN" ] || { echo "找不到可执行文件 $BIN，先跑 make build"; exit 1; }

workdir=$(mktemp -d)
cleanup() {
  [ -n "${PID:-}" ] && kill "$PID" 2>/dev/null || true
  rm -rf "$workdir"
}
trap cleanup EXIT

# 用临时配置副本跑，同时保留一份原始配置用于比对是否被改写
sed "s#0.0.0.0:8080#0.0.0.0:${PORT}#; s#./data/katch.db#${workdir}/katch.db#; s#./runtime/logs#${workdir}/logs#g" \
  configs/config.yaml > "$workdir/config.yaml"
cp "$workdir/config.yaml" "$workdir/config.expect"

"$BIN" -config "$workdir/config.yaml" > "$workdir/out.log" 2>&1 &
PID=$!

fail() { echo "✗ $1"; echo "--- 服务日志 ---"; tail -20 "$workdir/out.log"; exit 1; }

check() { # check <路径> <期望状态码> <说明>
  local got
  got=$(curl -s -o /dev/null -w '%{http_code}' --retry 30 --retry-delay 1 --retry-connrefused "${BASE}$1")
  [ "$got" = "$2" ] || fail "$3：期望 $2，实际 $got"
  echo "✓ $3"
}

check /api/v1/system/version 200 "版本接口可用"
curl -s "${BASE}/api/v1/system/version" | grep -q '"version"' || fail "版本接口返回体缺少 version 字段"
echo "✓ 版本接口返回体包含 version"

check /metrics 200 "Prometheus 指标端点可用"
curl -s "${BASE}/metrics" | grep -q "^go_" || fail "/metrics 没有输出任何指标"
echo "✓ /metrics 输出了指标"

check / 200 "SPA 首页可用"
check /api/v1/does-not-exist 404 "未命中的 API 返回 404 而不是回落 index.html"

diff -q "$workdir/config.expect" "$workdir/config.yaml" > /dev/null \
  || fail "配置文件被进程改写了（只读配置源可能失效）"
echo "✓ 配置文件未被改写"

echo "smoke 全部通过"
