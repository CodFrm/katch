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

fake_bin=$workdir/fake-bin
mkdir -p "$fake_bin"
cat > "$fake_bin/fake-command" <<'EOF'
#!/bin/sh
set -eu
case ${0##*/} in
  awk)
    last=
    for argument do
      last=$argument
    done
    if [ "$last" = /etc/hosts ]; then
      printf '%s\n' 172.18.0.2
    else
      exec /usr/bin/awk "$@"
    fi
    ;;
  id)
    [ "${1:-}" = "-u" ] || exit 64
    printf '%s\n' 0
    ;;
  iptables-save|ip6tables-save)
    printf '%s\n' '*filter' COMMIT
    ;;
  iptables-restore|ip6tables-restore|iptables|ip6tables)
    ;;
  strace)
    capture=
    while [ "$#" -gt 0 ]; do
      case $1 in
        -f|-qq) shift ;;
        -e|-s) shift 2 ;;
        -o) capture=$2; shift 2 ;;
        *) break ;;
      esac
    done
    [ -n "$capture" ] || exit 64
    : > "$capture"
    "$@"
    ;;
  runuser)
    printf '%s\n' "$*" >> "$RUNUSER_LOG"
    ;;
  *) exit 64 ;;
esac
EOF
chmod 0555 "$fake_bin/fake-command"
for command in awk id iptables ip6tables iptables-save iptables-restore ip6tables-save ip6tables-restore strace runuser; do
  ln -s fake-command "$fake_bin/$command"
done

run_entrypoint() {
  artifacts=$1
  shift
  env PATH="$fake_bin:$PATH" RUNUSER_LOG="$workdir/runuser.log" \
    KATCH_HOST=katch.invalid KATCH_PORT=8080 KATCH_ARTIFACTS="$artifacts" \
    "$@" "$ENTRYPOINT"
}

: > "$workdir/runuser.log"
run_entrypoint "$workdir/default-artifacts" env -u KATCH_CLIENT_USER
grep -q '^-u client --preserve-environment -- /bin/sh -eu -c ' "$workdir/runuser.log" ||
  fail "entrypoint did not retain the default client user"

: > "$workdir/runuser.log"
run_entrypoint "$workdir/homebrew-artifacts" env KATCH_CLIENT_USER=linuxbrew
grep -q '^-u linuxbrew --preserve-environment -- /bin/sh -eu -c ' "$workdir/runuser.log" ||
  fail "entrypoint did not switch to the requested client user"

bad_case=$workdir/bad.yaml
printf '%s\n' '{"name":"bad; touch /tmp/injected","image":"busybox:latest","setup":"true","run":"true","assert":"true","required_upstreams":[]}' > "$bad_case"
if "$HARNESS" --check "$bad_case" >"$workdir/out" 2>&1; then
  fail "unpinned image and unsafe case name were accepted"
fi

printf '%s\n' "harness self-tests passed"
