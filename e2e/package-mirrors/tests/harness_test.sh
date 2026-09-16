#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../../.." && pwd)
HARNESS=$ROOT/e2e/package-mirrors/harness.sh
ENTRYPOINT=$ROOT/e2e/package-mirrors/client-entrypoint.sh
CASES=$ROOT/e2e/package-mirrors/cases
workdir=$(mktemp -d "${TMPDIR:-/tmp}/katch-harness-test.XXXXXX")
trap 'rm -rf "$workdir"' EXIT HUP INT TERM

fail() {
  printf '%s\n' "FAIL: $*" >&2
  exit 1
}

"$HARNESS" --check "$CASES/npm.yaml" "$CASES/registry-git-regression.yaml"

cat > "$workdir/allowed.log" <<'EOF'
123 connect(3, {sa_family=AF_INET, sin_port=htons(8080), sin_addr=inet_addr("172.18.0.2")}, 16) = 0
EOF
"$ENTRYPOINT" --verify-capture "$workdir/allowed.log" 172.18.0.2

cat > "$workdir/dns-leak.log" <<'EOF'
124 sendto(3, "query", 5, MSG_NOSIGNAL, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr("8.8.8.8")}, 16) = -1 EACCES (Permission denied)
EOF
if "$ENTRYPOINT" --verify-capture "$workdir/dns-leak.log" 172.18.0.2 >"$workdir/out" 2>&1; then
  fail "external DNS attempt was accepted"
fi
grep -q '8.8.8.8' "$workdir/out" || fail "external DNS attempt was not reported"

cat > "$workdir/connect-leak.log" <<'EOF'
125 connect(3, {sa_family=AF_INET, sin_port=htons(443), sin_addr=inet_addr("104.16.30.34")}, 16) = -1 ECONNREFUSED (Connection refused)
EOF
if "$ENTRYPOINT" --verify-capture "$workdir/connect-leak.log" 172.18.0.2 >"$workdir/out" 2>&1; then
  fail "external connect attempt was accepted"
fi
grep -q '104.16.30.34' "$workdir/out" || fail "external connect attempt was not reported"

bad_case=$workdir/bad.yaml
printf '%s\n' '{"name":"bad; touch /tmp/injected","image":"busybox:latest","setup":"true","run":"true","assert":"true","required_upstreams":[]}' > "$bad_case"
if "$HARNESS" --check "$bad_case" >"$workdir/out" 2>&1; then
  fail "unpinned image and unsafe case name were accepted"
fi

printf '%s\n' "harness self-tests passed"
