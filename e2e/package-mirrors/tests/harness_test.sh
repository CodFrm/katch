#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../../.." && pwd)
HARNESS=$ROOT/e2e/package-mirrors/harness.sh
ENTRYPOINT=$ROOT/e2e/package-mirrors/client-entrypoint.sh
IMAGE_SMOKE=$ROOT/e2e/package-mirrors/images/client-smoke.sh
IMAGE_BUILD=$ROOT/e2e/package-mirrors/images/build.sh
IMAGE_CONTRACT=$ROOT/e2e/package-mirrors/images/contract.json
CASES=$ROOT/e2e/package-mirrors/cases
workdir=$(mktemp -d "${TMPDIR:-/tmp}/katch-harness-test.XXXXXX")
trap 'rm -rf "$workdir"' EXIT HUP INT TERM

fail() {
  printf '%s\n' "FAIL: $*" >&2
  exit 1
}

"$HARNESS" --check "$CASES"/*.yaml

for script in "$HARNESS" "$ENTRYPOINT" "$IMAGE_SMOKE" "$IMAGE_BUILD"; do
  sh -n "$script" || fail "invalid shell syntax: $script"
done

smoke_bin=$workdir/smoke-bin
mkdir -p "$smoke_bin"
cat > "$smoke_bin/fake-client" <<'EOF'
#!/bin/sh
set -eu
case ${0##*/} in
  java) printf '%s\n' 'openjdk version "21.0.8" 2025-07-15 LTS' >&2 ;;
  mvn) printf '%s\n' 'Apache Maven 3.9.11' ;;
  gradle) printf '%s\n' 'Gradle 9.0.0' ;;
  sbt) printf '%b\n' "${SBT_VERSION_OUTPUT:?SBT_VERSION_OUTPUT is required}" ;;
  *) exit 64 ;;
esac
EOF
chmod 0555 "$smoke_bin/fake-client"
for client in java mvn gradle sbt; do
  ln -s fake-client "$smoke_bin/$client"
done

SBT_VERSION_OUTPUT=' \t1.11.6 \t' KATCH_CLIENT_FLAVOR=jvm PATH="$smoke_bin:$PATH" \
  "$IMAGE_SMOKE" >"$workdir/smoke.out" 2>"$workdir/smoke.err" ||
  fail "client smoke rejected an exact sbt version surrounded by whitespace: $(cat "$workdir/smoke.err")"
grep -Fx 'sbt=1.11.6' "$workdir/smoke.out" >/dev/null ||
  fail "client smoke did not normalize surrounding sbt version whitespace"

if SBT_VERSION_OUTPUT=prefix1.11.6suffix KATCH_CLIENT_FLAVOR=jvm PATH="$smoke_bin:$PATH" \
  "$IMAGE_SMOKE" >"$workdir/smoke.out" 2>"$workdir/smoke.err"; then
  fail "client smoke accepted a non-exact sbt version"
fi
grep -F 'sbt version mismatch: expected 1.11.6, got prefix1.11.6suffix' "$workdir/smoke.err" >/dev/null ||
  fail "client smoke did not report the non-exact sbt version"

jq -e '
  type == "array" and length == 12 and
  all(.[];
    (keys | sort) == (["cases", "dockerfile", "image", "smoke", "target", "user"] | sort) and
    (.image | test("^katch/package-client-[a-z0-9-]+:[A-Za-z0-9._-]+$")) and
    (.dockerfile | test("^[a-z0-9-]+\\.Dockerfile$")) and
    (.target | test("^[a-z0-9-]+$")) and
    (.user == "client" or .user == "linuxbrew" or .user == "root") and
    (.cases | type == "array" and length > 0 and all(.[]; type == "string" and length > 0)) and
    (.smoke | type == "object" and length > 0 and all(to_entries[]; (.key | test("^[a-z0-9.+-]+$")) and (.value | test("^[0-9]+([.][0-9]+)+([+-][A-Za-z0-9.-]+)?$"))))
  ) and
  ([.[].image] | unique | length) == length and
  ([.[].target] | unique | length) == length and
  ([.[].cases[]] | unique | length) == ([.[].cases[]] | length) and
  ([.[] | select(.user == "root") | .cases[]] | sort) == (["apk", "apt-update-install-signature", "rpm-dnf-yum"] | sort)
' "$IMAGE_CONTRACT" >/dev/null || fail "invalid image contract"

expected_smoke=$(printf '%s' '{"alpine":{"apk":"2.14.9"},"composer":{"composer":"2.9.5"},"debian":{"apt":"2.6.1"},"dotnet":{"dotnet":"8.0.414"},"go":{"go":"1.26.0"},"homebrew":{"brew":"4.6.20"},"jvm":{"gradle":"9.0.0","java":"21.0.8","maven":"3.9.11","sbt":"1.11.6"},"node":{"bun":"1.3.11","node":"22.20.0","npm":"11.12.1","pnpm":"11.9.0","yarn":"1.22.22","yarn-berry":"4.10.3"},"python":{"pip":"25.2","poetry":"2.2.1","python":"3.13.7","uv":"0.8.17"},"ruby":{"bundler":"2.7.1","ruby":"3.4.5"},"rust":{"cargo":"1.89.0","rust":"1.89.0"},"rpm":{"dnf":"4.14.0","yum":"4.14.0"}}' | jq -Sc .)
actual_smoke=$(jq -Sc 'map({key: .target, value: .smoke}) | from_entries' "$IMAGE_CONTRACT")
[ "$actual_smoke" = "$expected_smoke" ] || fail "client smoke versions differ from the approved exact matrix"

for case_file in "$CASES"/*.yaml; do
  case_name=$(jq -r '.name' "$case_file")
  image=$(jq -r '.image' "$case_file")
  case "$image" in
    katch/package-client-*:*) ;;
    *) fail "$case_name bypasses the locally built isolation images: $image" ;;
  esac
  matches=$(jq --arg case_name "$case_name" --arg image "$image" '[.[] | select(.image == $image and (.cases | index($case_name)))] | length' "$IMAGE_CONTRACT")
  [ "$matches" -eq 1 ] || fail "$case_name does not match exactly one image contract entry"
  for phase in setup run assert; do
    jq -r --arg phase "$phase" '.[$phase]' "$case_file" > "$workdir/$case_name-$phase.sh"
    sh -n "$workdir/$case_name-$phase.sh" || fail "$case_name has invalid $phase shell syntax"
  done
done

jq -c '.[]' "$IMAGE_CONTRACT" | while IFS= read -r row; do
  dockerfile=$ROOT/e2e/package-mirrors/images/$(printf '%s' "$row" | jq -r '.dockerfile')
  target=$(printf '%s' "$row" | jq -r '.target')
  user=$(printf '%s' "$row" | jq -r '.user')
  [ -f "$dockerfile" ] || fail "missing Dockerfile for $target"
  stage=$(awk -v target="$target" '
    toupper($1) == "FROM" && found { exit }
    toupper($1) == "FROM" && toupper($(NF - 1)) == "AS" && $NF == target { found = 1 }
    found { print }
    END { if (!found) exit 1 }
  ' "$dockerfile") || fail "missing image target: $target"
  printf '%s\n' "$stage" | grep -F 'USER root' >/dev/null || fail "$target does not start as root"
  printf '%s\n' "$stage" | grep -F 'client-entrypoint.sh' >/dev/null || fail "$target lacks the common entrypoint"
  printf '%s\n' "$stage" | grep -F 'client-smoke.sh' >/dev/null || fail "$target lacks offline smoke"
  case $target in
    homebrew)
      printf '%s\n' "$stage" | grep -F '&& runuser -u linuxbrew -- env HOME=/home/linuxbrew USER=linuxbrew LOGNAME=linuxbrew /usr/local/bin/katch-client-smoke' >/dev/null ||
        fail "Homebrew does not run build smoke as linuxbrew with an explicit identity environment"
      final_user=$(printf '%s\n' "$stage" | awk 'toupper($1) == "USER" { user = $2 } END { print user }')
      [ "$final_user" = root ] || fail "Homebrew entrypoint does not remain root for firewall setup"
      ;;
    *)
      printf '%s\n' "$stage" | grep -F '&& /usr/local/bin/katch-client-smoke' >/dev/null || fail "$target does not run exact client smoke while building"
      ;;
  esac
  printf '%s\n' "$stage" | grep -F 'ENTRYPOINT ["/usr/local/bin/katch-client-entrypoint"]' >/dev/null || fail "$target bypasses isolation entrypoint"
  printf '%s\n' "$stage" | grep -F "KATCH_CLIENT_USER=$user" >/dev/null || fail "$target has the wrong runtime user"
  printf '%s\n' "$stage" | grep -F 'ca-certificates' >/dev/null || fail "$target does not install the system CA trust tooling"
  printf '%s' "$row" | jq -r '.smoke | to_entries[] | "\(.key) \(.value)"' | while IFS=' ' read -r client version; do
    grep -F "expect_version $client $version" "$IMAGE_SMOKE" >/dev/null ||
      fail "$target smoke metadata is not enforced for $client $version"
  done
  for tool in iptables ip6tables strace runuser; do
    printf '%s\n' "$stage" | grep -F "$tool" >/dev/null || fail "$target does not declare $tool"
  done
done

for dockerfile in "$ROOT"/e2e/package-mirrors/images/*.Dockerfile; do
  awk 'toupper($1) == "FROM" { print $2 }' "$dockerfile" | while IFS= read -r base; do
    case $base in
      *:*) ;;
      *@sha256:*) ;;
      *) fail "untagged base image in $dockerfile: $base" ;;
    esac
  done
done

if grep -R -n -E '(:latest|curl[^|]*[|][[:space:]]*(sh|bash)|--force)' "$ROOT/e2e/package-mirrors/images"/*.Dockerfile; then
  fail "image definitions contain latest, curl-pipe-shell, or forced installs"
fi

homebrew_dockerfile=$ROOT/e2e/package-mirrors/images/homebrew.Dockerfile
github_cli_cleanup_line=$(grep -n -F 'rm -f /etc/apt/sources.list.d/github-cli.list /etc/apt/sources.list.d/github-cli.sources' "$homebrew_dockerfile" | cut -d: -f1)
apt_update_line=$(grep -n -m1 -F 'apt-get update' "$homebrew_dockerfile" | cut -d: -f1)
[ -n "$github_cli_cleanup_line" ] || fail "Homebrew image does not remove the known GitHub CLI apt sources"
[ -n "$apt_update_line" ] || fail "Homebrew image does not update apt metadata"
[ "$github_cli_cleanup_line" -lt "$apt_update_line" ] || fail "Homebrew image removes GitHub CLI apt sources after apt-get update"
if grep -n -E '(AllowUnauthenticated|AllowInsecureRepositories|AllowDowngradeToInsecureRepositories|--allow-unauthenticated|--allow-insecure-repositories|trusted[[:space:]]*=[[:space:]]*yes)' "$homebrew_dockerfile"; then
  fail "Homebrew image disables apt signature verification"
fi

nuget_required_upstreams=$(jq -c '.required_upstreams' "$CASES/nuget.yaml")
[ "$nuget_required_upstreams" = '["api.nuget.org","nuget.azure.cn","azuresearch-usnc.nuget.org","azuresearch-ea.nuget.org","azuresearch-sea.nuget.org","globalcdn.nuget.org","www.nuget.org"]' ] ||
  fail "NuGet required_upstreams does not match the fixed official host contract"
nuget_setup=$(jq -r '.setup' "$CASES/nuget.yaml")
printf '%s\n' "$nuget_setup" | grep -Fx '    <NuGetAudit>false</NuGetAudit>' >/dev/null ||
  fail "NuGet test project does not disable only the out-of-scope vulnerability audit"
printf '%s\n' "$nuget_setup" | grep -F "'using System;'" >/dev/null ||
  fail "NuGet generated source does not import System for Console"
nuget_run=$(jq -r '.run' "$CASES/nuget.yaml")
[ "$(printf '%s\n' "$nuget_run" | grep -Fxc 'export DOTNET_CLI_TELEMETRY_OPTOUT=1')" -eq 1 ] ||
  fail "NuGet run does not opt out of .NET CLI telemetry with the exact documented environment setting"
[ "$(printf '%s\n' "$nuget_run" | grep -Fxc 'export NUGET_CERT_REVOCATION_MODE=offline')" -eq 1 ] ||
  fail "NuGet run does not use the exact documented offline certificate revocation mode"
nuget_case=$(jq -r '[.setup, .run, .assert] | join("\n")' "$CASES/nuget.yaml")
if printf '%s\n' "$nuget_case" | grep -i -E '(signatureValidationMode|allowUntrusted|allowInsecureConnections|disableTLSCertificateValidation|--allow-insecure-connections)'; then
  fail "NuGet mirror case disables package signatures, repository signatures, or TLS certificate validation"
fi

npm_run=$(jq -r '.run' "$CASES/npm.yaml")
printf '%s\n' "$npm_run" | grep -Fx '(cd npm && npm install --ignore-scripts --no-audit --no-update-notifier --registry="$registry" --replace-registry-host=always)' >/dev/null ||
  fail "npm mirror case does not disable audit and the npm update notifier"
printf '%s\n' "$npm_run" | grep -Fx '(cd pnpm && PNPM_CONFIG_UPDATE_NOTIFIER=false pnpm install --ignore-scripts --registry="$registry")' >/dev/null ||
  fail "npm mirror case does not disable the pnpm update notifier"
printf '%s\n' "$npm_run" | grep -Fx '(cd yarn-berry && yarn-berry config set npmRegistryServer "$registry" && yarn-berry config set unsafeHttpWhitelist --json "[\"$KATCH_HOST\"]" && yarn-berry install --mode=skip-build)' >/dev/null ||
  fail "npm mirror case does not allow HTTP only for the configured Katch host before Yarn Berry install"

pypi_run=$(jq -r '.run' "$CASES/pypi.yaml")
printf '%s\n' "$pypi_run" | grep -Fx 'pip/.venv/bin/python -m pip install --disable-pip-version-check --no-cache-dir --trusted-host "$KATCH_HOST" --index-url "$index" idna==3.10' >/dev/null ||
  fail "pip mirror case does not trust HTTP only for the configured Katch host"
printf '%s\n' "$pypi_run" | grep -Fx 'uv pip install --no-cache --python uv/.venv/bin/python --allow-insecure-host "$KATCH_HOST" --index-url "$index" idna==3.10' >/dev/null ||
  fail "uv mirror case does not allow HTTP only for the configured Katch host"
if printf '%s\n' "$pypi_run" | grep -E -- '(^|[[:space:]])(--trusted-host|--allow-insecure-host)(=|[[:space:]])[^[:space:]]*(\*|pypi[.]org|files[.]pythonhosted[.]org)|(^|[[:space:]])(PIP_TRUSTED_HOST|UV_INSECURE_HOST|UV_ALLOW_INSECURE_HOST|PYTHONHTTPSVERIFY|CURL_CA_BUNDLE|REQUESTS_CA_BUNDLE)='; then
  fail "PyPI mirror case broadens HTTP or TLS trust beyond the configured Katch host"
fi

jq -r '.run' "$CASES/homebrew.yaml" | grep -F 'HOMEBREW_ARTIFACT_DOMAIN="${KATCH_URL%/}/v2/ghcr.io"' >/dev/null ||
  fail "Homebrew bottle route changed from the approved /v2/ghcr.io contract"
jq -r '.run' "$CASES/registry-git-regression.yaml" | grep -F 'git clone --depth=1' >/dev/null ||
  fail "Git regression is no longer executed"
jq -r '.run' "$CASES/registry-git-regression.yaml" | grep -F 'blocked: $client is unavailable in the pinned client image' >/dev/null ||
  fail "Docker/Podman regression is no longer explicitly blocked"

if grep -R -n -E '^[[:space:]]*(docker|podman)[[:space:]].*(-v|--volume)[= ]?/var/run/(docker|podman)\.sock' "$ROOT/e2e/package-mirrors"; then
  fail "harness mounts a host container socket"
fi
if grep -R -n -E '(^|[;&|[:space:]])eval([;&|[:space:]]|$)' "$ROOT/e2e/package-mirrors" --include='*.sh'; then
  fail "harness uses eval"
fi

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
    elif [ "$last" = /etc/passwd ]; then
      user=client
      for argument do
        case $argument in user=*) user=${argument#user=} ;; esac
      done
      printf '/home/%s\n' "$user"
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
    printf '%s\n' "${0##*/} $*" >> "$ENTRYPOINT_EVENT_LOG"
    ;;
  install)
    printf '%s\n' "install $*" >> "$ENTRYPOINT_EVENT_LOG"
    ;;
  update-ca-certificates|update-ca-trust)
    printf '%s\n' "${0##*/} $*" >> "$ENTRYPOINT_EVENT_LOG"
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
    printf '%s\n' "runuser $*" >> "$ENTRYPOINT_EVENT_LOG"
    printf '%s\n' "$*" >> "$RUNUSER_LOG"
    ;;
  *) exit 64 ;;
esac
EOF
chmod 0555 "$fake_bin/fake-command"
for command in awk id install iptables ip6tables iptables-save iptables-restore ip6tables-save ip6tables-restore strace runuser; do
  ln -s fake-command "$fake_bin/$command"
done

run_entrypoint() {
  artifacts=$1
  shift
  env PATH="$fake_bin:$PATH" RUNUSER_LOG="$workdir/runuser.log" ENTRYPOINT_EVENT_LOG="$workdir/entrypoint-events.log" \
    KATCH_HOST=katch.invalid KATCH_PORT=8080 KATCH_ARTIFACTS="$artifacts" \
    "$@" "$ENTRYPOINT"
}

: > "$workdir/runuser.log"
: > "$workdir/entrypoint-events.log"
run_entrypoint "$workdir/default-artifacts" env -u KATCH_CLIENT_USER -u KATCH_CLIENT_CA_CERT
grep -q '^-u client --preserve-environment -- env HOME=/home/client USER=client LOGNAME=client /bin/sh -eu -c ' "$workdir/runuser.log" ||
  fail "entrypoint did not retain the default client user and identity environment"
if grep -E '^(install|update-ca-certificates|update-ca-trust) ' "$workdir/entrypoint-events.log"; then
  fail "entrypoint changed system trust when no client CA was configured"
fi

: > "$workdir/runuser.log"
: > "$workdir/entrypoint-events.log"
run_entrypoint "$workdir/homebrew-artifacts" env -u KATCH_CLIENT_CA_CERT KATCH_CLIENT_USER=linuxbrew
grep -q '^-u linuxbrew --preserve-environment -- env HOME=/home/linuxbrew USER=linuxbrew LOGNAME=linuxbrew /bin/sh -eu -c ' "$workdir/runuser.log" ||
  fail "entrypoint did not switch to the requested client identity environment"

trusted_ca=$workdir/test-ca.crt
printf '%s\n' 'test certificate' > "$trusted_ca"
ln -s fake-command "$fake_bin/update-ca-certificates"
: > "$workdir/runuser.log"
: > "$workdir/entrypoint-events.log"
run_entrypoint "$workdir/debian-ca-artifacts" env KATCH_CLIENT_CA_CERT="$trusted_ca"
grep -Fx "install -m 0644 $trusted_ca /usr/local/share/ca-certificates/katch-test-ca.crt" "$workdir/entrypoint-events.log" >/dev/null ||
  fail "entrypoint did not install the trusted CA into the update-ca-certificates anchor directory"
grep -Fx 'update-ca-certificates ' "$workdir/entrypoint-events.log" >/dev/null ||
  fail "entrypoint did not refresh Debian/Ubuntu/Alpine system trust"
ca_update_line=$(grep -n -F 'update-ca-certificates ' "$workdir/entrypoint-events.log" | cut -d: -f1)
firewall_line=$(grep -n -m1 -E '^iptables (-F|-P|-A)' "$workdir/entrypoint-events.log" | cut -d: -f1)
user_drop_line=$(grep -n -F 'runuser -u client ' "$workdir/entrypoint-events.log" | cut -d: -f1)
[ "$ca_update_line" -lt "$firewall_line" ] || fail "entrypoint installs the trusted CA after firewall setup"
[ "$ca_update_line" -lt "$user_drop_line" ] || fail "entrypoint installs the trusted CA after the user drop"
rm "$fake_bin/update-ca-certificates"

ln -s fake-command "$fake_bin/update-ca-trust"
: > "$workdir/entrypoint-events.log"
run_entrypoint "$workdir/rpm-ca-artifacts" env KATCH_CLIENT_CA_CERT="$trusted_ca"
grep -Fx "install -m 0644 $trusted_ca /etc/pki/ca-trust/source/anchors/katch-test-ca.crt" "$workdir/entrypoint-events.log" >/dev/null ||
  fail "entrypoint did not install the trusted CA into the update-ca-trust anchor directory"
grep -Fx 'update-ca-trust extract' "$workdir/entrypoint-events.log" >/dev/null ||
  fail "entrypoint did not refresh RPM system trust"
rm "$fake_bin/update-ca-trust"

: > "$workdir/runuser.log"
: > "$workdir/entrypoint-events.log"
if run_entrypoint "$workdir/no-ca-tool-artifacts" env KATCH_CLIENT_CA_CERT="$trusted_ca" >"$workdir/out" 2>&1; then
  fail "entrypoint accepted a trusted CA without a supported system trust mechanism"
fi
grep -F 'no supported system CA trust mechanism is available' "$workdir/out" >/dev/null ||
  fail "entrypoint did not report the missing system trust mechanism"
[ ! -s "$workdir/runuser.log" ] || fail "entrypoint dropped users after trusted CA installation failed"

harness_bin=$workdir/harness-bin
mkdir -p "$harness_bin"
cat > "$harness_bin/fake-runtime" <<'EOF'
#!/bin/sh
set -eu
for argument do
  printf 'ARG=%s\n' "$argument"
done >> "$RUNTIME_LOG"
printf '%s\n' END >> "$RUNTIME_LOG"
EOF
cat > "$harness_bin/curl" <<'EOF'
#!/bin/sh
set -eu
count=0
[ ! -f "$METRIC_STATE" ] || count=$(cat "$METRIC_STATE")
case $count in
  0) total=0 ;;
  *) total=1 ;;
esac
printf '%s\n' "$((count + 1))" > "$METRIC_STATE"
printf 'katch_origin_requests_total{upstream="example.invalid"} %s\n' "$total"
EOF
chmod 0555 "$harness_bin/fake-runtime" "$harness_bin/curl"

harness_case=$workdir/harness-case.yaml
printf '%s\n' '{"name":"ca-contract","image":"example/client:1","setup":"true","run":"true","assert":"true","required_upstreams":["example.invalid"]}' > "$harness_case"
run_harness() {
  artifact_dir=$1
  shift
  : > "$workdir/runtime.log"
  printf '%s\n' 0 > "$workdir/metric-state"
  env PATH="$harness_bin:$PATH" RUNTIME_LOG="$workdir/runtime.log" METRIC_STATE="$workdir/metric-state" \
    CONTAINER_RUNTIME=fake-runtime ARTIFACT_ROOT="$artifact_dir" \
    KATCH_URL=https://katch.invalid KATCH_METRICS_URL=http://metrics.invalid \
    KATCH_HOST=katch.invalid KATCH_PORT=443 \
    "$@" "$HARNESS" "$harness_case"
}

run_harness "$workdir/harness-unset" env -u KATCH_CA_CERT >"$workdir/out" 2>&1
if grep -E 'KATCH_(CA_CERT|CLIENT_CA_CERT)|/run/katch-test-ca.crt' "$workdir/runtime.log"; then
  fail "harness changed the container contract when KATCH_CA_CERT was unset"
fi

: > "$workdir/runtime.log"
if run_harness "$workdir/harness-relative" env KATCH_CA_CERT=relative/ca.crt >"$workdir/out" 2>&1; then
  fail "harness accepted a relative KATCH_CA_CERT"
fi
grep -F 'KATCH_CA_CERT must be an absolute readable regular file' "$workdir/out" >/dev/null ||
  fail "harness did not report the invalid relative CA path"
[ ! -s "$workdir/runtime.log" ] || fail "harness started a client for an invalid relative CA path"

invalid_ca_dir=$workdir/invalid-ca-dir
mkdir "$invalid_ca_dir"
: > "$workdir/runtime.log"
if run_harness "$workdir/harness-directory" env KATCH_CA_CERT="$invalid_ca_dir" >"$workdir/out" 2>&1; then
  fail "harness accepted a directory as KATCH_CA_CERT"
fi
[ ! -s "$workdir/runtime.log" ] || fail "harness started a client for a non-regular CA path"

run_harness "$workdir/harness-ca" env KATCH_CA_CERT="$trusted_ca" >"$workdir/out" 2>&1
[ "$(grep -Fc "ARG=type=bind,src=$trusted_ca,dst=/run/katch-test-ca.crt,readonly" "$workdir/runtime.log")" -eq 2 ] ||
  fail "harness did not mount the trusted CA read-only at the fixed client path for both phases"
[ "$(grep -Fc 'ARG=KATCH_CLIENT_CA_CERT=/run/katch-test-ca.crt' "$workdir/runtime.log")" -eq 2 ] ||
  fail "harness did not pass the internal trusted CA path for both phases"
if grep -F 'ARG=KATCH_CA_CERT=' "$workdir/runtime.log"; then
  fail "harness exposed the host CA path as a client environment variable"
fi

grep -F -- '--env "KATCH_URL=$KATCH_URL"' "$HARNESS" >/dev/null ||
  fail "harness does not forward KATCH_URL"
grep -F -- '--env "KATCH_BASE_URL=$KATCH_URL"' "$HARNESS" >/dev/null ||
  fail "harness does not forward the consistent base URL alias"

bad_case=$workdir/bad.yaml
printf '%s\n' '{"name":"bad; touch /tmp/injected","image":"busybox:latest","setup":"true","run":"true","assert":"true","required_upstreams":[]}' > "$bad_case"
if "$HARNESS" --check "$bad_case" >"$workdir/out" 2>&1; then
  fail "unpinned image and unsafe case name were accepted"
fi

printf '%s\n' "harness self-tests passed"
