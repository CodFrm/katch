#!/bin/sh
set -eu

verify_capture() {
  capture=$1
  katch_ip=$2
  [ -f "$capture" ] || {
    printf '%s\n' "missing connection record: $capture" >&2
    return 1
  }
  awk -v allowed="$katch_ip" '
    function reject(destination, line) {
      if (destination == allowed || destination ~ /^127\./ || destination == "::1") return
      print "non-katch connection attempt: " line > "/dev/stderr"
      bad = 1
    }
    /sa_family=AF_INET,/ {
      line = $0
      if (match(line, /inet_addr\("[0-9.]+"\)/)) {
        destination = substr(line, RSTART + 11, RLENGTH - 13)
        reject(destination, line)
      }
      next
    }
    /sa_family=AF_INET6,/ {
      line = $0
      if (match(line, /inet_pton\(AF_INET6, "[0-9A-Fa-f:.]+"/)) {
        destination = substr(line, RSTART + 23, RLENGTH - 24)
        reject(destination, line)
      }
      next
    }
    # Deterministic self-test records use tcpdump-style lines.
    / IP6? / && / > / {
      line = $0
      split(line, halves, / > /)
      destination = halves[2]
      sub(/:.*/, "", destination)
      sub(/\.[0-9]+$/, "", destination)
      reject(destination, line)
    }
    END { exit bad }
  ' "$capture"
}

if [ "${1:-}" = "--verify-capture" ]; then
  [ "$#" -eq 3 ] || exit 64
  verify_capture "$2" "$3"
  exit
fi

: "${KATCH_HOST:?KATCH_HOST is required}"
: "${KATCH_PORT:?KATCH_PORT is required}"
: "${KATCH_ARTIFACTS:?KATCH_ARTIFACTS is required}"

for command in iptables ip6tables iptables-save iptables-restore ip6tables-save ip6tables-restore strace runuser; do
  command -v "$command" >/dev/null 2>&1 || {
    printf '%s\n' "required isolation command unavailable: $command" >&2
    exit 69
  }
done
[ "$(id -u)" -eq 0 ] || {
  printf '%s\n' "client entrypoint requires container root for firewall setup" >&2
  exit 77
}

katch_ip=$(awk -v host="$KATCH_HOST" '
  {
    for (field = 2; field <= NF; field++) {
      if ($field == host) {
        print $1
        exit
      }
    }
  }
' /etc/hosts)
case "$katch_ip" in
  ''|*[!0-9.]*)
    printf '%s\n' "katch host must have a static IPv4 /etc/hosts entry: $KATCH_HOST" >&2
    exit 69
    ;;
esac

mkdir -p "$KATCH_ARTIFACTS"
connect_log=$KATCH_ARTIFACTS/connect.log
firewall_before_v4=$KATCH_ARTIFACTS/iptables.before
firewall_before_v6=$KATCH_ARTIFACTS/ip6tables.before
iptables-save > "$firewall_before_v4"
ip6tables-save > "$firewall_before_v6"
cleaned=0
cleanup() {
  if [ "$cleaned" -eq 0 ]; then
    iptables-restore < "$firewall_before_v4" || true
    ip6tables-restore < "$firewall_before_v6" || true
    cleaned=1
  fi
}
trap cleanup EXIT HUP INT TERM

iptables -F OUTPUT
iptables -P OUTPUT DROP
iptables -A OUTPUT -o lo -j ACCEPT
iptables -A OUTPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
iptables -A OUTPUT -p tcp -d "$katch_ip" --dport "$KATCH_PORT" -j ACCEPT
iptables -A OUTPUT -j REJECT --reject-with icmp-port-unreachable

ip6tables -F OUTPUT
ip6tables -P OUTPUT DROP
ip6tables -A OUTPUT -o lo -j ACCEPT
ip6tables -A OUTPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
ip6tables -A OUTPUT -j REJECT --reject-with icmp6-port-unreachable

status=0
strace -f -qq -e trace=connect,sendto -s 256 -o "$connect_log" \
  runuser -u client --preserve-environment -- /bin/sh -eu -c '
    /bin/sh -eu /case/setup
    /bin/sh -eu /case/run
    /bin/sh -eu /case/assert
  ' || status=$?

iptables -nvxL OUTPUT > "$KATCH_ARTIFACTS/iptables.after"
ip6tables -nvxL OUTPUT > "$KATCH_ARTIFACTS/ip6tables.after"
if ! verify_capture "$connect_log" "$katch_ip"; then
  status=70
fi
exit "$status"
