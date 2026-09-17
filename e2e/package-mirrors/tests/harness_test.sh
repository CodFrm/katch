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

homebrew_smoke_bin=$workdir/homebrew-smoke-bin
homebrew_runuser_log=$workdir/homebrew-runuser.log
homebrew_identity_log=$workdir/homebrew-identity.log
homebrew_id_log=$workdir/homebrew-id.log
mkdir -p "$homebrew_smoke_bin"
cat > "$homebrew_smoke_bin/id" <<'EOF'
#!/bin/sh
set -eu
printf '%s\n' "$*" >> "${ID_LOG:?ID_LOG is required}"
[ "$#" -eq 1 ] && [ "$1" = -u ] || exit 64
printf '%s\n' "${FAKE_UID:?FAKE_UID is required}"
EOF
cat > "$homebrew_smoke_bin/getent" <<'EOF'
#!/bin/sh
set -eu
[ "$#" -eq 2 ] && [ "$1" = passwd ] && [ "$2" = linuxbrew ] || exit 64
[ "${FAKE_USER_EXISTS:-1}" = 1 ] || exit 2
printf '%s\n' 'linuxbrew:x:1000:1000:Linuxbrew:/home/linuxbrew:/bin/bash'
EOF
cat > "$homebrew_smoke_bin/runuser" <<'EOF'
#!/bin/sh
set -eu
: > "${RUNUSER_LOG:?RUNUSER_LOG is required}"
for argument do
  printf '<%s>\n' "$argument" >> "$RUNUSER_LOG"
done
[ "$#" -eq 8 ] && [ "$1" = -u ] && [ "$2" = linuxbrew ] && [ "$3" = -- ] && [ "$4" = env ] || exit 64
shift 3
FAKE_UID=1000 "$@"
EOF
cat > "$homebrew_smoke_bin/brew" <<'EOF'
#!/bin/sh
set -eu
printf '%s|%s|%s\n' "$HOME" "$USER" "$LOGNAME" > "${BREW_IDENTITY_LOG:?BREW_IDENTITY_LOG is required}"
printf '%s\n' 'Homebrew 4.6.20'
EOF
cat > "$homebrew_smoke_bin/go" <<'EOF'
#!/bin/sh
set -eu
printf '%s\n' 'go version go1.26.0 linux/amd64'
EOF
chmod 0555 "$homebrew_smoke_bin"/*

: > "$homebrew_id_log"
FAKE_UID=0 ID_LOG="$homebrew_id_log" RUNUSER_LOG="$homebrew_runuser_log" \
  BREW_IDENTITY_LOG="$homebrew_identity_log" KATCH_CLIENT_FLAVOR=homebrew \
  KATCH_CLIENT_USER=linuxbrew HOME=/root USER=root LOGNAME=root \
  PATH="$homebrew_smoke_bin:$PATH" "$IMAGE_SMOKE" >"$workdir/homebrew-smoke.out" 2>"$workdir/homebrew-smoke.err" ||
  fail "root Homebrew smoke was not demoted: $(cat "$workdir/homebrew-smoke.err")"
[ "$(cat "$workdir/homebrew-smoke.out")" = 'brew=4.6.20' ] ||
  fail "demoted Homebrew smoke did not preserve the exact version check"
expected_runuser=$(printf '%s\n' '<-u>' '<linuxbrew>' '<-->' '<env>' '<HOME=/home/linuxbrew>' '<USER=linuxbrew>' '<LOGNAME=linuxbrew>' "<$IMAGE_SMOKE>")
[ "$(cat "$homebrew_runuser_log")" = "$expected_runuser" ] ||
  fail "root Homebrew smoke did not re-exec itself through the exact runuser contract"
[ "$(cat "$homebrew_identity_log")" = '/home/linuxbrew|linuxbrew|linuxbrew' ] ||
  fail "demoted Homebrew smoke did not use the passwd identity environment"
[ "$(wc -l < "$homebrew_id_log" | tr -d ' ')" -eq 2 ] ||
  fail "demoted Homebrew smoke recursed or skipped the nonroot execution"

rm -f "$homebrew_runuser_log"
FAKE_UID=1000 ID_LOG="$homebrew_id_log" RUNUSER_LOG="$homebrew_runuser_log" \
  BREW_IDENTITY_LOG="$homebrew_identity_log" KATCH_CLIENT_FLAVOR=homebrew \
  KATCH_CLIENT_USER=linuxbrew HOME=/tmp/caller-home USER=caller LOGNAME=caller \
  PATH="$homebrew_smoke_bin:$PATH" "$IMAGE_SMOKE" >"$workdir/homebrew-smoke.out" 2>"$workdir/homebrew-smoke.err" ||
  fail "nonroot Homebrew smoke changed behavior: $(cat "$workdir/homebrew-smoke.err")"
[ ! -e "$homebrew_runuser_log" ] || fail "nonroot Homebrew smoke invoked runuser"
[ "$(cat "$homebrew_identity_log")" = '/tmp/caller-home|caller|caller' ] ||
  fail "nonroot Homebrew smoke replaced the caller identity environment"

rm -f "$homebrew_runuser_log"
: > "$homebrew_id_log"
ID_LOG="$homebrew_id_log" RUNUSER_LOG="$homebrew_runuser_log" KATCH_CLIENT_FLAVOR=go \
  PATH="$homebrew_smoke_bin:$PATH" "$IMAGE_SMOKE" >"$workdir/homebrew-smoke.out" 2>"$workdir/homebrew-smoke.err" ||
  fail "non-Homebrew smoke changed behavior: $(cat "$workdir/homebrew-smoke.err")"
[ "$(cat "$workdir/homebrew-smoke.out")" = 'go=1.26.0' ] ||
  fail "non-Homebrew smoke did not preserve the exact version check"
[ ! -s "$homebrew_id_log" ] || fail "non-Homebrew smoke inspected or changed its user"
[ ! -e "$homebrew_runuser_log" ] || fail "non-Homebrew smoke invoked runuser"

rm -f "$homebrew_runuser_log"
if env -u KATCH_CLIENT_USER FAKE_UID=0 ID_LOG="$homebrew_id_log" RUNUSER_LOG="$homebrew_runuser_log" \
  BREW_IDENTITY_LOG="$homebrew_identity_log" KATCH_CLIENT_FLAVOR=homebrew \
  PATH="$homebrew_smoke_bin:$PATH" "$IMAGE_SMOKE" >"$workdir/homebrew-smoke.out" 2>"$workdir/homebrew-smoke.err"; then
  fail "root Homebrew smoke accepted an empty client user"
fi
[ ! -e "$homebrew_runuser_log" ] || fail "root Homebrew smoke invoked runuser with an empty client user"

rm -f "$homebrew_runuser_log"
if FAKE_UID=0 FAKE_USER_EXISTS=0 ID_LOG="$homebrew_id_log" RUNUSER_LOG="$homebrew_runuser_log" \
  BREW_IDENTITY_LOG="$homebrew_identity_log" KATCH_CLIENT_FLAVOR=homebrew \
  KATCH_CLIENT_USER=linuxbrew PATH="$homebrew_smoke_bin:$PATH" \
  "$IMAGE_SMOKE" >"$workdir/homebrew-smoke.out" 2>"$workdir/homebrew-smoke.err"; then
  fail "root Homebrew smoke accepted a nonexistent client user"
fi
[ ! -e "$homebrew_runuser_log" ] || fail "root Homebrew smoke invoked runuser with a nonexistent client user"

if grep -n -E '(safe[.]directory|(^|[[:space:]])(chmod|chown)([[:space:]]|$))' "$IMAGE_SMOKE"; then
  fail "client smoke bypasses Git ownership checks or changes filesystem ownership/modes"
fi

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

rpm_required_upstreams=$(jq -c '.required_upstreams' "$CASES/rpm.yaml")
[ "$rpm_required_upstreams" = '["ftp.riken.jp","www.centos.org"]' ] ||
  fail "RPM required_upstreams does not match the fixed RIKEN mirror and official GPG key hosts"
rpm_setup_script=$workdir/rpm-setup.sh
rpm_setup_root=$workdir/rpm-setup
jq -r '.setup' "$CASES/rpm.yaml" > "$rpm_setup_script"
mkdir -p "$rpm_setup_root"
(
  cd "$rpm_setup_root"
  KATCH_URL=https://katch.test /bin/sh "$rpm_setup_script"
)
expected_rpm_repo=$(printf '%s\n' \
  '[katch]' \
  'name=katch fixed baseurl' \
  'baseurl=https://katch.test/ftp.riken.jp/Linux/centos-stream/9-stream/BaseOS/x86_64/os/' \
  'mirrorlist=' \
  'metalink=' \
  'enabled=1' \
  'gpgcheck=1' \
  'gpgkey=https://katch.test/www.centos.org/keys/RPM-GPG-KEY-CentOS-Official')
[ "$(cat "$rpm_setup_root/katch.repo")" = "$expected_rpm_repo" ] ||
  fail "RPM repo does not use the exact fixed RIKEN baseurl and official signature configuration"
rpm_case=$(jq -r '[.setup, .run, .assert] | join("\n")' "$CASES/rpm.yaml")
if printf '%s\n' "$rpm_case" | grep -E 'https?://|mirror[.]stream[.]centos[.]org'; then
  fail "RPM mirror case contains an alternate direct URL or the region-blocked official CDN"
fi

nuget_required_upstreams=$(jq -c '.required_upstreams' "$CASES/nuget.yaml")
[ "$nuget_required_upstreams" = '["api.nuget.org","nuget.azure.cn","azuresearch-usnc.nuget.org","azuresearch-ea.nuget.org","azuresearch-sea.nuget.org","globalcdn.nuget.org","www.nuget.org"]' ] ||
  fail "NuGet required_upstreams does not match the fixed official host contract"
nuget_setup=$(jq -r '.setup' "$CASES/nuget.yaml")
printf '%s\n' "$nuget_setup" | grep -Fx '    <NuGetAudit>false</NuGetAudit>' >/dev/null ||
  fail "NuGet test project does not disable only the out-of-scope vulnerability audit"
printf '%s\n' "$nuget_setup" | grep -Fx '    <PackageReference Include="Newtonsoft.Json" Version="13.0.3" />' >/dev/null ||
  fail "NuGet test project does not use the exact Newtonsoft.Json dependency"
printf '%s\n' "$nuget_setup" | grep -F "'using System;'" >/dev/null ||
  fail "NuGet generated source does not import System for Console"
nuget_setup_script=$workdir/nuget-setup.sh
nuget_phase_root=$workdir/nuget-phase
jq -r '.setup' "$CASES/nuget.yaml" > "$nuget_setup_script"
mkdir -p "$nuget_phase_root/cold" "$nuget_phase_root/warm"
nuget_phase_root=$(CDPATH= cd -- "$nuget_phase_root" && pwd)
(
  cd "$nuget_phase_root/cold"
  KATCH_PHASE=cold /bin/sh "$nuget_setup_script"
)
[ ! -e "$nuget_phase_root/cold/app/packages.lock.json" ] ||
  fail "NuGet cold setup creates a lock file instead of exercising normal dependency resolution"
(
  cd "$nuget_phase_root/warm"
  KATCH_PHASE=warm /bin/sh "$nuget_setup_script"
)
[ -f "$nuget_phase_root/warm/app/packages.lock.json" ] ||
  fail "NuGet warm setup does not create packages.lock.json"
expected_nuget_lock=$(printf '%s' '{"dependencies":{"net8.0":{"Newtonsoft.Json":{"contentHash":"HrC5BXdl00IP9zeV+0Z848QWPAoCr9P3bDEZguI+gkLcBKAOxix/tLEAAHC+UvDNPv4a2d18lOReHMOagPa+zQ==","requested":"[13.0.3, )","resolved":"13.0.3","type":"Direct"}}},"version":1}' | jq -Sc .)
actual_nuget_lock=$(jq -Sc . "$nuget_phase_root/warm/app/packages.lock.json")
[ "$actual_nuget_lock" = "$expected_nuget_lock" ] ||
  fail "NuGet warm lock does not match the approved Newtonsoft.Json 13.0.3 lock"
nuget_run=$(jq -r '.run' "$CASES/nuget.yaml")
[ "$(printf '%s\n' "$nuget_run" | grep -Fxc 'export NUGET_HTTP_CACHE_PATH="$KATCH_SHARED/nuget-http-cache"')" -eq 1 ] ||
  fail "NuGet run does not use the exact documented HTTP metadata cache path under KATCH_SHARED"
if printf '%s\n' "$nuget_run" | grep -E 'NUGET_PACKAGES=.*KATCH_SHARED|NUGET_PACKAGES=.*[/]shared'; then
  fail "NuGet run shares the global packages cache instead of only HTTP metadata"
fi
[ "$(printf '%s\n' "$nuget_run" | grep -Fxc 'export DOTNET_CLI_TELEMETRY_OPTOUT=1')" -eq 1 ] ||
  fail "NuGet run does not opt out of .NET CLI telemetry with the exact documented environment setting"
[ "$(printf '%s\n' "$nuget_run" | grep -Fxc 'export NUGET_CERT_REVOCATION_MODE=offline')" -eq 1 ] ||
  fail "NuGet run does not use the exact documented offline certificate revocation mode"
nuget_run_script=$workdir/nuget-run.sh
nuget_phase_bin=$workdir/nuget-phase-bin
nuget_phase_events=$workdir/nuget-phase-events.log
jq -r '.run' "$CASES/nuget.yaml" > "$nuget_run_script"
mkdir -p "$nuget_phase_bin"
cat > "$nuget_phase_bin/dotnet" <<'EOF'
#!/bin/sh
set -eu
[ "${DOTNET_CLI_TELEMETRY_OPTOUT:-}" = 1 ] || exit 65
[ "${NUGET_CERT_REVOCATION_MODE:-}" = offline ] || exit 65
[ "${NUGET_HTTP_CACHE_PATH:-}" = "$KATCH_SHARED/nuget-http-cache" ] || exit 65
case ${NUGET_PACKAGES:-} in
  "$KATCH_SHARED"|"$KATCH_SHARED"/*) exit 65 ;;
esac
printf '%s\n' "$*" >> "$NUGET_EVENT_LOG"
EOF
chmod 0555 "$nuget_phase_bin/dotnet"
run_nuget_phase() {
  phase=$1
  phase_root=$nuget_phase_root/$phase
  : > "$nuget_phase_events"
  (
    cd "$phase_root"
    env -u DOTNET_CLI_TELEMETRY_OPTOUT -u NUGET_CERT_REVOCATION_MODE \
      -u NUGET_HTTP_CACHE_PATH -u NUGET_PACKAGES \
      PATH="$nuget_phase_bin:$PATH" KATCH_PHASE="$phase" KATCH_URL=https://katch.test \
      KATCH_SHARED=/shared \
      NUGET_EVENT_LOG="$nuget_phase_events" /bin/sh "$nuget_run_script"
  )
}
run_nuget_phase cold
expected_cold_restore="restore app/app.csproj --source https://katch.test/api.nuget.org/v3/index.json --packages $nuget_phase_root/cold/packages"
expected_nuget_search='package search Newtonsoft.Json --source https://katch.test/api.nuget.org/v3/index.json --take 1 --format json'
expected_cold_events=$(printf '%s\n' "$expected_cold_restore" "$expected_nuget_search" '--info')
[ "$(cat "$nuget_phase_events")" = "$expected_cold_events" ] ||
  fail "NuGet cold run is not a normal restore followed by search and client initialization"
run_nuget_phase warm
expected_warm_restore="restore app/app.csproj --source https://katch.test/api.nuget.org/v3/index.json --packages $nuget_phase_root/warm/packages --locked-mode"
expected_warm_events=$(printf '%s\n' "$expected_warm_restore" "$expected_nuget_search" '--info')
[ "$(cat "$nuget_phase_events")" = "$expected_warm_events" ] ||
  fail "NuGet warm run is not a locked restore followed by search and client initialization"
nuget_case=$(jq -r '[.setup, .run, .assert] | join("\n")' "$CASES/nuget.yaml")
if printf '%s\n' "$nuget_case" | grep -i -E '(signatureValidationMode|allowUntrusted|allowInsecureConnections|disableTLSCertificateValidation|--allow-insecure-connections)'; then
  fail "NuGet mirror case disables package signatures, repository signatures, or TLS certificate validation"
fi

nuget_assert_script=$workdir/nuget-assert.sh
nuget_assert_root=$workdir/nuget-assert
nuget_bin=$workdir/nuget-bin
nuget_events=$workdir/nuget-events.log
jq -r '.assert' "$CASES/nuget.yaml" > "$nuget_assert_script"
mkdir -p "$nuget_assert_root/app/obj" "$nuget_assert_root/packages/newtonsoft.json/13.0.3" "$nuget_bin"
: > "$nuget_assert_root/packages/newtonsoft.json/13.0.3/newtonsoft.json.13.0.3.nupkg"
printf '%s\n' 'Newtonsoft.Json' > "$nuget_assert_root/search.json"
cat > "$nuget_bin/dotnet" <<'EOF'
#!/bin/sh
set -eu
[ "${DOTNET_CLI_TELEMETRY_OPTOUT:-}" = 1 ] || {
  printf '%s\n' 'dotnet invocation lacks DOTNET_CLI_TELEMETRY_OPTOUT=1' >&2
  exit 65
}
[ "${NUGET_CERT_REVOCATION_MODE:-}" = offline ] || {
  printf '%s\n' 'dotnet invocation lacks NUGET_CERT_REVOCATION_MODE=offline' >&2
  exit 65
}
case "$*" in
  'run --project app/app.csproj --no-restore')
    printf '%s\n' run >> "$NUGET_EVENT_LOG"
    printf '%s\n' '{"mirrored":true}'
    ;;
  'build-server shutdown')
    printf '%s\n' shutdown >> "$NUGET_EVENT_LOG"
    exit "${NUGET_SHUTDOWN_STATUS:-0}"
    ;;
  *) exit 64 ;;
esac
EOF
chmod 0555 "$nuget_bin/dotnet"
run_nuget_assert() {
  shutdown_status=$1
  : > "$nuget_events"
  (cd "$nuget_assert_root" && env -u DOTNET_CLI_TELEMETRY_OPTOUT -u NUGET_CERT_REVOCATION_MODE \
    PATH="$nuget_bin:$PATH" NUGET_EVENT_LOG="$nuget_events" \
    NUGET_SHUTDOWN_STATUS="$shutdown_status" /bin/sh "$nuget_assert_script")
}

run_nuget_assert 0 >"$workdir/out" 2>&1 ||
  fail "NuGet assertions or build-server shutdown failed: $(cat "$workdir/out")"
[ "$(cat "$nuget_events")" = "$(printf 'run\nshutdown\n')" ] ||
  fail "NuGet case does not shut down build servers after the final program assertion"
if run_nuget_assert 42 >"$workdir/out" 2>&1; then
  fail "NuGet case masks a build-server shutdown failure after successful assertions"
else
  nuget_status=$?
fi
[ "$nuget_status" -eq 42 ] || fail "NuGet case changed the build-server shutdown failure status"
printf '%s\n' 'https://api.nuget.org/leak' >> "$nuget_assert_root/search.json"
if run_nuget_assert 42 >"$workdir/out" 2>&1; then
  fail "NuGet case masks an assertion failure while shutting down build servers"
else
  nuget_status=$?
fi
[ "$nuget_status" -eq 1 ] || fail "NuGet build-server cleanup masked the original assertion status"
[ "$(cat "$nuget_events")" = "$(printf 'run\nshutdown\n')" ] ||
  fail "NuGet case skips build-server cleanup after an assertion failure"

rubygems_case=$CASES/rubygems.yaml
rubygems_setup_script=$workdir/rubygems-setup.sh
rubygems_run_script=$workdir/rubygems-run.sh
jq -r '.setup' "$rubygems_case" > "$rubygems_setup_script"
jq -r '.run' "$rubygems_case" > "$rubygems_run_script"
rubygems_phase_root=$workdir/rubygems-phase
mkdir -p "$rubygems_phase_root/cold" "$rubygems_phase_root/warm"
for phase in cold warm; do
  (
    cd "$rubygems_phase_root/$phase"
    KATCH_PHASE=$phase KATCH_URL=https://katch.test /bin/sh "$rubygems_setup_script"
  )
  expected_gemfile=$rubygems_phase_root/$phase/expected-Gemfile
  printf "%s\n%s\n" "source 'https://katch.test/rubygems.org/'" "gem 'rake', '13.2.1'" > "$expected_gemfile"
  cmp -s "$expected_gemfile" "$rubygems_phase_root/$phase/bundler/Gemfile" ||
    fail "RubyGems $phase setup does not write the exact Katch-only Gemfile"
  [ -d "$rubygems_phase_root/$phase/gem-home" ] ||
    fail "RubyGems $phase setup does not create a fresh GEM_HOME"
  [ -d "$rubygems_phase_root/$phase/bundler/vendor/cache" ] ||
    fail "RubyGems $phase setup does not create a fresh Bundler artifact cache"
done
[ ! -e "$rubygems_phase_root/cold/bundler/Gemfile.lock" ] ||
  fail "RubyGems cold setup pre-locks dependencies instead of exercising normal resolution"
cat > "$rubygems_phase_root/warm/expected-Gemfile.lock" <<'EOF'
GEM
  remote: https://katch.test/rubygems.org/
  specs:
    rake (13.2.1)

PLATFORMS
  ruby
  x86_64-linux

DEPENDENCIES
  rake (= 13.2.1)

BUNDLED WITH
   2.7.1
EOF
cmp -s "$rubygems_phase_root/warm/expected-Gemfile.lock" "$rubygems_phase_root/warm/bundler/Gemfile.lock" ||
  fail "RubyGems warm setup does not write the approved deterministic lock"

rubygems_bin=$workdir/rubygems-bin
rubygems_events=$workdir/rubygems-events.log
mkdir -p "$rubygems_bin"
cat > "$rubygems_bin/fake-ruby-client" <<'EOF'
#!/bin/sh
set -eu
tool=${0##*/}
printf '%s|%s|%s|%s|%s|%s\n' "$tool" "${GEM_HOME:-}" "${GEM_PATH-unset}" "${BUNDLE_GEMFILE:-}" "${BUNDLE_PATH:-}" "$*" >> "$RUBYGEMS_EVENT_LOG"
if [ "$tool" = ruby ]; then
  output=
  for argument do output=$argument; done
  : > "$output"
fi
EOF
chmod 0555 "$rubygems_bin/fake-ruby-client"
for client in ruby gem bundle; do
  ln -s fake-ruby-client "$rubygems_bin/$client"
done
run_rubygems_phase() {
  phase=$1
  : > "$rubygems_events"
  (
    cd "$rubygems_phase_root/$phase"
    PATH="$rubygems_bin:$PATH" KATCH_PHASE=$phase KATCH_URL=https://katch.test \
      RUBYGEMS_EVENT_LOG="$rubygems_events" /bin/sh "$rubygems_run_script"
  )
}
run_rubygems_phase cold
[ "$(wc -l < "$rubygems_events" | tr -d ' ')" -eq 2 ] ||
  fail "RubyGems cold run does not invoke exactly gem and Bundler"
grep -F 'gem|' "$rubygems_events" | grep -F -- 'install rake --version 13.2.1 --no-document --clear-sources --source https://katch.test/rubygems.org/' >/dev/null ||
  fail "RubyGems cold run is not a normal mirrored gem install"
grep -F 'bundle|' "$rubygems_events" | grep -F '|install' >/dev/null ||
  fail "RubyGems cold run is not a normal Bundler install"
run_rubygems_phase warm
[ "$(wc -l < "$rubygems_events" | tr -d ' ')" -eq 3 ] ||
  fail "RubyGems warm run does not invoke exactly fetch, local gem install, and local Bundler install"
grep -F 'ruby|' "$rubygems_events" | grep -F 'Gem::RemoteFetcher.fetcher.fetch_path' | grep -F 'https://katch.test/rubygems.org/gems/rake-13.2.1.gem' >/dev/null ||
  fail "RubyGems warm run does not fetch the exact immutable gem through Katch with RemoteFetcher"
grep -F 'gem|' "$rubygems_events" | grep -F -- 'install --local ' | grep -F 'rake-13.2.1.gem --no-document' >/dev/null ||
  fail "RubyGems warm run does not install the fetched artifact locally"
grep -F 'bundle|' "$rubygems_events" | grep -F '|install --local' >/dev/null ||
  fail "RubyGems warm run does not install the cached artifact locally with Bundler"
[ -f "$rubygems_phase_root/warm/bundler/vendor/cache/rake-13.2.1.gem" ] ||
  fail "RubyGems warm run does not copy the fetched artifact into fresh vendor/cache"
rubygems_case_commands=$(jq -r '[.setup, .run, .assert] | join("\n")' "$rubygems_case")
if printf '%s\n' "$rubygems_case_commands" | grep -i -E '(NoSecurity|VERIFY_NONE|ssl[_-]?verify|--trust-policy|https?://rubygems[.]org)'; then
  fail "RubyGems mirror case disables trust or uses a public RubyGems URL"
fi
printf '%s\n' "$rubygems_case_commands" | grep -F 'BUNDLE_FROZEN=true' >/dev/null ||
  fail "RubyGems warm Bundler install is not frozen"

maven_case=$CASES/maven.yaml
maven_setup=$(jq -r '.setup' "$maven_case")
maven_run=$(jq -r '.run' "$maven_case")
maven_assert=$(jq -r '.assert' "$maven_case")
printf '%s\n' "$maven_setup" | grep -Fx 'root=$PWD/maven-clients' >/dev/null ||
  fail "JVM setup does not use the phase workdir for client projects"
printf '%s\n' "$maven_setup" | grep -Fx 'mkdir -p "$PWD/client-home"' >/dev/null ||
  fail "JVM setup does not create a writable phase-local client home"
printf '%s\n' "$maven_setup" | grep -Fx 'mkdir -p "$PWD/client-tmp"' >/dev/null ||
  fail "JVM setup does not create a writable phase-local client temp directory"
printf '%s\n' "$maven_run" | grep -Fx 'export HOME="$PWD/client-home"' >/dev/null ||
  fail "JVM run does not export the phase-local client home before invoking tools"
printf '%s\n' "$maven_run" | grep -Fx 'client_tmp=$PWD/client-tmp' >/dev/null ||
  fail "JVM run does not preserve the phase-local client temp path before changing directories"
printf '%s\n' "$maven_run" | grep -Fx 'root=$PWD/maven-clients' >/dev/null ||
  fail "JVM run does not use the phase workdir for client projects"
printf '%s\n' "$maven_run" | grep -Fx '(cd "$root/sbt" && sbt -batch -Djava.io.tmpdir="$client_tmp" -Dsbt.override.build.repos=true -Dsbt.repository.config="$root/sbt/repositories" compile)' >/dev/null ||
  fail "sbt does not pass the writable phase-local temp directory as a JVM property before the compile command"
printf '%s\n' "$maven_assert" | grep -Fx 'root=$PWD/maven-clients' >/dev/null ||
  fail "JVM assertions do not use the phase workdir for client projects"
printf '%s\n' "$maven_assert" | grep -Fx 'test -d "$PWD/client-tmp" && test -w "$PWD/client-tmp"' >/dev/null ||
  fail "JVM assertions do not verify the phase-local client temp directory remains writable"
if jq -r '[.setup, .run, .assert] | join("\n")' "$maven_case" | grep -E '(^|[[:space:];|&])chmod([[:space:]]|$)|(^|[[:space:]="'"'"'])/tmp(/|[[:space:]="'"'"']|$)|allowInsecureProtocol|trustAll|disable[^[:space:]]*(TLS|SSL|Certificate)|-D[^[:space:]]*(insecure|trustStore)'; then
  fail "JVM case uses root temp state, broad permission changes, or insecure JVM transport flags"
fi

maven_setup_script=$workdir/maven-setup.sh
maven_run_script=$workdir/maven-run.sh
maven_assert_script=$workdir/maven-assert.sh
jq -r '.setup' "$maven_case" > "$maven_setup_script"
jq -r '.run' "$maven_case" > "$maven_run_script"
jq -r '.assert' "$maven_case" > "$maven_assert_script"
maven_case_bin=$workdir/maven-case-bin
maven_events=$workdir/maven-events.log
mkdir -p "$maven_case_bin"
: > "$maven_events"
cat > "$maven_case_bin/fake-jvm-client" <<'EOF'
#!/bin/sh
set -eu
phase_root=${PWD%%/maven-clients/*}
[ "$HOME" = "$phase_root/client-home" ]
[ -d "$HOME" ] && [ -w "$HOME" ]
tool=${0##*/}
if [ "$tool" = sbt ]; then
  expected_tmp=$phase_root/client-tmp
  [ -d "$expected_tmp" ] && [ -w "$expected_tmp" ]
  found_tmp=false
  for argument in "$@"; do
    [ "$argument" = "-Djava.io.tmpdir=$expected_tmp" ] && found_tmp=true
  done
  [ "$found_tmp" = true ]
fi
: > "$HOME/$tool.cache"
printf '%s|%s|%s\n' "$tool" "$PWD" "$HOME" >> "$JVM_EVENT_LOG"
case $tool in
  mvn) output=target/classes/example/Example.class ;;
  gradle) output=build/classes/java/main/example/Example.class ;;
  sbt) output=target/scala-2.13/classes/example/Example.class ;;
  *) exit 64 ;;
esac
mkdir -p "${output%/*}"
: > "$output"
EOF
chmod 0555 "$maven_case_bin/fake-jvm-client"
for client in mvn gradle sbt; do
  ln -s fake-jvm-client "$maven_case_bin/$client"
done
for phase in cold warm; do
  maven_phase_root=$workdir/maven-$phase
  mkdir -p "$maven_phase_root"
  (
    cd "$maven_phase_root"
    PATH="$maven_case_bin:$PATH" KATCH_URL=https://katch.invalid JVM_EVENT_LOG="$maven_events" \
      /bin/sh "$maven_setup_script"
    PATH="$maven_case_bin:$PATH" KATCH_URL=https://katch.invalid JVM_EVENT_LOG="$maven_events" \
      /bin/sh "$maven_run_script"
    /bin/sh "$maven_assert_script"
  ) || fail "JVM $phase phase did not use a writable phase-local home and project root"
  for client in mvn gradle sbt; do
    [ -f "$maven_phase_root/client-home/$client.cache" ] ||
      fail "JVM $phase phase did not give $client a writable phase-local cache"
  done
done
[ "$(wc -l < "$maven_events" | tr -d ' ')" -eq 6 ] ||
  fail "JVM case did not invoke all three tools in both isolated phases"

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

composer_setup=$(jq -r '.setup' "$CASES/composer.yaml")
composer_run=$(jq -r '.run' "$CASES/composer.yaml")
printf '%s\n' "$composer_run" | grep -Fx 'composer config --global notify-on-install false' >/dev/null ||
  fail "Composer mirror case does not disable the out-of-scope install notification"
printf '%s\n' "$composer_run" | grep -Fx 'test "$(composer config --global notify-on-install)" = false' >/dev/null ||
  fail "Composer mirror case does not assert that install notifications are disabled"
printf '%s\n' "$composer_run" | grep -Fx 'composer install --prefer-dist --no-dev --no-interaction --no-progress --no-security-blocking' >/dev/null ||
  fail "Composer mirror case does not use the expected non-auditing install command"
if printf '%s\n' "$composer_run" | grep -E -- '(^|[[:space:]])--(no-)?audit([=[:space:]]|$)'; then
  fail "Composer mirror case passes an audit flag"
fi
if printf '%s\n%s\n' "$composer_setup" "$composer_run" | grep -E -- '("secure-http"[[:space:]]*:[[:space:]]*false|secure-http[[:space:]]+false|--disable-tls|COMPOSER_DISABLE_TLS)'; then
  fail "Composer mirror case disables HTTPS/TLS verification"
fi

homebrew_case=$CASES/homebrew.yaml
homebrew_required_upstreams=$(jq -c '.required_upstreams' "$homebrew_case")
[ "$homebrew_required_upstreams" = '["formulae.brew.sh","ghcr.io","pkg-containers.githubusercontent.com","github.com"]' ] ||
  fail "Homebrew required_upstreams does not include the exact registered GHCR CDN host"
homebrew_setup=$(jq -r '.setup' "$homebrew_case")
homebrew_run=$(jq -r '.run' "$homebrew_case")
homebrew_assert=$(jq -r '.assert' "$homebrew_case")
printf '%s\n' "$homebrew_setup" | grep -Fx 'mkdir -p results client-home' >/dev/null ||
  fail "Homebrew does not create a phase-local client home"
printf '%s\n' "$homebrew_run" | grep -Fx 'export HOME="$PWD/client-home"' >/dev/null ||
  fail "Homebrew does not use its phase-local client home"
printf '%s\n' "$homebrew_run" | grep -Fx 'export HOMEBREW_ARTIFACT_DOMAIN="${KATCH_URL%/}/registry/ghcr.io"' >/dev/null ||
  fail "Homebrew does not use the exact registry base route"
printf '%s\n' "$homebrew_run" | grep -Fx 'export HOMEBREW_ARTIFACT_DOMAIN_NO_FALLBACK=1' >/dev/null ||
  fail "Homebrew does not force artifact-domain no-fallback"
printf '%s\n' "$homebrew_run" | grep -Fx '    export HOMEBREW_NO_INSTALL_FROM_API=1' >/dev/null ||
  fail "Homebrew warm phase does not switch to the pinned core tap"
[ "$(printf '%s\n' "$homebrew_run" | grep -Fxc 'git ls-remote "$HOMEBREW_CORE_GIT_REMOTE" HEAD > results/core-head')" -eq 1 ] ||
  fail "Homebrew does not run one unconditional mirrored git ls-remote per phase"
[ "$(printf '%s\n' "$homebrew_run" | grep -Fxc 'brew list --versions jq > results/jq-list')" -eq 1 ] ||
  fail "Homebrew does not record the native installed formula version"
[ "$(printf '%s\n' "$homebrew_run" | grep -Fxc 'brew info --json=v2 jq > results/jq.json')" -eq 1 ] ||
  fail "Homebrew does not record installed formula metadata"
[ "$(printf '%s\n' "$homebrew_run" | grep -Fxc 'brew --cellar jq > results/jq-cellar')" -eq 1 ] ||
  fail "Homebrew does not record the formula Cellar path"
printf '%s\n' "$homebrew_assert" | grep -Fx "grep -Eqx 'jq [0-9]+([.][0-9]+)+([._-][A-Za-z0-9]+)*' results/jq-list" >/dev/null ||
  fail "Homebrew does not require one exact native formula version line"
printf '%s\n' "$homebrew_assert" | grep -Fx 'test -s "$(cat results/jq-cellar)/$jq_version/INSTALL_RECEIPT.json"' >/dev/null ||
  fail "Homebrew does not require a nonempty receipt for the listed Cellar version"
printf '%s\n' "$homebrew_assert" | grep -F '.installed | length > 0' >/dev/null ||
  fail "Homebrew no longer asserts the installed JSON entry"
homebrew_commands=$(jq -r '[.setup, .run, .assert] | join("\n")' "$homebrew_case")
if printf '%s\n' "$homebrew_commands" | grep -E '(brew --prefix jq|[/]bin[/]jq|jq-version)'; then
  fail "Homebrew mirror case executes the installed Bottle binary"
fi
if printf '%s\n' "$homebrew_commands" | grep -i -E 'https?://([^/]*[.])?ghcr[.]io([/:]|$)'; then
  fail "Homebrew mirror case contains a direct GHCR URL"
fi
if printf '%s\n' "$homebrew_commands" | grep -i -E -- '(^|[[:space:]])--insecure([=[:space:]]|$)|HOMEBREW_.*(NO_VERIFY|DISABLE.*(TLS|SSL|CHECKSUM|SIGNATURE))|(^|[[:space:]])(SSL_CERT_FILE|CURL_CA_BUNDLE)=/dev/null'; then
  fail "Homebrew mirror case disables TLS, checksum, or signature verification"
fi

homebrew_setup_script=$workdir/homebrew-setup.sh
homebrew_run_script=$workdir/homebrew-run.sh
homebrew_assert_script=$workdir/homebrew-assert.sh
printf '%s\n' "$homebrew_setup" > "$homebrew_setup_script"
printf '%s\n' "$homebrew_run" > "$homebrew_run_script"
printf '%s\n' "$homebrew_assert" > "$homebrew_assert_script"
homebrew_case_bin=$workdir/homebrew-case-bin
homebrew_prefix=$workdir/homebrew-prefix
homebrew_events=$workdir/homebrew-events.log
mkdir -p "$homebrew_case_bin" "$homebrew_prefix/bin"
: > "$homebrew_events"
cat > "$homebrew_case_bin/brew" <<'EOF'
#!/bin/sh
set -eu
printf 'brew|%s|%s|%s|%s|%s|%s|%s\n' "$KATCH_PHASE" "$HOME" "$HOMEBREW_API_DOMAIN" \
  "$HOMEBREW_ARTIFACT_DOMAIN" "$HOMEBREW_ARTIFACT_DOMAIN_NO_FALLBACK" \
  "${HOMEBREW_NO_INSTALL_FROM_API-unset}" "$*" >> "$HOMEBREW_EVENT_LOG"
case "$*" in
  'install jq') ;;
  'info --json=v2 jq') printf '%s\n' '{"formulae":[{"name":"jq","installed":[{"version":"1.8.2"}]}]}' ;;
  'list --versions jq') printf '%s\n' 'jq 1.8.2' ;;
  '--cellar jq') printf '%s\n' "$HOMEBREW_TEST_CELLAR/jq" ;;
  '--prefix jq') printf '%s\n' "$HOMEBREW_TEST_PREFIX" ;;
  *) exit 64 ;;
esac
EOF
cat > "$homebrew_case_bin/git" <<'EOF'
#!/bin/sh
set -eu
[ "$#" -eq 3 ] && [ "$1" = ls-remote ] && [ "$2" = "$HOMEBREW_CORE_GIT_REMOTE" ] && [ "$3" = HEAD ]
printf 'git|%s|%s|%s\n' "$KATCH_PHASE" "$HOME" "$*" >> "$HOMEBREW_EVENT_LOG"
printf '%s\n' '0123456789abcdef0123456789abcdef01234567\tHEAD'
EOF
cat > "$homebrew_prefix/bin/jq" <<'EOF'
#!/bin/sh
set -eu
printf '%s\n' 'installed Bottle binary was executed' >&2
exit 86
EOF
homebrew_cellar=$workdir/homebrew-cellar
mkdir -p "$homebrew_cellar/jq/1.8.2"
printf '%s\n' '{"source":{"path":"/home/linuxbrew/.linuxbrew/Homebrew/Library/Taps/homebrew/homebrew-core/Formula/j/jq.rb"}}' > "$homebrew_cellar/jq/1.8.2/INSTALL_RECEIPT.json"
chmod 0555 "$homebrew_case_bin"/* "$homebrew_prefix/bin/jq"
homebrew_phase_root=$workdir/homebrew-phase
mkdir -p "$homebrew_phase_root/cold" "$homebrew_phase_root/warm"
homebrew_phase_root=$(CDPATH= cd -- "$homebrew_phase_root" && pwd)
for phase in cold warm; do
  (
    cd "$homebrew_phase_root/$phase"
    PATH="$homebrew_case_bin:$PATH" KATCH_PHASE=$phase KATCH_URL=https://katch.test \
      HOMEBREW_EVENT_LOG="$homebrew_events" HOMEBREW_TEST_PREFIX="$homebrew_prefix" \
      HOMEBREW_TEST_CELLAR="$homebrew_cellar" /bin/sh "$homebrew_setup_script"
    PATH="$homebrew_case_bin:$PATH" KATCH_PHASE=$phase KATCH_URL=https://katch.test \
      HOMEBREW_EVENT_LOG="$homebrew_events" HOMEBREW_TEST_PREFIX="$homebrew_prefix" \
      HOMEBREW_TEST_CELLAR="$homebrew_cellar" /bin/sh "$homebrew_run_script"
    /bin/sh "$homebrew_assert_script"
  ) || fail "Homebrew $phase phase does not satisfy the isolated runtime contract"
done
[ "$(grep -c '^git|' "$homebrew_events")" -eq 2 ] ||
  fail "Homebrew does not execute mirrored git ls-remote in both phases"
grep -F "brew|cold|$homebrew_phase_root/cold/client-home|https://katch.test/formulae.brew.sh/api|https://katch.test/registry/ghcr.io|1|unset|install jq" "$homebrew_events" >/dev/null ||
  fail "Homebrew cold install does not use API mode and the exact Katch environment"
grep -F "brew|warm|$homebrew_phase_root/warm/client-home|https://katch.test/formulae.brew.sh/api|https://katch.test/registry/ghcr.io|1|1|install jq" "$homebrew_events" >/dev/null ||
  fail "Homebrew warm install does not use the pinned tap with a fresh HOME"

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

capture_katch_ip=172.17.0.1
capture_katch_port=38443
cat > "$workdir/ipv4-katch.log" <<EOF
123 connect(3, {sa_family=AF_INET, sin_addr=inet_addr("$capture_katch_ip"), sin_port=htons($capture_katch_port)}, 16) = 0
EOF
KATCH_PORT=$capture_katch_port "$ENTRYPOINT" --verify-capture "$workdir/ipv4-katch.log" "$capture_katch_ip" ||
  fail "exact IPv4 Katch connection on KATCH_PORT was rejected"

cat > "$workdir/ipv4-katch-probe.log" <<EOF
124 connect(3, {sa_family=AF_INET, sin_port=htons(0), sin_addr=inet_addr("$capture_katch_ip")}, 16) = 0
EOF
KATCH_PORT=$capture_katch_port "$ENTRYPOINT" --verify-capture "$workdir/ipv4-katch-probe.log" "$capture_katch_ip" ||
  fail "IPv4 JVM route probe to Katch port zero was rejected"

cat > "$workdir/mapped-katch.log" <<EOF
125 connect(3, {sa_family=AF_INET6, sin6_port=htons($capture_katch_port), sin6_flowinfo=htonl(0), inet_pton(AF_INET6, "::ffff:$capture_katch_ip", &sin6_addr), sin6_scope_id=0}, 28) = 0
EOF
KATCH_PORT=$capture_katch_port "$ENTRYPOINT" --verify-capture "$workdir/mapped-katch.log" "$capture_katch_ip" ||
  fail "exact IPv4-mapped Katch connection was rejected"

cat > "$workdir/mapped-katch-probe.log" <<EOF
126 connect(3, {sa_family=AF_INET6, sin6_flowinfo=htonl(0), inet_pton(AF_INET6, "::ffff:$capture_katch_ip", &sin6_addr), sin6_port=htons(0), sin6_scope_id=0}, 28) = 0
EOF
KATCH_PORT=$capture_katch_port "$ENTRYPOINT" --verify-capture "$workdir/mapped-katch-probe.log" "$capture_katch_ip" ||
  fail "IPv4-mapped JVM route probe to Katch port zero was rejected"

cat > "$workdir/katch-musl-route-probe-success.log" <<EOF
127 connect(3, {sa_family=AF_INET, sin_port=htons(65535), sin_addr=inet_addr("$capture_katch_ip")}, 16) = 0
128 connect(3, {sa_family=AF_INET6, sin6_port=htons(65535), inet_pton(AF_INET6, "::ffff:$capture_katch_ip", &sin6_addr), sin6_scope_id=0}, 28) = 0
EOF
KATCH_PORT=$capture_katch_port "$ENTRYPOINT" --verify-capture "$workdir/katch-musl-route-probe-success.log" "$capture_katch_ip" ||
  fail "successful musl route probe to exact IPv4 or IPv4-mapped Katch IP was rejected"

cat > "$workdir/katch-musl-route-probe-failure.log" <<EOF
129 connect(3, {sa_family=AF_INET, sin_port=htons(65535), sin_addr=inet_addr("$capture_katch_ip")}, 16) = -1 EPERM (Operation not permitted)
130 connect(3, {sa_family=AF_INET6, sin6_port=htons(65535), inet_pton(AF_INET6, "::ffff:$capture_katch_ip", &sin6_addr), sin6_scope_id=0}, 28) = -1 EINPROGRESS (Operation now in progress)
EOF
if KATCH_PORT=$capture_katch_port "$ENTRYPOINT" --verify-capture "$workdir/katch-musl-route-probe-failure.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
  fail "failed musl route probe to exact IPv4 or IPv4-mapped Katch IP was accepted"
fi
grep -F 'sa_family=AF_INET, sin_port=htons(65535)' "$workdir/out" >/dev/null ||
  fail "failed IPv4 musl route probe was not reported"
grep -F 'sa_family=AF_INET6, sin6_port=htons(65535)' "$workdir/out" >/dev/null ||
  fail "failed IPv4-mapped musl route probe was not reported"

cat > "$workdir/other-musl-route-probe.log" <<'EOF'
131 connect(3, {sa_family=AF_INET, sin_port=htons(65535), sin_addr=inet_addr("192.0.2.10")}, 16) = 0
132 connect(3, {sa_family=AF_INET6, sin6_port=htons(65535), inet_pton(AF_INET6, "::ffff:192.0.2.11", &sin6_addr), sin6_scope_id=0}, 28) = 0
EOF
if KATCH_PORT=$capture_katch_port "$ENTRYPOINT" --verify-capture "$workdir/other-musl-route-probe.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
  fail "musl route probe to a non-Katch IPv4 or IPv4-mapped destination was accepted"
fi
grep -F '192.0.2.10' "$workdir/out" >/dev/null ||
  fail "IPv4 musl route probe to another IP was not reported"
grep -F '::ffff:192.0.2.11' "$workdir/out" >/dev/null ||
  fail "IPv4-mapped musl route probe to another IP was not reported"

cat > "$workdir/loopback.log" <<'EOF'
127 connect(3, {sa_family=AF_INET, sin_port=htons(49152), sin_addr=inet_addr("127.0.0.1")}, 16) = 0
128 connect(3, {sa_family=AF_INET, sin_port=htons(49153), sin_addr=inet_addr("127.42.0.9")}, 16) = 0
129 connect(3, {sa_family=AF_INET6, sin6_port=htons(49154), inet_pton(AF_INET6, "::1", &sin6_addr)}, 28) = 0
130 connect(3, {sa_family=AF_INET6, inet_pton(AF_INET6, "::ffff:127.0.0.1", &sin6_addr), sin6_port=htons(49155)}, 28) = 0
EOF
KATCH_PORT=$capture_katch_port "$ENTRYPOINT" --verify-capture "$workdir/loopback.log" "$capture_katch_ip" ||
  fail "loopback connection was rejected"

cat > "$workdir/ipv4-other-port.log" <<EOF
131 connect(3, {sa_family=AF_INET, sin_port=htons(443), sin_addr=inet_addr("$capture_katch_ip")}, 16) = -1 ECONNREFUSED (Connection refused)
EOF
if KATCH_PORT=$capture_katch_port "$ENTRYPOINT" --verify-capture "$workdir/ipv4-other-port.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
  fail "IPv4 Katch connection to another port was accepted"
fi
grep -F 'sin_port=htons(443)' "$workdir/out" >/dev/null ||
  fail "IPv4 Katch connection to another port was not reported"

cat > "$workdir/katch-missing-port.log" <<EOF
132 connect(3, {sa_family=AF_INET, sin_addr=inet_addr("$capture_katch_ip")}, 16) = -1 EINVAL (Invalid argument)
EOF
if KATCH_PORT=$capture_katch_port "$ENTRYPOINT" --verify-capture "$workdir/katch-missing-port.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
  fail "Katch connection without a parsed IPv4 port was accepted as a route probe"
fi
grep -F "$capture_katch_ip" "$workdir/out" >/dev/null ||
  fail "Katch connection without a parsed IPv4 port was not reported"

cat > "$workdir/mapped-other-port.log" <<EOF
133 connect(3, {sa_family=AF_INET6, sin6_port=htons(443), sin6_flowinfo=htonl(0), inet_pton(AF_INET6, "::ffff:$capture_katch_ip", &sin6_addr), sin6_scope_id=0}, 28) = -1 ECONNREFUSED (Connection refused)
EOF
if KATCH_PORT=$capture_katch_port "$ENTRYPOINT" --verify-capture "$workdir/mapped-other-port.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
  fail "IPv4-mapped Katch connection to another port was accepted"
fi
grep -F 'sin6_port=htons(443)' "$workdir/out" >/dev/null ||
  fail "IPv4-mapped Katch connection to another port was not reported"

cat > "$workdir/other-port-zero.log" <<'EOF'
133 connect(3, {sa_family=AF_INET, sin_port=htons(0), sin_addr=inet_addr("192.0.2.10")}, 16) = 0
134 connect(3, {sa_family=AF_INET6, sin6_port=htons(0), inet_pton(AF_INET6, "::ffff:192.0.2.11", &sin6_addr)}, 28) = 0
EOF
if KATCH_PORT=$capture_katch_port "$ENTRYPOINT" --verify-capture "$workdir/other-port-zero.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
  fail "port-zero route probe to a non-Katch destination was accepted"
fi
grep -F '192.0.2.10' "$workdir/out" >/dev/null ||
  fail "IPv4 port-zero route probe to another IP was not reported"
grep -F '::ffff:192.0.2.11' "$workdir/out" >/dev/null ||
  fail "IPv4-mapped port-zero route probe to another IP was not reported"

cat > "$workdir/mapped-other-ip.log" <<EOF
135 connect(3, {sa_family=AF_INET6, sin6_port=htons($capture_katch_port), sin6_flowinfo=htonl(0), inet_pton(AF_INET6, "::ffff:172.17.0.2", &sin6_addr), sin6_scope_id=0}, 28) = -1 ECONNREFUSED (Connection refused)
EOF
if KATCH_PORT=$capture_katch_port "$ENTRYPOINT" --verify-capture "$workdir/mapped-other-ip.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
  fail "IPv4-mapped connection to another IP was accepted"
fi
grep -F '::ffff:172.17.0.2' "$workdir/out" >/dev/null ||
  fail "IPv4-mapped connection to another IP was not reported"

cat > "$workdir/ipv6-leak.log" <<EOF
136 connect(3, {sa_family=AF_INET6, sin6_port=htons($capture_katch_port), sin6_flowinfo=htonl(0), inet_pton(AF_INET6, "2001:db8::1", &sin6_addr), sin6_scope_id=0}, 28) = -1 ENETUNREACH (Network unreachable)
EOF
if KATCH_PORT=$capture_katch_port "$ENTRYPOINT" --verify-capture "$workdir/ipv6-leak.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
  fail "ordinary IPv6 connection was accepted"
fi
grep -F '2001:db8::1' "$workdir/out" >/dev/null || fail "ordinary IPv6 connection was not reported"

cat > "$workdir/tcpdump-allowed.log" <<EOF
12:00:00.000000 IP 192.0.2.20.50000 > $capture_katch_ip.$capture_katch_port: Flags [S]
12:00:00.000001 IP 192.0.2.20.50001 > 127.0.0.1.49152: Flags [S]
EOF
KATCH_PORT=$capture_katch_port "$ENTRYPOINT" --verify-capture "$workdir/tcpdump-allowed.log" "$capture_katch_ip" ||
  fail "deterministic tcpdump Katch or loopback record was rejected"
cat > "$workdir/tcpdump-leak.log" <<'EOF'
12:00:00.000002 IP 192.0.2.20.50002 > 198.51.100.10.443: Flags [S]
EOF
if KATCH_PORT=$capture_katch_port "$ENTRYPOINT" --verify-capture "$workdir/tcpdump-leak.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
  fail "deterministic tcpdump external connection was accepted"
fi
grep -F '198.51.100.10.443' "$workdir/out" >/dev/null ||
  fail "deterministic tcpdump external connection was not reported"

cat > "$workdir/dns-leak.log" <<'EOF'
137 sendto(3, "query", 5, MSG_NOSIGNAL, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr("8.8.8.8")}, 16) = -1 EACCES (Permission denied)
EOF
if KATCH_PORT=$capture_katch_port "$ENTRYPOINT" --verify-capture "$workdir/dns-leak.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
  fail "external DNS attempt was accepted"
fi
grep -q '8.8.8.8' "$workdir/out" || fail "external DNS attempt was not reported"

hosts_file=$workdir/hosts
cat > "$hosts_file" <<'EOF'
127.0.0.1 localhost
192.0.2.10 katch.invalid
::ffff:192.0.2.11 katch.invalid
::ffff:192.0.2.10 other.invalid katch.invalid.example
EOF
"$ENTRYPOINT" --install-host-mapping "$hosts_file" katch.invalid 192.0.2.10
[ "$(grep -Fxc '::ffff:192.0.2.10 katch.invalid' "$hosts_file")" -eq 1 ] ||
  fail "host mapping helper did not install the exact IPv4-mapped Katch address"
if grep -Eq '^::[[:space:]]+katch\.invalid([[:space:]]|$)' "$hosts_file"; then
  fail "host mapping helper installed a broad IPv6 Katch address"
fi
"$ENTRYPOINT" --install-host-mapping "$hosts_file" katch.invalid 192.0.2.10
[ "$(grep -Fxc '::ffff:192.0.2.10 katch.invalid' "$hosts_file")" -eq 1 ] ||
  fail "host mapping helper duplicated an existing exact mapping"
mkdir "$workdir/hosts-directory"
if "$ENTRYPOINT" --install-host-mapping "$workdir/hosts-directory" katch.invalid 192.0.2.10 >"$workdir/out" 2>&1; then
  fail "host mapping helper accepted a hosts target that cannot be updated"
fi

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
    mapping_check=0
    for argument do
      last=$argument
      case $argument in mapped=::ffff:*) mapping_check=1 ;; esac
    done
    if [ "$last" = /etc/hosts ]; then
      if [ "$mapping_check" -eq 1 ]; then
        [ -f "$HOST_MAPPING_STATE" ]
      else
        printf '%s\n' 172.18.0.2
      fi
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
  keytool)
    printf '%s\n' "keytool $*" >> "$ENTRYPOINT_EVENT_LOG"
    operation=
    ca_file=
    alias=
    while [ "$#" -gt 0 ]; do
      case $1 in
        -list) operation=list; shift ;;
        -importcert) operation=import; shift ;;
        -file) ca_file=$2; shift 2 ;;
        -alias) alias=$2; shift 2 ;;
        *) shift ;;
      esac
    done
    case $operation in
      list)
        [ -f "$KEYTOOL_ALIAS_STATE" ] && [ "$(cat "$KEYTOOL_ALIAS_STATE")" = "$alias" ]
        ;;
      import)
        [ "${KEYTOOL_IMPORT_FAIL:-0}" -eq 0 ] || {
          printf '%s\n' 'fake keytool import failure' >&2
          exit 42
        }
        [ -n "$ca_file" ] && [ -n "$alias" ] || exit 64
        cat "$ca_file" > "$KEYTOOL_CERT_LOG"
        printf '%s\n' "$alias" > "$KEYTOOL_ALIAS_STATE"
        ;;
      *) exit 64 ;;
    esac
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
  tee)
    mapping=$(cat)
    printf '%s\n' "hosts-write $mapping $*" >> "$ENTRYPOINT_EVENT_LOG"
    [ "${TEE_FAIL:-0}" -eq 0 ] || exit 1
    : > "$HOST_MAPPING_STATE"
    printf '%s\n' "$mapping"
    ;;
  runuser)
    printf '%s\n' "runuser $*" >> "$ENTRYPOINT_EVENT_LOG"
    printf '%s\n' "$*" >> "$RUNUSER_LOG"
    ;;
  *) exit 64 ;;
esac
EOF
chmod 0555 "$fake_bin/fake-command"
for command in awk id install iptables ip6tables iptables-save iptables-restore ip6tables-save ip6tables-restore strace runuser tee; do
  ln -s fake-command "$fake_bin/$command"
done
for command in cat cksum env mkdir; do
  ln -s "$(command -v "$command")" "$fake_bin/$command"
done

run_entrypoint() {
  artifacts=$1
  shift
  env PATH="$fake_bin" RUNUSER_LOG="$workdir/runuser.log" ENTRYPOINT_EVENT_LOG="$workdir/entrypoint-events.log" \
    HOST_MAPPING_STATE="$workdir/host-mapping.state" KEYTOOL_ALIAS_STATE="$workdir/keytool-alias.state" \
    KEYTOOL_CERT_LOG="$workdir/keytool-cert.log" \
    KATCH_HOST=katch.invalid KATCH_PORT=8080 KATCH_ARTIFACTS="$artifacts" \
    "$@" "$ENTRYPOINT"
}

rm -f "$workdir/host-mapping.state"
: > "$workdir/runuser.log"
: > "$workdir/entrypoint-events.log"
if run_entrypoint "$workdir/hosts-failure-artifacts" env TEE_FAIL=1 >"$workdir/out" 2>&1; then
  fail "entrypoint continued when the IPv4-mapped hosts entry could not be installed"
fi
if grep -E '^(iptables |ip6tables |runuser )' "$workdir/entrypoint-events.log"; then
  fail "entrypoint configured the firewall or started the client after hosts update failure"
fi

: > "$workdir/runuser.log"
: > "$workdir/entrypoint-events.log"
run_entrypoint "$workdir/default-artifacts" env -u KATCH_CLIENT_USER -u KATCH_CLIENT_CA_CERT
mapping_line=$(grep -n -F 'hosts-write ::ffff:172.18.0.2 katch.invalid -a /etc/hosts' "$workdir/entrypoint-events.log" | cut -d: -f1)
firewall_line=$(grep -n -m1 -E '^iptables (-F|-P|-A)' "$workdir/entrypoint-events.log" | cut -d: -f1)
user_drop_line=$(grep -n -F 'runuser -u client ' "$workdir/entrypoint-events.log" | cut -d: -f1)
[ -n "$mapping_line" ] || fail "entrypoint did not install the IPv4-mapped Katch hosts entry"
[ "$mapping_line" -lt "$firewall_line" ] || fail "entrypoint installed the Katch hosts mapping after firewall setup"
[ "$mapping_line" -lt "$user_drop_line" ] || fail "entrypoint installed the Katch hosts mapping after the client started"
grep -q '^-u client --preserve-environment -- env HOME=/home/client USER=client LOGNAME=client /bin/sh -eu -c ' "$workdir/runuser.log" ||
  fail "entrypoint did not retain the default client user and identity environment"
if grep -E '^(install|update-ca-certificates|update-ca-trust) ' "$workdir/entrypoint-events.log"; then
  fail "entrypoint changed system trust when no client CA was configured"
fi

: > "$workdir/runuser.log"
: > "$workdir/entrypoint-events.log"
run_entrypoint "$workdir/homebrew-artifacts" env -u KATCH_CLIENT_CA_CERT KATCH_CLIENT_USER=linuxbrew
if grep -F 'hosts-write ' "$workdir/entrypoint-events.log"; then
  fail "entrypoint duplicated an existing exact IPv4-mapped Katch hosts entry"
fi
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
if grep -F 'keytool ' "$workdir/entrypoint-events.log"; then
  fail "entrypoint invoked Java trust tooling when keytool was absent"
fi
ca_update_line=$(grep -n -F 'update-ca-certificates ' "$workdir/entrypoint-events.log" | cut -d: -f1)
firewall_line=$(grep -n -m1 -E '^iptables (-F|-P|-A)' "$workdir/entrypoint-events.log" | cut -d: -f1)
user_drop_line=$(grep -n -F 'runuser -u client ' "$workdir/entrypoint-events.log" | cut -d: -f1)
[ "$ca_update_line" -lt "$firewall_line" ] || fail "entrypoint installs the trusted CA after firewall setup"
[ "$ca_update_line" -lt "$user_drop_line" ] || fail "entrypoint installs the trusted CA after the user drop"

ln -s fake-command "$fake_bin/keytool"
rm -f "$workdir/keytool-alias.state" "$workdir/keytool-cert.log"
: > "$workdir/runuser.log"
: > "$workdir/entrypoint-events.log"
run_entrypoint "$workdir/java-ca-artifacts" env KATCH_CLIENT_CA_CERT="$trusted_ca"
expected_alias=katch-test-ca-$(cksum < "$trusted_ca" | awk '{ print $1 "-" $2 }')
grep -Fx "keytool -cacerts -storepass changeit -list -alias $expected_alias" "$workdir/entrypoint-events.log" >/dev/null ||
  fail "entrypoint did not inspect the deterministic Java cacerts alias"
grep -Fx "keytool -cacerts -storepass changeit -noprompt -trustcacerts -importcert -alias $expected_alias -file $trusted_ca" "$workdir/entrypoint-events.log" >/dev/null ||
  fail "entrypoint did not import the trusted CA with native Java trust verification"
cmp -s "$trusted_ca" "$workdir/keytool-cert.log" ||
  fail "entrypoint did not pass the exact trusted CA certificate to keytool"
ca_update_line=$(grep -n -F 'update-ca-certificates ' "$workdir/entrypoint-events.log" | cut -d: -f1)
java_import_line=$(grep -n -F 'keytool -cacerts -storepass changeit -noprompt -trustcacerts -importcert ' "$workdir/entrypoint-events.log" | cut -d: -f1)
firewall_line=$(grep -n -m1 -E '^iptables (-F|-P|-A)' "$workdir/entrypoint-events.log" | cut -d: -f1)
user_drop_line=$(grep -n -F 'runuser -u client ' "$workdir/entrypoint-events.log" | cut -d: -f1)
[ "$ca_update_line" -lt "$java_import_line" ] || fail "entrypoint imports Java trust before system trust"
[ "$java_import_line" -lt "$firewall_line" ] || fail "entrypoint imports Java trust after firewall setup"
[ "$java_import_line" -lt "$user_drop_line" ] || fail "entrypoint imports Java trust after the client started"

: > "$workdir/entrypoint-events.log"
run_entrypoint "$workdir/java-ca-rerun-artifacts" env KATCH_CLIENT_CA_CERT="$trusted_ca"
[ "$(grep -Fc "keytool -cacerts -storepass changeit -list -alias $expected_alias" "$workdir/entrypoint-events.log")" -eq 1 ] ||
  fail "entrypoint did not check the existing Java cacerts alias on rerun"
if grep -F 'keytool -cacerts -storepass changeit -noprompt -trustcacerts -importcert ' "$workdir/entrypoint-events.log"; then
  fail "entrypoint re-imported an existing Java cacerts alias"
fi

rm -f "$workdir/keytool-alias.state"
: > "$workdir/runuser.log"
: > "$workdir/entrypoint-events.log"
if run_entrypoint "$workdir/java-ca-failure-artifacts" env KATCH_CLIENT_CA_CERT="$trusted_ca" KEYTOOL_IMPORT_FAIL=1 >"$workdir/out" 2>&1; then
  fail "entrypoint continued when Java cacerts import failed"
fi
grep -F 'failed to install trusted client CA in Java cacerts' "$workdir/out" >/dev/null ||
  fail "entrypoint did not report the Java cacerts import failure"
if grep -E '^(iptables |ip6tables |runuser )' "$workdir/entrypoint-events.log"; then
  fail "entrypoint configured the firewall or started the client after Java cacerts import failure"
fi
rm "$fake_bin/keytool"
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
total=$(((count + 2) / 3))
printf '%s\n' "$((count + 1))" > "$METRIC_STATE"
printf 'katch_origin_requests_total{upstream="example.invalid"} %s\n' "$total"
EOF
chmod 0555 "$harness_bin/fake-runtime" "$harness_bin/curl"

harness_case=$workdir/harness-case.yaml
second_harness_case=$workdir/second-harness-case.yaml
printf '%s\n' '{"name":"ca-contract","image":"example/client:1","setup":"true","run":"true","assert":"true","required_upstreams":["example.invalid"]}' > "$harness_case"
printf '%s\n' '{"name":"shared-isolation","image":"example/client:1","setup":"true","run":"true","assert":"true","required_upstreams":["example.invalid"]}' > "$second_harness_case"
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
shared_dir=$(find "$workdir/harness-unset" -type d -name shared -print)
[ -n "$shared_dir" ] && [ "$(printf '%s\n' "$shared_dir" | wc -l | tr -d ' ')" -eq 1 ] ||
  fail "harness did not create exactly one per-case shared directory"
[ -n "$(find "$shared_dir" -prune -perm -0002 -print)" ] ||
  fail "harness shared directory is not writable by an unprivileged client"
[ "$(grep -Fc "ARG=type=bind,src=$shared_dir,dst=/shared" "$workdir/runtime.log")" -eq 2 ] ||
  fail "harness did not mount the same case shared directory read-write in both phases"
if grep -F "ARG=type=bind,src=$shared_dir,dst=/shared,readonly" "$workdir/runtime.log"; then
  fail "harness mounted the case shared directory read-only"
fi
[ "$(grep -Fc 'ARG=KATCH_SHARED=/shared' "$workdir/runtime.log")" -eq 2 ] ||
  fail "harness did not export the fixed shared path in both phases"
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

: > "$workdir/runtime.log"
printf '%s\n' 0 > "$workdir/metric-state"
env PATH="$harness_bin:$PATH" RUNTIME_LOG="$workdir/runtime.log" METRIC_STATE="$workdir/metric-state" \
  CONTAINER_RUNTIME=fake-runtime ARTIFACT_ROOT="$workdir/harness-two-cases" \
  KATCH_URL=https://katch.invalid KATCH_METRICS_URL=http://metrics.invalid \
  KATCH_HOST=katch.invalid KATCH_PORT=443 \
  "$HARNESS" "$harness_case" "$second_harness_case" >"$workdir/out" 2>&1
shared_mounts=$(grep -F 'ARG=type=bind,src=' "$workdir/runtime.log" | grep -F ',dst=/shared')
[ "$(printf '%s\n' "$shared_mounts" | wc -l | tr -d ' ')" -eq 4 ] ||
  fail "harness did not mount shared storage in both phases of both cases"
[ "$(printf '%s\n' "$shared_mounts" | sort -u | wc -l | tr -d ' ')" -eq 2 ] ||
  fail "harness did not isolate shared storage by case"
for case_name in ca-contract shared-isolation; do
  [ "$(printf '%s\n' "$shared_mounts" | grep -Fc "/$case_name/shared,dst=/shared")" -eq 2 ] ||
    fail "harness shared storage is not stable across phases for $case_name"
done
[ "$(grep -Fc 'ARG=KATCH_SHARED=/shared' "$workdir/runtime.log")" -eq 4 ] ||
  fail "harness did not export KATCH_SHARED for every case phase"

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
