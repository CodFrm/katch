#!/bin/sh
set -eu

verify_capture() {
  capture=$1
  katch_ip=$2
  [ -f "$capture" ] || {
    printf '%s\n' "missing connection record: $capture" >&2
    return 1
  }
  awk -v allowed="$katch_ip" -v allowed_port="${KATCH_PORT:-}" '
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
        destination = substr(line, RSTART + 21, RLENGTH - 22)
        if (destination == "::ffff:" allowed && match(line, /sin6_port=htons\([0-9]+\)/)) {
          port = substr(line, RSTART + 16, RLENGTH - 17)
          if (port == allowed_port) next
        }
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
: "${KATCH_CLIENT_USER:=client}"

[ "$(id -u)" -eq 0 ] || {
  printf '%s\n' "client entrypoint requires container root for trust and firewall setup" >&2
  exit 77
}

install_trusted_ca() {
  ca_cert=${KATCH_CLIENT_CA_CERT:-}
  [ -n "$ca_cert" ] || return 0
  if [ ! -f "$ca_cert" ] || [ ! -r "$ca_cert" ]; then
    printf '%s\n' "trusted client CA is not a readable regular file: $ca_cert" >&2
    return 66
  fi

  if command -v update-ca-certificates >/dev/null 2>&1; then
    install -m 0644 "$ca_cert" /usr/local/share/ca-certificates/katch-test-ca.crt
    update-ca-certificates
    return
  fi
  if command -v update-ca-trust >/dev/null 2>&1; then
    install -m 0644 "$ca_cert" /etc/pki/ca-trust/source/anchors/katch-test-ca.crt
    update-ca-trust extract
    return
  fi

  printf '%s\n' "no supported system CA trust mechanism is available" >&2
  return 69
}

install_trusted_ca

for command in iptables ip6tables iptables-save iptables-restore ip6tables-save ip6tables-restore strace runuser; do
  command -v "$command" >/dev/null 2>&1 || {
    printf '%s\n' "required isolation command unavailable: $command" >&2
    exit 69
  }
done

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

client_home=$(awk -F: -v user="$KATCH_CLIENT_USER" '
  $1 == user {
    print $6
    exit
  }
' /etc/passwd)
[ -n "$client_home" ] || {
  printf '%s\n' "runtime user has no passwd entry: $KATCH_CLIENT_USER" >&2
  exit 69
}

status=0
strace -f -qq -e trace=connect,sendto -s 256 -o "$connect_log" \
  runuser -u "$KATCH_CLIENT_USER" --preserve-environment -- \
  env HOME="$client_home" USER="$KATCH_CLIENT_USER" LOGNAME="$KATCH_CLIENT_USER" \
  /bin/sh -eu -c '
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
