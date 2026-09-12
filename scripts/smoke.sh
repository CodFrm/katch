#!/usr/bin/env bash
# 启动真实二进制并验证最基本的对外行为。
#
# 存在的理由：这个项目初始化时踩到的三个缺陷——配置文件被框架回写、迁移列表为空
# 导致启动 panic、metric 组件重复注册让 /metrics 返回 500——lint 和单元测试全都
# 发现不了，它们只在进程真正跑起来时才暴露。
#
# 最后一段还守着 spec 的「扩展点」：一个此前不存在的上游，只经管理接口加一条记录
# 就要能拉通并在第二次命中缓存，全程不改代码、不重启进程。用例那一侧
# （internal/proxy/extension）用的是拉取处理器的替身，这里用的是 make build 出来的
# 真二进制——生产的 NoRoute、真配置、真 sqlite 只有在这里才走得到。
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
sed "s#0.0.0.0:8080#0.0.0.0:${PORT}#; s#./data/katch.db#${workdir}/katch.db#; \
     s#./data/cache#${workdir}/cache#; s#./runtime/logs#${workdir}/logs#g" \
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

# 管理接口：密钥的两种失败必须不可区分，带对密钥则能写进去再读回来。
# 单元测试用的是 mock 仓储，这里走的是真二进制 + 真 sqlite——迁移建的表对不对、
# 初始密钥有没有落库，只有在这里才会暴露。
ADMIN_KEY=$(awk '/^admin:/{f=1;next} f && /initialKey:/{sub(/^[^:]*:[[:space:]]*/,""); gsub(/"/,""); print; exit}' "$workdir/config.yaml")
[ -n "$ADMIN_KEY" ] || fail "没能从配置里读出 admin.initialKey"

check /api/v1/admin/upstreams 401 "管理接口未带密钥返回 401"

nokey=$(curl -s "${BASE}/api/v1/admin/upstreams")
wrongkey=$(curl -s -H "Authorization: Bearer definitely-not-the-key" "${BASE}/api/v1/admin/upstreams")
[ "$nokey" = "$wrongkey" ] || fail "未带密钥与密钥错误的响应体不同，可被用来确认密钥字段名"
echo "✓ 未带密钥与密钥错误的响应完全一致"

curl -s -X POST "${BASE}/api/v1/admin/upstreams" \
  -H "Authorization: Bearer ${ADMIN_KEY}" -H 'Content-Type: application/json' \
  -d '{"host":"smoke.example.com","kind":"static","origin":"https://smoke.example.com","enabled":true}' \
  | grep -q '"code":0' || fail "带正确密钥创建上游失败"
echo "✓ 带正确密钥可以创建上游"

curl -s -H "Authorization: Bearer ${ADMIN_KEY}" "${BASE}/api/v1/admin/upstreams" \
  | grep -q 'smoke.example.com' || fail "刚创建的上游没能被列表读回"
echo "✓ 创建的上游能被列表读回"

# 扩展点：只经管理接口加一条记录，就能拉通一个此前不存在的上游并命中缓存。
#
# 假上游的源站就是这台 katch 自己的版本接口。smoke 不该为了造一个假源站引入
# python -m http.server 之类的新依赖，而这条边界要验的是「只加一条记录就够了」，
# 源站是谁与它无关——回源走的是记录上的 origin 字段，和打到公网别无二致。
FAKE_HOST="packages.smoke-new-mirror.invalid"
FAKE_PULL="${BASE}/${FAKE_HOST}/version"

# 注册之前它必须什么都不是：这既是对照组，也把「表里没有这个主机」塞进了上游那层
# 进程内快照——后面那次拉取要成立，写入就必须把快照掀掉，这正是「不重启」的兑现点。
check "/${FAKE_HOST}/version" 404 "未注册的上游拉取返回 404"

curl -s -X POST "${BASE}/api/v1/admin/upstreams" \
  -H "Authorization: Bearer ${ADMIN_KEY}" -H 'Content-Type: application/json' \
  -d "{\"host\":\"${FAKE_HOST}\",\"kind\":\"static\",\"origin\":\"${BASE}/api/v1/system\",\
       \"enabled\":true,\"immutable_patterns\":[\"/version\"]}" \
  | grep -q '"code":0' || fail "经管理接口注册假上游失败"
echo "✓ 经管理接口注册了一个此前不存在的上游"

pull() { # pull <名字>：状态码写进 $status，响应头进 <名字>.h，响应体进 <名字>.b
  status=$(curl -s -D "$workdir/$1.h" -o "$workdir/$1.b" -w '%{http_code}' "$FAKE_PULL")
}

# 拉取之前先确认进程还是启动时那一个，后面的「不重启」才说得出口。
kill -0 "$PID" 2>/dev/null || fail "服务进程已经不在了，后面的拉取说明不了「不重启」"

pull first
[ "$status" = "200" ] || fail "经新上游的第一次拉取：期望 200，实际 $status"
grep -q '"version"' "$workdir/first.b" || fail "第一次拉取没有把源站的内容带回来"
grep -qi '^x-katch-cache: MISS' "$workdir/first.h" || fail "第一次拉取应当是 MISS"
echo "✓ 新上游第一次拉取回源成功（MISS）"

pull second
[ "$status" = "200" ] || fail "经新上游的第二次拉取：期望 200，实际 $status"
grep -qi '^x-katch-cache: HIT' "$workdir/second.h" || fail "第二次拉取没有命中缓存"
# 命中不能是一份残缺副本：磁盘上那一份要和回源拿到的逐字节相同。
cmp -s "$workdir/first.b" "$workdir/second.b" || fail "缓存命中的内容和回源拿到的不一致"
echo "✓ 新上游第二次拉取由缓存服务（HIT），内容与回源逐字节相同"

kill -0 "$PID" 2>/dev/null || fail "拉取过程中服务进程重启过"
echo "✓ 从注册到命中全程是同一个进程，没有重启"

diff -q "$workdir/config.expect" "$workdir/config.yaml" > /dev/null \
  || fail "配置文件被进程改写了（只读配置源可能失效）"
echo "✓ 配置文件未被改写"

echo "smoke 全部通过"
