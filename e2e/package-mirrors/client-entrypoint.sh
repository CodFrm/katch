#!/bin/sh
set -eu

KATCH_ALLOW_BLOCKED_DNS_PROBE=${KATCH_ALLOW_BLOCKED_DNS_PROBE-0}
case $KATCH_ALLOW_BLOCKED_DNS_PROBE in
  0|1) ;;
  *)
    printf '%s\n' "KATCH_ALLOW_BLOCKED_DNS_PROBE must be 0 or 1" >&2
    exit 64
    ;;
esac

verify_capture() {
  capture=$1
  katch_ip=$2
  blocked_dns_evidence=${3:-}
  [ -f "$capture" ] || {
    printf '%s\n' "missing connection record: $capture" >&2
    return 1
  }
  [ -z "$blocked_dns_evidence" ] || : > "$blocked_dns_evidence"
  if awk -v allowed="$katch_ip" -v allowed_port="${KATCH_PORT:-}" \
    -v allow_blocked_dns_probe="$KATCH_ALLOW_BLOCKED_DNS_PROBE" \
    -v blocked_dns_evidence="$blocked_dns_evidence" '
    function reject(line) {
      print "non-katch connection attempt: " line > "/dev/stderr"
      bad = 1
    }
    function trace_pid(line, pid) {
      pid = line
      sub(/^[[:space:]]*/, "", pid)
      sub(/[[:space:]].*$/, "", pid)
      if (pid !~ /^[0-9]+$/) return ""
      return pid
    }
    function preserve_blocked_dns_probe(line) {
      if (blocked_dns_evidence != "") print line >> blocked_dns_evidence
    }
    function audit(destination, port, line, pid) {
      if (destination ~ /^127\./ || destination == "::1" || destination ~ /^::ffff:127\./) return
      if (destination == allowed || destination == "::ffff:" allowed) {
        if (port == 53) {
          if (allow_blocked_dns_probe == 1 && line ~ /(^|[[:space:]])connect\(/) {
            if (line ~ /[[:space:]]=[[:space:]]0[[:space:]]*$/) {
              preserve_blocked_dns_probe(line)
              return
            }
            if (line ~ /<unfinished \.\.\.>[[:space:]]*$/) {
              pid = trace_pid(line)
              if (pid != "" && !(pid in pending_dns_probe)) {
                pending_dns_probe[pid] = line
                pending_dns_probe_count++
                return
              }
            }
          }
          reject(line)
          return
        }
        if (port == allowed_port || port == 0) return
        if (port == 65535 && line ~ /(^|[[:space:]])connect\(/ && line ~ /[[:space:]]=[[:space:]]0[[:space:]]*$/) return
      }
      reject(line)
    }
    allow_blocked_dns_probe == 1 && /<\.\.\.[[:space:]][^[:space:]]+[[:space:]]resumed>/ {
      line = $0
      pid = trace_pid(line)
      if (pid in pending_dns_probe) {
        if (line ~ /^[[:space:]]*[0-9]+[[:space:]]+<\.\.\.[[:space:]]+connect[[:space:]]+resumed>\)[[:space:]]*=[[:space:]]*0[[:space:]]*$/) {
          preserve_blocked_dns_probe(pending_dns_probe[pid])
          preserve_blocked_dns_probe(line)
        } else {
          reject(line)
        }
        delete pending_dns_probe[pid]
        pending_dns_probe_count--
      } else if (pending_dns_probe_count > 0) {
        reject(line)
      }
      next
    }
    /sa_family=AF_INET,/ {
      line = $0
      if (match(line, /inet_addr\("[0-9.]+"\)/)) {
        destination = substr(line, RSTART, RLENGTH)
        sub(/^inet_addr\("/, "", destination)
        sub(/"\)$/, "", destination)
        port = ""
        if (match(line, /sin_port=htons\([0-9]+\)/)) {
          port = substr(line, RSTART, RLENGTH)
          sub(/^sin_port=htons\(/, "", port)
          sub(/\)$/, "", port)
        }
        audit(destination, port, line)
      }
      next
    }
    /sa_family=AF_INET6,/ {
      line = $0
      if (match(line, /inet_pton\(AF_INET6, "[0-9A-Fa-f:.]+"/)) {
        destination = substr(line, RSTART, RLENGTH)
        sub(/^inet_pton\(AF_INET6, "/, "", destination)
        sub(/"$/, "", destination)
        port = ""
        if (match(line, /sin6_port=htons\([0-9]+\)/)) {
          port = substr(line, RSTART, RLENGTH)
          sub(/^sin6_port=htons\(/, "", port)
          sub(/\)$/, "", port)
        }
        audit(destination, port, line)
      }
      next
    }
    # Deterministic self-test records use tcpdump-style lines.
    / IP6? / && / > / {
      line = $0
      split(line, halves, / > /)
      endpoint = halves[2]
      sub(/: .*/, "", endpoint)
      destination = endpoint
      sub(/\.[0-9]+$/, "", destination)
      port = endpoint
      sub(/^.*\./, "", port)
      audit(destination, port, line)
    }
    END {
      for (pid in pending_dns_probe) reject(pending_dns_probe[pid])
      exit bad
    }
  ' "$capture"; then
    return 0
  fi
  [ -z "$blocked_dns_evidence" ] || rm -f "$blocked_dns_evidence"
  return 1
}

install_host_mapping() {
  hosts_file=$1
  host=$2
  ip=$3
  mapped_ip=::ffff:$ip

  if awk -v mapped="$mapped_ip" -v host="$host" '
    $1 == mapped {
      for (field = 2; field <= NF; field++) {
        if ($field ~ /^#/) break
        if ($field == host) found = 1
      }
    }
    END { exit found ? 0 : 1 }
  ' "$hosts_file"; then
    return 0
  fi

  if ! printf '%s %s\n' "$mapped_ip" "$host" | tee -a "$hosts_file" >/dev/null; then
    printf '%s\n' "failed to install IPv4-mapped hosts entry for $host" >&2
    return 73
  fi
  awk -v mapped="$mapped_ip" -v host="$host" '
    $1 == mapped {
      for (field = 2; field <= NF; field++) {
        if ($field ~ /^#/) break
        if ($field == host) found = 1
      }
    }
    END { exit found ? 0 : 1 }
  ' "$hosts_file" || {
    printf '%s\n' "failed to verify IPv4-mapped hosts entry for $host" >&2
    return 73
  }
}

install_loopback_resolver() {
  resolver_file=$1
  evidence_dir=$2
  [ -f "$resolver_file" ] && [ -r "$resolver_file" ] || {
    printf '%s\n' "resolver config is not a readable regular file: $resolver_file" >&2
    return 66
  }
  mkdir -p "$evidence_dir"
  cat "$resolver_file" > "$evidence_dir/resolv.conf.before"
  if ! printf '%s\n' 'nameserver 127.0.0.1' 'options timeout:1 attempts:1' > "$resolver_file"; then
    printf '%s\n' "failed to install loopback resolver config: $resolver_file" >&2
    return 73
  fi
  cat "$resolver_file" > "$evidence_dir/resolv.conf.after"
}

if [ "${1:-}" = "--install-loopback-resolver" ]; then
  [ "$#" -eq 3 ] || exit 64
  install_loopback_resolver "$2" "$3"
  exit
fi

if [ "${1:-}" = "--install-host-mapping" ]; then
  [ "$#" -eq 4 ] || exit 64
  install_host_mapping "$2" "$3" "$4"
  exit
fi

if [ "${1:-}" = "--verify-capture" ]; then
  [ "$#" -eq 3 ] || [ "$#" -eq 4 ] || exit 64
  verify_capture "$2" "$3" "${4:-}"
  exit
fi

: "${KATCH_HOST:?KATCH_HOST is required}"
: "${KATCH_PORT:?KATCH_PORT is required}"
: "${KATCH_ARTIFACTS:?KATCH_ARTIFACTS is required}"
: "${KATCH_CLIENT_USER:=client}"
: "${KATCH_LOOPBACK_RESOLVER:=0}"
case $KATCH_LOOPBACK_RESOLVER in
  0|1) ;;
  *)
    printf '%s\n' "KATCH_LOOPBACK_RESOLVER must be 0 or 1" >&2
    exit 64
    ;;
esac

[ "$(id -u)" -eq 0 ] || {
  printf '%s\n' "client entrypoint requires container root for trust and firewall setup" >&2
  exit 77
}

install_java_trusted_ca() {
  ca_cert=$1
  command -v keytool >/dev/null 2>&1 || return 0
  command -v cksum >/dev/null 2>&1 || {
    printf '%s\n' "cannot derive Java cacerts alias: cksum is unavailable" >&2
    return 69
  }

  ca_identity=$(cksum < "$ca_cert") || {
    printf '%s\n' "cannot derive Java cacerts alias from trusted client CA" >&2
    return 69
  }
  set -- $ca_identity
  [ "$#" -eq 2 ] || {
    printf '%s\n' "cannot derive Java cacerts alias from trusted client CA" >&2
    return 69
  }
  ca_alias=katch-test-ca-$1-$2

  if keytool -cacerts -storepass changeit -list -alias "$ca_alias" >/dev/null 2>&1; then
    return 0
  fi
  if ! keytool -cacerts -storepass changeit -noprompt -trustcacerts \
    -importcert -alias "$ca_alias" -file "$ca_cert"; then
    printf '%s\n' "failed to install trusted client CA in Java cacerts" >&2
    return 69
  fi
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
  elif command -v update-ca-trust >/dev/null 2>&1; then
    install -m 0644 "$ca_cert" /etc/pki/ca-trust/source/anchors/katch-test-ca.crt
    update-ca-trust extract
  else
    printf '%s\n' "no supported system CA trust mechanism is available" >&2
    return 69
  fi

  install_java_trusted_ca "$ca_cert"
}

install_trusted_ca

for command in awk iptables ip6tables iptables-save iptables-restore ip6tables-save ip6tables-restore strace runuser tee; do
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
install_host_mapping /etc/hosts "$KATCH_HOST" "$katch_ip"

mkdir -p "$KATCH_ARTIFACTS"
if [ "$KATCH_LOOPBACK_RESOLVER" -eq 1 ]; then
  cat /etc/hosts > "$KATCH_ARTIFACTS/hosts"
  install_loopback_resolver /etc/resolv.conf "$KATCH_ARTIFACTS"
fi
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
blocked_dns_evidence=
if [ "$KATCH_ALLOW_BLOCKED_DNS_PROBE" -eq 1 ]; then
  blocked_dns_evidence=$KATCH_ARTIFACTS/blocked-dns-probes.log
fi
if ! verify_capture "$connect_log" "$katch_ip" "$blocked_dns_evidence"; then
  status=70
fi
exit "$status"
