#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../../.." && pwd)
HARNESS=$ROOT/e2e/package-mirrors/harness.sh
ENTRYPOINT=$ROOT/e2e/package-mirrors/client-entrypoint.sh
IMAGE_SMOKE=$ROOT/e2e/package-mirrors/images/client-smoke.sh
IMAGE_BUILD=$ROOT/e2e/package-mirrors/images/build.sh
IMAGE_CONTRACT=$ROOT/e2e/package-mirrors/images/contract.json
CASE_SCHEMA=$ROOT/e2e/package-mirrors/schema/case.schema.json
DOCKER_CLIENT_DOCKERFILE=$ROOT/e2e/package-mirrors/images/docker.Dockerfile
PODMAN_CLIENT_DOCKERFILE=$ROOT/e2e/package-mirrors/images/podman.Dockerfile
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
  type == "array" and length == 14 and
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
  ([.[] | select(.user == "root") | .cases[]] | sort) == (["apk", "apt-update-install-signature", "docker-registry-regression", "podman-registry-regression", "rpm-dnf-yum"] | sort)
' "$IMAGE_CONTRACT" >/dev/null || fail "invalid image contract"

expected_smoke=$(printf '%s' '{"alpine":{"apk":"2.14.9"},"composer":{"composer":"2.9.5"},"debian":{"apt":"2.6.1"},"docker":{"docker":"29.2.1"},"dotnet":{"dotnet":"8.0.414"},"go":{"go":"1.26.0"},"homebrew":{"brew":"4.6.20"},"jvm":{"gradle":"9.0.0","java":"21.0.8","maven":"3.9.11","sbt":"1.11.6"},"node":{"bun":"1.3.11","node":"22.20.0","npm":"11.12.1","pnpm":"11.9.0","yarn":"1.22.22","yarn-berry":"4.10.3"},"podman":{"podman":"5.6.2"},"python":{"pip":"25.2","poetry":"2.2.1","python":"3.13.7","uv":"0.8.17"},"ruby":{"bundler":"2.7.1","ruby":"3.4.5"},"rust":{"cargo":"1.89.0","rust":"1.89.0"},"rpm":{"dnf":"4.14.0","yum":"4.14.0"}}' | jq -Sc .)
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

[ "$(awk 'toupper($1) == "FROM" { print $2; exit }' "$DOCKER_CLIENT_DOCKERFILE")" = 'docker:29.2.1-dind@sha256:68f6d9ab84623d1116c5432a3b924a07ee09960e6129ca1cb03ef14010588cb4' ] ||
  fail "Docker client image does not use the approved official manifest digest"
[ "$(awk 'toupper($1) == "FROM" { print $2; exit }' "$PODMAN_CLIENT_DOCKERFILE")" = 'quay.io/podman/stable:v5.6.2@sha256:28c72e39a70b8a6e2b567efe1b34e53850ea77b4c7c1538e41fe5a138055566d' ] ||
  fail "Podman client image does not use the approved linux/amd64 digest"
if grep -n -E '(apk|apt-get|dnf)[[:space:]].*(docker|podman)' "$DOCKER_CLIENT_DOCKERFILE" "$PODMAN_CLIENT_DOCKERFILE"; then
  fail "registry client images install a second container client instead of using their pinned base"
fi

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

# APT's InRelease carries max-age=120, and a CDN Age can consume most of it before the
# index arrives, so a warm re-fetch is what the protocol asks for rather than a cache
# miss. The warm phase therefore restores the cold phase's signature-verified index and
# verifies it again, and only the .deb still crosses the network, where zero origin
# requests is a promise Katch can keep. What the case must never do is widen that
# sharing into the artifact cache, or skip the signature check on the restored index.
apt_case=$CASES/apt.yaml
apt_setup=$(jq -r '.setup' "$apt_case")
apt_run=$(jq -r '.run' "$apt_case")
apt_assert=$(jq -r '.assert' "$apt_case")
apt_run_script=$workdir/apt-run.sh
printf '%s\n' "$apt_run" > "$apt_run_script"
printf '%s\n' "$apt_setup" | grep -Fx '  test -d "$KATCH_SHARED/apt-lists"' >/dev/null ||
  fail "APT warm setup does not require the cold phase's shared index directory"
printf '%s\n' "$apt_setup" | grep -Fx '  cp -a "$KATCH_SHARED/apt-lists/." /var/lib/apt/lists/' >/dev/null ||
  fail "APT warm setup does not restore the shared index into the APT list directory"
printf '%s\n' "$apt_setup" | grep -Fx 'rm -rf /var/lib/apt/lists/*' >/dev/null ||
  fail "APT setup does not start each phase from an empty index directory"
[ "$(printf '%s\n' "$apt_run" | grep -c 'apt-get')" -eq 2 ] ||
  fail "APT run does not invoke apt-get exactly twice: one cold update and one install per phase"
# apt's output goes to a file rather than through tee: without pipefail a pipeline's
# status is tee's, so a failed install or update would not fail the run step.
printf '%s\n' "$apt_run" |
  grep -Fx '    apt-get -o Acquire::Retries=0 update > /tmp/apt-update.log 2>&1 || { cat /tmp/apt-update.log; exit 1; }' >/dev/null ||
  fail "APT run does not refresh indexes with retries disabled in the cold phase, failing when apt fails"
printf '%s\n' "$apt_run" |
  grep -Fx 'apt-get -o Acquire::Retries=0 install -y --no-install-recommends hello > /tmp/apt-install.log 2>&1 || { cat /tmp/apt-install.log; exit 1; }' >/dev/null ||
  fail "APT run does not install the package with retries disabled in both phases, keeping apt's own transfer log"
if printf '%s\n' "$apt_run" | grep -F '| tee '; then
  fail "APT run pipes apt through tee, which hides apt's exit status"
fi
apt_gpgv_line=$(grep -n -F 'gpgv --keyring /usr/share/keyrings/debian-archive-keyring.gpg' "$apt_run_script" | cut -d: -f1)
apt_share_line=$(grep -n -F 'cp -a {} "$KATCH_SHARED/apt-lists/"' "$apt_run_script" | cut -d: -f1)
apt_esac_line=$(grep -n -Fx 'esac' "$apt_run_script" | head -1 | cut -d: -f1)
[ "$(printf '%s\n' "$apt_gpgv_line" | wc -l | tr -d ' ')" -eq 1 ] && [ -n "$apt_gpgv_line" ] ||
  fail "APT run does not verify the index signature exactly once"
[ -n "$apt_share_line" ] || fail "APT cold run does not publish the index for the warm phase"
[ -n "$apt_esac_line" ] && [ "$apt_esac_line" -lt "$apt_gpgv_line" ] ||
  fail "APT run verifies the index signature inside a phase branch instead of in both phases"
[ "$apt_gpgv_line" -lt "$apt_share_line" ] ||
  fail "APT cold run publishes the index before verifying its signature"
printf '%s\n' "$apt_run" | grep -F "\\( -name '*_InRelease' -o -name '*_Packages*' \\)" >/dev/null ||
  fail "APT cold run shares more than the signed index and its package lists"
printf '%s\n' "$apt_assert" | grep -Fx '[ -z "$(find "$KATCH_SHARED" -name '"'"'*.deb'"'"' -print -quit)" ]' >/dev/null ||
  fail "APT assertions do not prove the artifact cache stayed out of the shared directory"
# The warm phase installs the only package that crosses the network, so both phases have
# to show that the install really transferred it. dpkg's state cannot say that (it does not
# distinguish a fresh install from a package the image already carried) and the archive
# cache cannot either (docker-clean deletes the .deb straight after dpkg runs), so the
# evidence is apt's own Get line, backed by a setup precondition that hello is absent.
printf '%s\n' "$apt_assert" | grep -Fx "grep -Eq '^Get:[0-9]+ .*hello' /tmp/apt-install.log" >/dev/null ||
  fail "APT assertions do not prove the phase downloaded the package through Katch"
printf '%s\n' "$apt_assert" | grep -Fx "if grep -q 'is already the newest version' /tmp/apt-install.log; then" >/dev/null ||
  fail "APT assertions accept an install that had nothing to do"
printf '%s\n' "$apt_setup" |
  grep -Fx "if dpkg-query -W -f='\${Status}\\n' hello 2>/dev/null | grep -qx 'install ok installed'; then" >/dev/null ||
  fail "APT setup does not require each phase to start without the package installed"
# set -e does not apply to a pipeline that begins with !, so a negated check that is not the
# script's last command can never fail. The APT scripts write those checks as if/exit; the
# same rule holds for every case, so the ban is enforced across the whole matrix rather
# than per case — a trailing ! works today only because nothing was appended after it.
for case_yaml in "$CASES"/*.yaml; do
  case_name=$(jq -r '.name' "$case_yaml")
  for case_script_field in setup run assert; do
    case_script=$(jq -r ".$case_script_field" "$case_yaml")
    if printf '%s\n' "$case_script" | grep -E '^[[:space:]]*! '; then
      fail "$case_name negates a pipeline with ! in $case_script_field, which set -e ignores"
    fi
  done
done
# The index the warm phase verified and installed from must be the one the cold phase
# published after verifying it. Whether the warm phase refreshes indexes at all is a
# property of the run script, pinned by the apt-get count above; no runtime file state
# can show it, because apt stamps each list with the origin's Last-Modified, so a refetch
# of an unchanged InRelease leaves both its bytes and its timestamp as the restore did.
printf '%s\n' "$apt_assert" | grep -Fx '    cmp "${shared}" "${inrelease}"' >/dev/null ||
  fail "APT warm assertions do not compare the used index against the published one"
if printf '%s\n' "$apt_assert" | grep -F 'stat -c %Y'; then
  fail "APT warm assertions claim a timestamp comparison can detect a refresh; apt stamps lists with Last-Modified"
fi
if printf '%s\n' "$apt_assert" | grep -F 'test ! -s /tmp/apt-update.log'; then
  fail "APT warm assertions lean on a log the warm phase truncates itself, which cannot fail"
fi
apt_case_commands=$(jq -r '[.setup, .run, .assert] | join("\n")' "$apt_case")
if printf '%s\n' "$apt_case_commands" |
  grep -i -E '(allow-unauthenticated|AllowUnauthenticated|trusted=yes|Dir::Cache|/var/cache/apt|--force-yes|https?://deb[.]debian[.]org)'; then
  fail "APT mirror case disables signature checks, redirects the artifact cache, or uses a public Debian URL"
fi
if printf '%s\n' "$apt_case_commands" | grep -E 'KATCH_SHARED[^ ]*(archives|[.]deb)'; then
  fail "APT mirror case shares downloaded packages instead of only the signed index"
fi

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

npm_case=$CASES/npm.yaml
npm_run=$(jq -r '.run' "$npm_case")
npm_commands=$(jq -r '[.setup, .run, .assert] | join("\n")' "$npm_case")
ca_guard_line=$(printf '%s\n' "$npm_run" | awk '$0 == "[ \"${KATCH_CLIENT_CA_CERT:-}\" = /run/katch-test-ca.crt ] || { echo '\''npm-family requires mounted test CA at /run/katch-test-ca.crt'\'' >&2; exit 69; }" { print NR }')
node_extra_line=$(printf '%s\n' "$npm_run" | awk '$0 == "export NODE_EXTRA_CA_CERTS=\"$KATCH_CLIENT_CA_CERT\"" { print NR }')
[ -n "$ca_guard_line" ] || fail "npm mirror case does not require the exact mounted test CA path"
[ -n "$node_extra_line" ] || fail "npm mirror case does not add the mounted test CA to Node trust"
[ "$ca_guard_line" -lt "$node_extra_line" ] || fail "npm mirror case configures Node trust before requiring the exact mounted test CA"
printf '%s\n' "$npm_run" | grep -Fx '[ -f "$KATCH_CLIENT_CA_CERT" ] && [ -r "$KATCH_CLIENT_CA_CERT" ] || { echo '\''npm-family test CA is not a readable regular file'\'' >&2; exit 69; }' >/dev/null ||
  fail "npm mirror case does not require the mounted test CA to be a readable regular file"
[ "$(printf '%s\n' "$npm_run" | grep -Fxc 'export NODE_EXTRA_CA_CERTS="$KATCH_CLIENT_CA_CERT"')" -eq 1 ] ||
  fail "npm mirror case does not configure the exact Node extra CA once"
if printf '%s\n' "$npm_commands" | grep -i -E -- 'strict[-_]?ssl([=[:space:]]+)false|NODE_TLS_REJECT_UNAUTHORIZED[[:space:]]*=[[:space:]]*0|NODE_EXTRA_CA_CERTS[[:space:]]*=[[:space:]]*([^[:space:]]*[*]|/dev/null)|(^|[[:space:]])--?insecure([=[:space:]]|$)|trust[-_]?all|(^|[[:space:]])(ca|cafile)([=[:space:]]+)(/dev/null|[*])'; then
  fail "npm mirror case bypasses TLS verification or broadens CA trust"
fi
for case_file in "$CASES"/*.yaml; do
  [ "$case_file" = "$npm_case" ] && continue
  if grep -F 'NODE_EXTRA_CA_CERTS' "$case_file"; then
    fail "non-npm mirror case configures Node-specific CA trust: $case_file"
  fi
done
if grep -F 'NODE_EXTRA_CA_CERTS' "$HARNESS" "$ENTRYPOINT"; then
  fail "shared package mirror harness configures Node-specific CA trust"
fi
printf '%s\n' "$npm_run" | grep -Fx '(cd npm && npm install --ignore-scripts --no-audit --no-update-notifier --registry="$registry" --replace-registry-host=always)' >/dev/null ||
  fail "npm mirror case does not disable audit and the npm update notifier"
printf '%s\n' "$npm_run" | grep -Fx '(cd pnpm && PNPM_CONFIG_UPDATE_NOTIFIER=false pnpm install --ignore-scripts --registry="$registry")' >/dev/null ||
  fail "npm mirror case does not disable the pnpm update notifier"
printf '%s\n' "$npm_run" | grep -Fx '(cd yarn-classic && yarn install --ignore-scripts --registry "$registry")' >/dev/null ||
  fail "npm mirror case does not run Yarn Classic through the configured registry"
printf '%s\n' "$npm_run" | grep -Fx '(cd yarn-berry && yarn-berry config set npmRegistryServer "$registry" && yarn-berry config set unsafeHttpWhitelist --json "[\"$KATCH_HOST\"]" && yarn-berry install --mode=skip-build)' >/dev/null ||
  fail "npm mirror case does not allow HTTP only for the configured Katch host before Yarn Berry install"
printf '%s\n' "$npm_run" | grep -Fx '(cd bun && bun install --ignore-scripts --registry "$registry")' >/dev/null ||
  fail "npm mirror case does not run Bun through the configured registry"

pypi_case=$CASES/pypi.yaml
pypi_run=$(jq -r '.run' "$pypi_case")
pypi_assert=$(jq -r '.assert' "$pypi_case")
pypi_commands=$(jq -r '[.setup, .run, .assert] | join("\n")' "$pypi_case")
pypi_ca_guard='[ "${KATCH_CLIENT_CA_CERT:-}" = /run/katch-test-ca.crt ] || { echo '\''pypi-family requires mounted test CA at /run/katch-test-ca.crt'\'' >&2; exit 69; }'
pypi_ca_readable='[ -f "$KATCH_CLIENT_CA_CERT" ] && [ -r "$KATCH_CLIENT_CA_CERT" ] || { echo '\''pypi-family test CA is not a readable regular file'\'' >&2; exit 69; }'
printf '%s\n' "$pypi_run" | grep -Fx "$pypi_ca_guard" >/dev/null ||
  fail "PyPI mirror case does not require the exact mounted test CA path"
printf '%s\n' "$pypi_run" | grep -Fx "$pypi_ca_readable" >/dev/null ||
  fail "PyPI mirror case does not require the mounted test CA to be a readable regular file"
printf '%s\n' "$pypi_run" | grep -Fx 'pip/.venv/bin/python -m pip install --disable-pip-version-check --no-cache-dir --cert "$KATCH_CLIENT_CA_CERT" --index-url "$index" idna==3.10' >/dev/null ||
  fail "pip mirror case does not validate TLS with the exact mounted test CA"
printf '%s\n' "$pypi_run" | grep -Fx 'uv --native-tls pip install --no-cache --python uv/.venv/bin/python --index-url "$index" idna==3.10' >/dev/null ||
  fail "uv mirror case does not use native TLS trust with the global option in the correct position"
printf '%s\n' "$pypi_run" | grep -Fx 'poetry source add --priority=primary katch "$index"' >/dev/null ||
  fail "Poetry mirror case does not use the katch source name"
printf '%s\n' "$pypi_run" | grep -Fx 'poetry config certificates.katch.cert "$KATCH_CLIENT_CA_CERT"' >/dev/null ||
  fail "Poetry mirror case does not validate TLS with the exact mounted test CA"
[ "$(printf '%s\n' "$pypi_commands" | grep -Ec -- '(^|[[:space:]])--cert([=[:space:]]|$)')" -eq 1 ] ||
  fail "PyPI mirror case has an additional or missing pip certificate option"
[ "$(printf '%s\n' "$pypi_commands" | grep -Foc -- '--native-tls')" -eq 1 ] ||
  fail "PyPI mirror case has an additional or missing uv native TLS option"
[ "$(printf '%s\n' "$pypi_commands" | grep -Ec 'poetry config certificates[.][^[:space:]]+[.]cert([[:space:]]|$)')" -eq 1 ] ||
  fail "PyPI mirror case has an additional or missing Poetry certificate setting"
pypi_ca_guard_line=$(printf '%s\n' "$pypi_run" | awk -v expected="$pypi_ca_guard" '$0 == expected { print NR }')
pypi_pip_line=$(printf '%s\n' "$pypi_run" | awk '$0 == "pip/.venv/bin/python -m pip install --disable-pip-version-check --no-cache-dir --cert \"$KATCH_CLIENT_CA_CERT\" --index-url \"$index\" idna==3.10" { print NR }')
pypi_uv_line=$(printf '%s\n' "$pypi_run" | awk '$0 == "uv --native-tls pip install --no-cache --python uv/.venv/bin/python --index-url \"$index\" idna==3.10" { print NR }')
pypi_poetry_cert_line=$(printf '%s\n' "$pypi_run" | awk '$0 == "poetry config certificates.katch.cert \"$KATCH_CLIENT_CA_CERT\"" { print NR }')
pypi_poetry_install_line=$(printf '%s\n' "$pypi_run" | awk '$0 == "poetry install --no-interaction --no-root)" { print NR }')
[ "$pypi_ca_guard_line" -lt "$pypi_pip_line" ] && [ "$pypi_ca_guard_line" -lt "$pypi_uv_line" ] && [ "$pypi_ca_guard_line" -lt "$pypi_poetry_cert_line" ] ||
  fail "PyPI mirror case configures a client before requiring the exact mounted test CA"
[ "$pypi_poetry_cert_line" -lt "$pypi_poetry_install_line" ] ||
  fail "Poetry mirror case installs before configuring its exact source certificate"
printf '%s\n' "$pypi_assert" | grep -Fx '[ ! -e poetry/poetry.toml ] || { echo '\''generated Poetry project metadata contains phase-local certificate configuration'\'' >&2; exit 1; }' >/dev/null ||
  fail "Poetry mirror case permits certificate configuration in project-local poetry.toml"
printf '%s\n' "$pypi_assert" | grep -F 'poetry/poetry.lock poetry/pyproject.toml' >/dev/null ||
  fail "Poetry mirror case does not check generated project metadata for a leaked CA path"
if printf '%s\n' "$pypi_commands" | grep -i -E -- '(^|[[:space:]])--trusted-host([=[:space:]]|$)|(^|[[:space:]])--allow-insecure-host([=[:space:]]|$)|(^|[[:space:]])(PIP_TRUSTED_HOST|PIP_CERT|UV_INSECURE_HOST|UV_ALLOW_INSECURE_HOST|PYTHONHTTPSVERIFY|SSL_CERT_FILE|SSL_CERT_DIR|CURL_CA_BUNDLE|REQUESTS_CA_BUNDLE)=|POETRY_CERTIFICATES_[^=[:space:]]+_CERT[[:space:]]*=[[:space:]]*(false|/dev/null|[^[:space:]]*[?*][^[:space:]]*)|certificates[.][^[:space:]]+[.]cert[[:space:]]+(false|/dev/null|[^[:space:]]*[?*][^[:space:]]*)|(^|[[:space:]])(--?insecure|--disable-tls|--no-verify|--no-ssl-verify)([=[:space:]]|$)|TLS[^=[:space:]]*[[:space:]]*=[[:space:]]*(0|false)|(^|[=[:space:]"'\''])(/dev/null|[*])([[:space:]"'\'']|$)|trust[-_]?all|poetry[[:space:]]+config[[:space:]]+--local[[:space:]]+certificates[.]'; then
  fail "PyPI mirror case disables TLS verification or broadens CA trust"
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
printf '%s\n' "$homebrew_run" | grep -Fx 'export HOMEBREW_NO_INSTALL_FROM_API=1' >/dev/null ||
  fail "Homebrew does not disable API installs before invoking brew ruby"
printf '%s\n' "$homebrew_run" | grep -F 'require "api/internal"' >/dev/null ||
  fail "Homebrew cold phase does not load the native internal API"
printf '%s\n' "$homebrew_run" | grep -F 'Homebrew::API.fetch_json_api_file(Homebrew::API::Internal.formula_endpoint)' >/dev/null ||
  fail "Homebrew cold phase does not fetch the system-specific formula JWS through the native API helper"
printf '%s\n' "$homebrew_run" | grep -F 'entry = data.fetch("formulae").fetch("jq")' >/dev/null ||
  fail "Homebrew cold phase does not select jq from the internal formula Hash"
printf '%s\n' "$homebrew_run" | grep -F 'entry.is_a?(Hash)' >/dev/null ||
  fail "Homebrew cold phase does not require jq formula metadata to be a Hash"
printf '%s\n' "$homebrew_run" | grep -F 'entry.fetch("stable_version")' >/dev/null ||
  fail "Homebrew cold phase does not extract stable_version"
printf '%s\n' "$homebrew_run" | grep -F 'entry.fetch("stable_checksum")' >/dev/null ||
  fail "Homebrew cold phase does not extract stable_checksum"
printf '%s\n' "$homebrew_run" | grep -F 'entry.fetch("bottle_checksum")' >/dev/null ||
  fail "Homebrew cold phase does not extract bottle_checksum"
printf '%s\n' "$homebrew_run" | grep -F 'JSON.generate({"name" => "jq", "version" => version, "source_sha256" => source_sha256, "bottle_sha256" => bottle_sha256})' >/dev/null ||
  fail "Homebrew cold phase does not emit compact jq version and checksum evidence"
printf '%s\n' "$homebrew_run" | grep -F '> results/api-jq.json' >/dev/null ||
  fail "Homebrew cold phase does not preserve formula API evidence"
[ "$(printf '%s\n' "$homebrew_run" | grep -Fxc 'export HOMEBREW_NO_INSTALL_FROM_API=1')" -eq 1 ] ||
  fail "Homebrew does not keep both installs and the API probe in pinned tap mode"
if printf '%s\n' "$homebrew_run" | grep -F 'unset HOMEBREW_NO_INSTALL_FROM_API'; then
  fail "Homebrew enables global API preloading before the cold probe"
fi
[ "$(printf '%s\n' "$homebrew_run" | grep -Fc 'brew info --json=v2 jq')" -eq 1 ] ||
  fail "Homebrew invokes brew info outside the single installed tap metadata check"
[ "$(printf '%s\n' "$homebrew_run" | grep -Fxc 'brew info --json=v2 jq > results/jq.json')" -eq 1 ] ||
  fail "Homebrew does not limit brew info to installed tap metadata"
[ "$(printf '%s\n' "$homebrew_run" | grep -Fxc '    git ls-remote "$HOMEBREW_CORE_GIT_REMOTE" HEAD > results/core-head')" -eq 1 ] ||
  fail "Homebrew does not run one cold-only mirrored git ls-remote"
[ "$(printf '%s\n' "$homebrew_run" | grep -Fxc 'brew list --versions jq > results/jq-list')" -eq 1 ] ||
  fail "Homebrew does not record the native installed formula version"
[ "$(printf '%s\n' "$homebrew_run" | grep -Fxc 'brew --cellar jq > results/jq-cellar')" -eq 1 ] ||
  fail "Homebrew does not record the formula Cellar path"
printf '%s\n' "$homebrew_assert" | grep -Fx "grep -Eqx 'jq [0-9]+([.][0-9]+)+([._-][A-Za-z0-9]+)*' results/jq-list" >/dev/null ||
  fail "Homebrew does not require one exact native formula version line"
printf '%s\n' "$homebrew_assert" | grep -Fx 'portable_ruby="$(brew --repository)/Library/Homebrew/vendor/portable-ruby/current/bin/ruby"' >/dev/null ||
  fail "Homebrew does not resolve portable Ruby from the active brew repository"
printf '%s\n' "$homebrew_assert" | grep -Fx 'test -x "$portable_ruby"' >/dev/null ||
  fail "Homebrew does not require its vendored portable Ruby to be executable"
[ "$(printf '%s\n' "$homebrew_assert" | grep -Fc '"$portable_ruby" -rjson -e ')" -eq 3 ] ||
  fail "Homebrew does not validate API, installed metadata, and receipt with portable Ruby stdlib JSON"
printf '%s\n' "$homebrew_assert" | grep -Fx '[ "$(cat results/jq-list)" = "jq $jq_version" ]' >/dev/null ||
  fail "Homebrew does not compare installed JSON against the exact native version line"
printf '%s\n' "$homebrew_assert" | grep -Fx 'receipt="$(cat results/jq-cellar)/$jq_version/INSTALL_RECEIPT.json"' >/dev/null ||
  fail "Homebrew does not locate the receipt from the dynamically extracted version"
printf '%s\n' "$homebrew_assert" | grep -Fx 'test -s "$receipt"' >/dev/null ||
  fail "Homebrew does not require a nonempty install receipt"
homebrew_commands=$(jq -r '[.setup, .run, .assert] | join("\n")' "$homebrew_case")
if printf '%s\n' "$homebrew_commands" | grep -F '1.8.2'; then
  fail "Homebrew mirror case hardcodes the mutable current jq version"
fi
if printf '%s\n' "$homebrew_commands" | grep -E 'KATCH_SHARED|HOMEBREW_(CACHE|API_AUTO_UPDATE_SECS)=.*shared'; then
  fail "Homebrew mirror case shares package, API, or tap state between phases"
fi
if printf '%s\n' "$homebrew_commands" | grep -E '(^|[;&|()][[:space:]]*)jq([[:space:]]|$)|(brew --prefix jq|[/]bin[/]jq|jq-version)'; then
  fail "Homebrew mirror case executes the installed Bottle binary"
fi
if printf '%s\n' "$homebrew_commands" | grep -i -E 'https?://([^/]*[.])?ghcr[.]io([/:]|$)'; then
  fail "Homebrew mirror case contains a direct GHCR URL"
fi
if printf '%s\n' "$homebrew_commands" | grep -i -E -- '(^|[^[:alnum:]_])(curl|wget)([^[:alnum:]_]|$)|verify_and_parse_jws|cached_jws_payload|openssl|homebrew-1|formula[.]jws[.]json|cask[.]jws[.]json|cask_endpoint|fetch_cask_api|internal/cask[.]'; then
  fail "Homebrew mirror case bypasses the native system-specific formula JWS verification path"
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
homebrew_system_jq=$(command -v jq)
mkdir -p "$homebrew_case_bin" "$homebrew_prefix/bin"
: > "$homebrew_events"
cat > "$homebrew_case_bin/brew" <<'EOF'
#!/bin/sh
set -eu
if [ "$*" = '--repository' ]; then
  printf '%s\n' "$HOMEBREW_TEST_REPOSITORY"
  exit 0
fi
printf 'brew|%s|%s|%s|%s|%s|%s|%s\n' "$KATCH_PHASE" "$HOME" "$HOMEBREW_API_DOMAIN" \
  "$HOMEBREW_ARTIFACT_DOMAIN" "$HOMEBREW_ARTIFACT_DOMAIN_NO_FALLBACK" \
  "${HOMEBREW_NO_INSTALL_FROM_API-unset}" "$*" >> "$HOMEBREW_EVENT_LOG"
[ "${HOMEBREW_NO_INSTALL_FROM_API-unset}" = 1 ] || exit 65
if [ "$#" -eq 4 ] && [ "$1" = ruby ] && [ "$2" = -rjson ] && [ "$3" = -e ]; then
  script=$4
  case $script in *'require "api/internal"'*) ;; *) exit 66 ;; esac
  case $script in *'Homebrew::API.fetch_json_api_file(Homebrew::API::Internal.formula_endpoint)'*) ;; *) exit 66 ;; esac
  case $script in *'entry = data.fetch("formulae").fetch("jq")'*'entry.is_a?(Hash)'*) ;; *) exit 66 ;; esac
  case $script in *'entry.fetch("stable_version")'*'entry.fetch("stable_checksum")'*'entry.fetch("bottle_checksum")'*) ;; *) exit 66 ;; esac
  case $script in *'JSON.generate({"name" => "jq", "version" => version, "source_sha256" => source_sha256, "bottle_sha256" => bottle_sha256})'*) ;; *) exit 66 ;; esac
  "$HOMEBREW_TEST_JQ" -ce '
    .formulae as $formulae |
    select(($formulae | type) == "object") |
    $formulae.jq as $entry |
    select(($entry | type) == "object") |
    $entry.stable_version as $version |
    $entry.stable_checksum as $source_sha256 |
    $entry.bottle_checksum as $bottle_sha256 |
    select([$version, $source_sha256, $bottle_sha256] | all(type == "string" and length > 0)) |
    {name: "jq", version: $version, source_sha256: $source_sha256, bottle_sha256: $bottle_sha256}
  ' "${HOMEBREW_TEST_FORMULA_API_JSON:?HOMEBREW_TEST_FORMULA_API_JSON is required}"
  exit 0
fi
case "$*" in
  'install jq') ;;
  'info --json=v2 jq')
    printf '%s\n' '{"formulae":[{"name":"jq","installed":[{"version":"9.7.6"}]}]}'
    ;;
  'list --versions jq') printf '%s\n' 'jq 9.7.6' ;;
  '--cellar jq') printf '%s\n' "$HOMEBREW_TEST_CELLAR/$KATCH_PHASE/jq" ;;
  '--prefix jq') printf '%s\n' "$HOMEBREW_TEST_PREFIX" ;;
  *) exit 64 ;;
esac
EOF
cat > "$homebrew_case_bin/git" <<'EOF'
#!/bin/sh
set -eu
[ "$#" -eq 3 ] && [ "$1" = ls-remote ] && [ "$2" = "$HOMEBREW_CORE_GIT_REMOTE" ] && [ "$3" = HEAD ]
printf 'git|%s|%s|%s\n' "$KATCH_PHASE" "$HOME" "$*" >> "$HOMEBREW_EVENT_LOG"
printf '%s\t%s\n' '0123456789abcdef0123456789abcdef01234567' HEAD
EOF
cat > "$homebrew_prefix/bin/jq" <<'EOF'
#!/bin/sh
set -eu
: > "${JQ_EXECUTION_LOG:?JQ_EXECUTION_LOG is required}"
printf '%s\n' 'installed Bottle binary was executed' >&2
exit 86
EOF
cat > "$homebrew_case_bin/ruby" <<'EOF'
#!/bin/sh
set -eu
: > "${PATH_RUBY_EXECUTION_LOG:?PATH_RUBY_EXECUTION_LOG is required}"
printf '%s\n' 'PATH Ruby was executed' >&2
exit 87
EOF
homebrew_formula_api_fixture=$workdir/homebrew-formula-api.json
cat > "$homebrew_formula_api_fixture" <<'EOF'
{"formulae":{"jq":{"stable_version":"10.0.0","stable_checksum":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","bottle_checksum":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}}
EOF
homebrew_jq_execution_log=$workdir/homebrew-jq-execution.log
homebrew_path_ruby_execution_log=$workdir/homebrew-path-ruby-execution.log
homebrew_portable_ruby_execution_log=$workdir/homebrew-portable-ruby-execution.log
homebrew_cellar=$workdir/homebrew-cellar
homebrew_repository=$workdir/homebrew-repository
homebrew_portable_ruby=$homebrew_repository/Library/Homebrew/vendor/portable-ruby/current/bin/ruby
mkdir -p "$(dirname "$homebrew_portable_ruby")"
for phase in cold warm; do
  mkdir -p "$homebrew_cellar/$phase/jq/9.7.6"
  printf '%s\n' '{"poured_from_bottle":true,"source":{"path":"/home/linuxbrew/.linuxbrew/Homebrew/Library/Taps/homebrew/homebrew-core/Formula/j/jq.rb"}}' > "$homebrew_cellar/$phase/jq/9.7.6/INSTALL_RECEIPT.json"
done
cat > "$homebrew_portable_ruby" <<'EOF'
#!/bin/sh
set -eu
[ "$#" -eq 4 ] && [ "$1" = -rjson ] && [ "$2" = -e ]
script=$3
file=$4
printf '%s\n' "$file" >> "${PORTABLE_RUBY_EXECUTION_LOG:?PORTABLE_RUBY_EXECUTION_LOG is required}"
case $file in
  results/api-jq.json)
    case $script in *'data.keys.sort == ["bottle_sha256", "name", "source_sha256", "version"]'*'data["name"] == "jq"'*'version.empty?'*'\A[0-9a-f]{64}\z'*) ;; *) exit 88 ;; esac
    "$HOMEBREW_TEST_JQ" -e '
      type == "object" and
      (keys == ["bottle_sha256", "name", "source_sha256", "version"]) and
      .name == "jq" and
      (.version | type == "string" and length > 0 and (contains("\n") | not)) and
      (.source_sha256 | type == "string" and test("^[0-9a-f]{64}$")) and
      (.bottle_sha256 | type == "string" and test("^[0-9a-f]{64}$"))
    ' "$file" >/dev/null
    ;;
  results/jq.json)
    case $script in *'entry["name"] == "jq"'*'installed.length == 1'*'version.empty?'*) ;; *) exit 88 ;; esac
    "$HOMEBREW_TEST_JQ" -er '
      .formulae as $formulae |
      select(($formulae | type) == "array") |
      [$formulae[] | select(.name == "jq")] as $matches |
      select(($matches | length) == 1) |
      $matches[0].installed as $installed |
      select(($installed | type) == "array" and ($installed | length) == 1) |
      $installed[0].version |
      select(type == "string" and length > 0)
    ' "$file"
    ;;
  */INSTALL_RECEIPT.json)
    case $script in *'poured_from_bottle'*'== true'*) ;; *) exit 88 ;; esac
    "$HOMEBREW_TEST_JQ" -e '.poured_from_bottle == true' "$file" >/dev/null
    ;;
  *) exit 88 ;;
esac
EOF
chmod 0555 "$homebrew_case_bin"/* "$homebrew_prefix/bin/jq" "$homebrew_portable_ruby"
homebrew_phase_root=$workdir/homebrew-phase
mkdir -p "$homebrew_phase_root/cold" "$homebrew_phase_root/warm"
homebrew_phase_root=$(CDPATH= cd -- "$homebrew_phase_root" && pwd)
for phase in cold warm; do
  (
    cd "$homebrew_phase_root/$phase"
    PATH="$homebrew_case_bin:$homebrew_prefix/bin:$PATH" KATCH_PHASE=$phase KATCH_URL=https://katch.test \
      HOMEBREW_EVENT_LOG="$homebrew_events" HOMEBREW_TEST_PREFIX="$homebrew_prefix" \
      HOMEBREW_TEST_CELLAR="$homebrew_cellar" HOMEBREW_TEST_REPOSITORY="$homebrew_repository" \
      HOMEBREW_TEST_JQ="$homebrew_system_jq" \
      JQ_EXECUTION_LOG="$homebrew_jq_execution_log" PATH_RUBY_EXECUTION_LOG="$homebrew_path_ruby_execution_log" \
      /bin/sh "$homebrew_setup_script"
    PATH="$homebrew_case_bin:$homebrew_prefix/bin:$PATH" KATCH_PHASE=$phase KATCH_URL=https://katch.test \
      HOMEBREW_EVENT_LOG="$homebrew_events" HOMEBREW_TEST_PREFIX="$homebrew_prefix" \
      HOMEBREW_TEST_CELLAR="$homebrew_cellar" HOMEBREW_TEST_REPOSITORY="$homebrew_repository" \
      HOMEBREW_TEST_JQ="$homebrew_system_jq" HOMEBREW_TEST_FORMULA_API_JSON="$homebrew_formula_api_fixture" \
      JQ_EXECUTION_LOG="$homebrew_jq_execution_log" PATH_RUBY_EXECUTION_LOG="$homebrew_path_ruby_execution_log" \
      /bin/sh "$homebrew_run_script"
    PATH="$homebrew_case_bin:$homebrew_prefix/bin:$PATH" KATCH_PHASE=$phase HOMEBREW_TEST_REPOSITORY="$homebrew_repository" \
      HOMEBREW_TEST_JQ="$homebrew_system_jq" \
      JQ_EXECUTION_LOG="$homebrew_jq_execution_log" PATH_RUBY_EXECUTION_LOG="$homebrew_path_ruby_execution_log" \
      PORTABLE_RUBY_EXECUTION_LOG="$homebrew_portable_ruby_execution_log" \
      /bin/sh "$homebrew_assert_script"
  ) || fail "Homebrew $phase phase does not satisfy the isolated runtime contract"
done
[ -s "$homebrew_phase_root/cold/results/api-jq.json" ] || fail "Homebrew cold phase did not preserve API JSON evidence"
[ -s "$homebrew_phase_root/cold/results/core-head" ] || fail "Homebrew cold phase did not preserve Tap Git evidence"
[ ! -e "$homebrew_phase_root/warm/results/api-jq.json" ] || fail "Homebrew warm phase fetched mutable API metadata"
[ ! -e "$homebrew_phase_root/warm/results/core-head" ] || fail "Homebrew warm phase queried mutable Tap Git state"
[ ! -e "$homebrew_jq_execution_log" ] || fail "Homebrew assertion executed the installed jq command"
[ ! -e "$homebrew_path_ruby_execution_log" ] || fail "Homebrew assertion executed Ruby from PATH"

homebrew_invalid_formula_events=$workdir/homebrew-invalid-formula-events.log
for invalid_homebrew_formula_api in \
  'not-json' \
  '{}' \
  '{"formulae":[]}' \
  '{"formulae":{}}' \
  '{"formulae":{"jq":[]}}' \
  '{"formulae":{"jq":{"stable_checksum":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","bottle_checksum":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}}' \
  '{"formulae":{"jq":{"stable_version":"","stable_checksum":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","bottle_checksum":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}}' \
  '{"formulae":{"jq":{"stable_version":"10.0.0","stable_checksum":7,"bottle_checksum":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}}' \
  '{"formulae":{"jq":{"stable_version":"10.0.0","stable_checksum":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","bottle_checksum":""}}}'; do
  printf '%s\n' "$invalid_homebrew_formula_api" > "$homebrew_formula_api_fixture"
  homebrew_invalid_formula_root=$workdir/homebrew-invalid-formula
  rm -rf "$homebrew_invalid_formula_root"
  mkdir -p "$homebrew_invalid_formula_root"
  if (cd "$homebrew_invalid_formula_root" && PATH="$homebrew_case_bin:$homebrew_prefix/bin:$PATH" KATCH_PHASE=cold KATCH_URL=https://katch.test \
    HOMEBREW_EVENT_LOG="$homebrew_invalid_formula_events" HOMEBREW_TEST_PREFIX="$homebrew_prefix" \
    HOMEBREW_TEST_CELLAR="$homebrew_cellar" HOMEBREW_TEST_REPOSITORY="$homebrew_repository" \
    HOMEBREW_TEST_JQ="$homebrew_system_jq" HOMEBREW_TEST_FORMULA_API_JSON="$homebrew_formula_api_fixture" \
    JQ_EXECUTION_LOG="$homebrew_jq_execution_log" PATH_RUBY_EXECUTION_LOG="$homebrew_path_ruby_execution_log" \
    /bin/sh "$homebrew_setup_script" && /bin/sh "$homebrew_run_script" >/dev/null 2>&1); then
    fail "Homebrew cold extraction accepted invalid internal formula metadata: $invalid_homebrew_formula_api"
  fi
done
cat > "$homebrew_formula_api_fixture" <<'EOF'
{"formulae":{"jq":{"stable_version":"10.0.0","stable_checksum":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","bottle_checksum":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}}
EOF

for invalid_homebrew_json in \
  'not-json' \
  '{"formulae":[{"name":"not-jq","installed":[{"version":"9.7.6"}]}]}' \
  '{"formulae":[{"name":"jq","installed":[]}]}' \
  '{"formulae":[{"name":"jq","installed":[{"version":""}]}]}' \
  '{"formulae":[{"name":"jq","installed":[{"version":"9.7.6"},{"version":"9.7.6"}]}]}' \
  '{"formulae":[{"name":"jq","installed":[{"version":"9.7.5"}]}]}' \
  '{"formulae":[{"name":"jq","installed":[{"version":"9.7.6"}]},{"name":"jq","installed":[{"version":"9.7.6"}]}]}'; do
  printf '%s\n' "$invalid_homebrew_json" > "$homebrew_phase_root/cold/results/jq.json"
  if (cd "$homebrew_phase_root/cold" && PATH="$homebrew_case_bin:$homebrew_prefix/bin:$PATH" KATCH_PHASE=cold \
    HOMEBREW_TEST_REPOSITORY="$homebrew_repository" HOMEBREW_TEST_JQ="$homebrew_system_jq" \
    JQ_EXECUTION_LOG="$homebrew_jq_execution_log" PATH_RUBY_EXECUTION_LOG="$homebrew_path_ruby_execution_log" \
    PORTABLE_RUBY_EXECUTION_LOG="$homebrew_portable_ruby_execution_log" /bin/sh "$homebrew_assert_script" >/dev/null 2>&1); then
    fail "Homebrew assertion accepted invalid installed formula metadata: $invalid_homebrew_json"
  fi
done
printf '%s\n' '{"formulae":[{"name":"jq","installed":[{"version":"9.7.6"}]}]}' > "$homebrew_phase_root/cold/results/jq.json"
for invalid_homebrew_api_json in \
  'not-json' \
  '{}' \
  '{"name":"jq","version":"10.0.0","source_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}' \
  '{"name":"not-jq","version":"10.0.0","source_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","bottle_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}' \
  '{"name":"jq","version":"","source_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","bottle_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}' \
  '{"name":"jq","version":"10.0.0","source_sha256":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","bottle_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}' \
  '{"name":"jq","version":"10.0.0","source_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","bottle_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}' \
  '{"name":"jq","version":"10.0.0","source_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","bottle_sha256":"gggggggggggggggggggggggggggggggggggggggggggggggggggggggggggggggg"}' \
  '{"name":"jq","version":"10.0.0","source_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","bottle_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","extra":true}' \
  '[{"name":"jq","version":"10.0.0","source_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","bottle_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]'; do
  printf '%s\n' "$invalid_homebrew_api_json" > "$homebrew_phase_root/cold/results/api-jq.json"
  if (cd "$homebrew_phase_root/cold" && PATH="$homebrew_case_bin:$homebrew_prefix/bin:$PATH" KATCH_PHASE=cold \
    HOMEBREW_TEST_REPOSITORY="$homebrew_repository" HOMEBREW_TEST_JQ="$homebrew_system_jq" \
    JQ_EXECUTION_LOG="$homebrew_jq_execution_log" PATH_RUBY_EXECUTION_LOG="$homebrew_path_ruby_execution_log" \
    PORTABLE_RUBY_EXECUTION_LOG="$homebrew_portable_ruby_execution_log" /bin/sh "$homebrew_assert_script" >/dev/null 2>&1); then
    fail "Homebrew assertion accepted invalid formula API evidence: $invalid_homebrew_api_json"
  fi
done
printf '%s\n' '{"name":"jq","version":"10.0.0","source_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","bottle_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}' > "$homebrew_phase_root/cold/results/api-jq.json"
printf '%s\n' '{"poured_from_bottle":false}' > "$homebrew_cellar/cold/jq/9.7.6/INSTALL_RECEIPT.json"
if (cd "$homebrew_phase_root/cold" && PATH="$homebrew_case_bin:$homebrew_prefix/bin:$PATH" KATCH_PHASE=cold \
  HOMEBREW_TEST_REPOSITORY="$homebrew_repository" HOMEBREW_TEST_JQ="$homebrew_system_jq" \
  JQ_EXECUTION_LOG="$homebrew_jq_execution_log" PATH_RUBY_EXECUTION_LOG="$homebrew_path_ruby_execution_log" \
  PORTABLE_RUBY_EXECUTION_LOG="$homebrew_portable_ruby_execution_log" /bin/sh "$homebrew_assert_script" >/dev/null 2>&1); then
  fail "Homebrew assertion accepted a non-bottle install receipt"
fi
[ ! -e "$homebrew_jq_execution_log" ] || fail "Homebrew assertion executed the installed jq command"
[ ! -e "$homebrew_path_ruby_execution_log" ] || fail "Homebrew assertion executed Ruby from PATH"
homebrew_api_ruby_script='require "api/internal"; data, = Homebrew::API.fetch_json_api_file(Homebrew::API::Internal.formula_endpoint); entry = data.fetch("formulae").fetch("jq"); raise "invalid jq formula metadata" unless entry.is_a?(Hash); version = entry.fetch("stable_version"); source_sha256 = entry.fetch("stable_checksum"); bottle_sha256 = entry.fetch("bottle_checksum"); raise "invalid jq formula metadata" unless [version, source_sha256, bottle_sha256].all? { |value| value.is_a?(String) && !value.empty? }; puts JSON.generate({"name" => "jq", "version" => version, "source_sha256" => source_sha256, "bottle_sha256" => bottle_sha256})'
expected_homebrew_events=$(printf '%s\n' \
  "brew|cold|$homebrew_phase_root/cold/client-home|https://katch.test/formulae.brew.sh/api|https://katch.test/registry/ghcr.io|1|1|ruby -rjson -e $homebrew_api_ruby_script" \
  "git|cold|$homebrew_phase_root/cold/client-home|ls-remote https://katch.test/github.com/Homebrew/homebrew-core.git HEAD" \
  "brew|cold|$homebrew_phase_root/cold/client-home|https://katch.test/formulae.brew.sh/api|https://katch.test/registry/ghcr.io|1|1|install jq" \
  "brew|cold|$homebrew_phase_root/cold/client-home|https://katch.test/formulae.brew.sh/api|https://katch.test/registry/ghcr.io|1|1|list --versions jq" \
  "brew|cold|$homebrew_phase_root/cold/client-home|https://katch.test/formulae.brew.sh/api|https://katch.test/registry/ghcr.io|1|1|info --json=v2 jq" \
  "brew|cold|$homebrew_phase_root/cold/client-home|https://katch.test/formulae.brew.sh/api|https://katch.test/registry/ghcr.io|1|1|--cellar jq" \
  "brew|warm|$homebrew_phase_root/warm/client-home|https://katch.test/formulae.brew.sh/api|https://katch.test/registry/ghcr.io|1|1|install jq" \
  "brew|warm|$homebrew_phase_root/warm/client-home|https://katch.test/formulae.brew.sh/api|https://katch.test/registry/ghcr.io|1|1|list --versions jq" \
  "brew|warm|$homebrew_phase_root/warm/client-home|https://katch.test/formulae.brew.sh/api|https://katch.test/registry/ghcr.io|1|1|info --json=v2 jq" \
  "brew|warm|$homebrew_phase_root/warm/client-home|https://katch.test/formulae.brew.sh/api|https://katch.test/registry/ghcr.io|1|1|--cellar jq")
[ "$(cat "$homebrew_events")" = "$expected_homebrew_events" ] ||
  fail "Homebrew cold/warm command sequence does not match the exact API, Tap, and Bottle contract"

git_regression_case=$CASES/registry-git-regression.yaml
git_regression_setup=$(jq -r '.setup' "$git_regression_case")
git_regression_run=$(jq -r '.run' "$git_regression_case")
git_regression_assert=$(jq -r '.assert' "$git_regression_case")
git_regression_commands=$(jq -r '[.setup, .run, .assert] | join("\n")' "$git_regression_case")
if printf '%s\n' "$git_regression_commands" | grep -E -- '(^|[[:space:]])--(depth|filter)(=|[[:space:]]|$)'; then
  fail "Git regression uses shallow or filtered requests that must remain passthrough"
fi
printf '%s\n' "$git_regression_run" | grep -Fx 'case "$KATCH_PHASE" in' >/dev/null ||
  fail "Git regression does not branch explicitly between cold and warm phases"
printf '%s\n' "$git_regression_run" | grep -Fx 'cold)' >/dev/null ||
  fail "Git regression has no explicit cold phase"
printf '%s\n' "$git_regression_run" | grep -Fx '  git clone "$repository_url" repository' >/dev/null ||
  fail "Git regression cold phase is not a fresh ordinary clone"
printf '%s\n' "$git_regression_run" | grep -Fx '  max_attempts=60' >/dev/null ||
  fail "Git regression local-mirror poll does not have the approved finite attempt bound"
printf '%s\n' "$git_regression_run" | grep -Fx '  poll_interval=5' >/dev/null ||
  fail "Git regression local-mirror poll does not have the approved interval"
printf '%s\n' "$git_regression_run" | grep -Fx '  poll_timeout=300' >/dev/null ||
  fail "Git regression local-mirror poll does not have a reasonable wall-clock timeout"
printf '%s\n' "$git_regression_run" | grep -Fx '  request_timeout=15' >/dev/null ||
  fail "Git regression ls-remote attempts do not have an individual timeout"
printf '%s\n' "$git_regression_run" | grep -Fx '  deadline=$(($(date +%s) + poll_timeout))' >/dev/null ||
  fail "Git regression local-mirror poll does not calculate a finite deadline"
printf '%s\n' "$git_regression_run" | grep -Fx '  while [ "$attempt" -le "$max_attempts" ] && [ "$(date +%s)" -lt "$deadline" ]; do' >/dev/null ||
  fail "Git regression local-mirror poll does not consume its finite bound"
printf '%s\n' "$git_regression_run" | grep -Fx '    attempt=$((attempt + 1))' >/dev/null ||
  fail "Git regression local-mirror poll does not advance its attempt counter"
printf '%s\n' "$git_regression_run" | grep -Fx '        sleep "$poll_interval"' >/dev/null ||
  fail "Git regression local-mirror poll does not apply its bounded interval"
printf '%s\n' "$git_regression_run" | grep -Fx '    poll_trace="results/cold-local-$attempt.trace"' >/dev/null ||
  fail "Git regression poll does not use a fresh trace file for every attempt"
printf '%s\n' "$git_regression_run" | grep -Fx '    if GIT_TRACE_CURL=1 timeout "$call_timeout" git ls-remote "$repository_url" HEAD > "$poll_refs" 2> "$poll_trace"; then' >/dev/null ||
  fail "Git regression cold phase does not use a bounded ls-remote through Katch with curl tracing"
printf '%s\n' "$git_regression_run" | grep -Fx "      if tr -d '\\r' < \"\$poll_trace\" | grep -Eiq '<= Recv header: X-Katch-Git: local\$'; then" >/dev/null ||
  fail "Git regression poll does not require the exact local response header case-insensitively"
printf '%s\n' "$git_regression_run" | grep -Fx '  if [ "$local_ready" != true ]; then' >/dev/null ||
  fail "Git regression does not fail when the mirror never becomes local"
printf '%s\n' "$git_regression_run" | grep -Fx '    echo "last trace: $last_poll_trace" >&2' >/dev/null ||
  fail "Git regression timeout does not identify its last trace diagnostic"
printf '%s\n' "$git_regression_run" | grep -Fx '    cat "$last_poll_trace" >&2 || true' >/dev/null ||
  fail "Git regression timeout does not emit its last trace diagnostic"
printf '%s\n' "$git_regression_run" | grep -Fx 'warm)' >/dev/null ||
  fail "Git regression has no explicit warm phase"
printf '%s\n' "$git_regression_run" | grep -Fx '  GIT_TRACE_CURL=1 git clone "$repository_url" repository > results/warm-clone.stdout 2> results/warm-clone.trace' >/dev/null ||
  fail "Git regression warm phase is not a fresh traced ordinary clone"
printf '%s\n' "$git_regression_assert" | grep -Fx "  tr -d '\\r' < results/warm-clone.trace | grep -Eiq '<= Recv header: X-Katch-Git: local\$'" >/dev/null ||
  fail "Git regression warm phase does not assert local Git evidence"
printf '%s\n' "$git_regression_setup" | grep -Fx 'test ! -e repository && test ! -e results' >/dev/null ||
  fail "Git regression setup does not require fresh phase files"
if printf '%s\n' "$git_regression_commands" | grep -F 'KATCH_SHARED'; then
  fail "Git regression shares checkout or client state between phases"
fi
if printf '%s\n' "$git_regression_commands" | grep -Ei '(^|[^[:alnum:]_-])(docker|dockerd|podman)([^[:alnum:]_-]|$)|NOT runtime verification|diagnostic only'; then
  fail "Git regression still contains Docker/Podman diagnostics"
fi

registry_upstreams='["ghcr.io","pkg-containers.githubusercontent.com"]'
for registry_client in docker podman; do
  registry_case=$CASES/$registry_client-registry-regression.yaml
  [ -f "$registry_case" ] || fail "missing real $registry_client registry regression case"
  [ "$(jq -r '.privileged' "$registry_case")" = true ] || fail "$registry_client registry case is not privileged"
  [ "$(jq -c '.required_upstreams' "$registry_case")" = "$registry_upstreams" ] ||
    fail "$registry_client registry case has the wrong required upstreams"
  registry_commands=$(jq -r '[.setup, .run, .assert] | join("\n")' "$registry_case")
  if [ "$registry_client" = podman ]; then
    native_pull='podman_client pull "$image_ref"'
  else
    native_pull='docker pull "$image_ref"'
  fi
  printf '%s\n' "$registry_commands" | grep -F "$native_pull" >/dev/null ||
    fail "$registry_client registry case does not perform a native pull"
  printf '%s\n' "$registry_commands" | grep -F './podinfo' >/dev/null ||
    fail "$registry_client registry case does not run the pulled podinfo binary"
  if printf '%s\n' "$registry_commands" | grep -Ei 'blocked:|diagnostic only|NOT runtime verification|--tls-verify=false|--tlsverify=false|insecure'; then
    fail "$registry_client registry case accepts blocked diagnostics or disables registry TLS"
  fi
  [ "$(printf '%s\n' "$registry_commands" | grep -Fc '^[a-f0-9]{64}$')" -eq 0 ] ||
    fail "$registry_client registry case accepts a digest without the sha256 algorithm"
  printf '%s\n' "$registry_commands" | grep -F "^sha256:[a-f0-9]{64}$" >/dev/null ||
    fail "$registry_client registry case does not require exact sha256 evidence"
  for resolver_contract in \
    'artifact_katch_ip=$(awk -v host="$KATCH_HOST"' \
    '"$KATCH_ARTIFACTS/hosts")' \
    'missing exact static Katch hosts evidence' \
    'test -s "$KATCH_ARTIFACTS/resolv.conf.before"' \
    'test -s "$KATCH_ARTIFACTS/hosts"' \
    'nameserver 127.0.0.1' \
    'options timeout:1 attempts:1' \
    'cmp -s "$KATCH_ARTIFACTS/resolv.conf.after" /etc/resolv.conf'; do
    printf '%s\n' "$registry_commands" | grep -F -- "$resolver_contract" >/dev/null ||
      fail "$registry_client registry case lacks resolver evidence contract: $resolver_contract"
  done
done

[ "$(jq -r '.allow_blocked_dns_probe' "$CASES/docker-registry-regression.yaml")" = true ] ||
  fail "Docker registry case does not explicitly allow its blocked DNS route probe"
[ "$(find "$CASES" -maxdepth 1 -type f -name '*.yaml' -exec jq -r 'select(has("allow_blocked_dns_probe")) | .name' {} +)" = docker-registry-regression ] ||
  fail "only the Docker registry case may declare the blocked DNS route probe allowance"
jq -e '.properties.allow_blocked_dns_probe.type == "boolean"' "$CASE_SCHEMA" >/dev/null ||
  fail "case schema does not define the blocked DNS route probe allowance as boolean"
[ "$(jq -r '.allow_musl_route_probe' "$CASES/apk.yaml")" = true ] ||
  fail "APK case does not explicitly allow its musl route probe"
[ "$(find "$CASES" -maxdepth 1 -type f -name '*.yaml' -exec jq -r 'select(has("allow_musl_route_probe")) | .name' {} +)" = apk ] ||
  fail "only the APK case may declare the musl route probe allowance"
jq -e '.properties.allow_musl_route_probe.type == "boolean"' "$CASE_SCHEMA" >/dev/null ||
  fail "case schema does not define the musl route probe allowance as boolean"

if awk '
  /^(iptables|ip6tables)[[:space:]]/ && /-j[[:space:]]+ACCEPT/ && /(^|[^0-9])53([^0-9]|$)/ { found = 1 }
  END { exit found ? 0 : 1 }
' "$ENTRYPOINT"; then
  fail "entrypoint allows DNS through the firewall"
fi
if awk '
  /^(iptables|ip6tables)[[:space:]]/ && /-j[[:space:]]+ACCEPT/ && /(^|[^0-9])65535([^0-9]|$)/ { found = 1 }
  END { exit found ? 0 : 1 }
' "$ENTRYPOINT"; then
  fail "entrypoint allows the musl route probe through the firewall"
fi

[ "$(find "$CASES" -maxdepth 1 -type f -name '*.yaml' -exec jq -r 'select(.privileged == true) | .name' {} + | LC_ALL=C sort)" = "$(printf '%s\n' docker-registry-regression podman-registry-regression)" ] ||
  fail "only Docker and Podman registry cases may opt into privileged containers"

docker_registry_commands=$(jq -r '[.setup, .run, .assert] | join("\n")' "$CASES/docker-registry-regression.yaml")
for contract in \
  'dockerd --host=unix:///run/katch-docker.sock' \
  '--storage-driver=vfs' \
  '--bridge=none' \
  '--iptables=false' \
  'docker info' \
  'ghcr.io/stefanprodan/podinfo:6.9.2' \
  'cmp -s "$KATCH_CLIENT_CA_CERT" "$registry_ca"' \
  'kill -TERM "$dockerd_pid"'; do
  printf '%s\n' "$docker_registry_commands" | grep -F -- "$contract" >/dev/null ||
    fail "Docker registry case lacks contract: $contract"
done
printf '%s\n' "$docker_registry_commands" | grep -F "[ \"\$(cat results/version)\" = '6.9.2' ]" >/dev/null ||
  fail "Docker registry case does not require podinfo 6.9.2"
printf '%s\n' "$docker_registry_commands" | grep -F 'docker run --rm --network none --dns 127.0.0.1 --entrypoint ./podinfo "$image_ref" --version' >/dev/null ||
  fail "Docker registry nested run does not combine network isolation with loopback DNS"

podman_registry_assert=$(jq -r '.assert' "$CASES/podman-registry-regression.yaml")
podman_registry_commands=$(jq -r '[.setup, .run, .assert] | join("\n")' "$CASES/podman-registry-regression.yaml")
for contract in \
  '--root "$PWD/podman-root"' \
  '--runroot "$PWD/podman-runroot"' \
  '--storage-driver=vfs' \
  'ghcr.io/stefanprodan/podinfo:6.9.1' \
  'cmp -s "$KATCH_CLIENT_CA_CERT" "$registry_ca"'; do
  printf '%s\n' "$podman_registry_commands" | grep -F -- "$contract" >/dev/null ||
    fail "Podman registry case lacks contract: $contract"
done
printf '%s\n' "$podman_registry_commands" | grep -F "[ \"\$(cat results/version)\" = '6.9.1' ]" >/dev/null ||
  fail "Podman registry case does not require podinfo 6.9.1"
printf '%s\n' "$podman_registry_commands" | grep -F 'podman_client run --rm --network none --entrypoint ./podinfo "$image_ref" --version' >/dev/null ||
  fail "Podman registry nested run does not retain network isolation without DNS options"
if printf '%s\n' "$podman_registry_assert" | grep -F 'image_ref' >/dev/null; then
  fail "Podman registry assert references run-local image_ref"
fi
printf '%s\n' "$podman_registry_assert" | grep -Fx 'expected_repo_name="${KATCH_HOST}:${KATCH_PORT}/ghcr.io/stefanprodan/podinfo"' >/dev/null ||
  fail "Podman registry assert does not reconstruct the exact expected repository name"
if printf '%s\n' "$podman_registry_commands" | grep -F -- '--dns' >/dev/null; then
  fail "Podman registry case configures DNS despite network mode none"
fi
for contract in \
  "--format '{{range .RepoDigests}}{{printf \"%s\\n\" .}}{{end}}'" \
  'LC_ALL=C sort > results/repo-digests' \
  'source_index_digest=' \
  'selected_manifest_digest=' \
  'assert_sha256 results/source-index-digest' \
  'assert_sha256 results/selected-manifest-digest' \
  '[ "$(cat results/source-index-digest)" != "$(cat results/selected-manifest-digest)" ]' \
  'grep -Fxc "$selected_manifest_reference" results/repo-digests' \
  'grep -Fxc "$source_index_reference" results/repo-digests'; do
  printf '%s\n' "$podman_registry_commands" | grep -F -- "$contract" >/dev/null ||
    fail "Podman registry case lacks multi-digest evidence contract: $contract"
done
if printf '%s\n' "$podman_registry_commands" | grep -F '{{index .RepoDigests 0}}' >/dev/null; then
  fail "Podman registry case still treats RepoDigests index zero as the selected manifest"
fi

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
if KATCH_PORT=$capture_katch_port \
  "$ENTRYPOINT" --verify-capture "$workdir/katch-musl-route-probe-success.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
  fail "default capture audit accepted valid-looking musl route probes"
fi
grep -F 'sa_family=AF_INET, sin_port=htons(65535)' "$workdir/out" >/dev/null ||
  fail "default capture audit did not report the IPv4 musl route probe"
grep -F 'sa_family=AF_INET6, sin6_port=htons(65535)' "$workdir/out" >/dev/null ||
  fail "default capture audit did not report the IPv4-mapped musl route probe"

KATCH_ALLOW_MUSL_ROUTE_PROBE=1 KATCH_PORT=$capture_katch_port \
  "$ENTRYPOINT" --verify-capture "$workdir/katch-musl-route-probe-success.log" "$capture_katch_ip" ||
  fail "explicit APK musl route probe allowance rejected exact successful probes"

cat > "$workdir/katch-musl-route-probe-failure.log" <<EOF
129 connect(3, {sa_family=AF_INET, sin_port=htons(65535), sin_addr=inet_addr("$capture_katch_ip")}, 16) = -1 EPERM (Operation not permitted)
130 connect(3, {sa_family=AF_INET6, sin6_port=htons(65535), inet_pton(AF_INET6, "::ffff:$capture_katch_ip", &sin6_addr), sin6_scope_id=0}, 28) = 1
EOF
if KATCH_ALLOW_MUSL_ROUTE_PROBE=1 KATCH_PORT=$capture_katch_port \
  "$ENTRYPOINT" --verify-capture "$workdir/katch-musl-route-probe-failure.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
  fail "explicit musl route probe allowance accepted a failed or nonzero result"
fi
grep -F 'sa_family=AF_INET, sin_port=htons(65535)' "$workdir/out" >/dev/null ||
  fail "failed IPv4 musl route probe was not reported"
grep -F 'sa_family=AF_INET6, sin6_port=htons(65535)' "$workdir/out" >/dev/null ||
  fail "failed IPv4-mapped musl route probe was not reported"

cat > "$workdir/other-musl-route-probe.log" <<'EOF'
131 connect(3, {sa_family=AF_INET, sin_port=htons(65535), sin_addr=inet_addr("192.0.2.10")}, 16) = 0
132 connect(3, {sa_family=AF_INET6, sin6_port=htons(65535), inet_pton(AF_INET6, "::ffff:192.0.2.11", &sin6_addr), sin6_scope_id=0}, 28) = 0
EOF
if KATCH_ALLOW_MUSL_ROUTE_PROBE=1 KATCH_PORT=$capture_katch_port \
  "$ENTRYPOINT" --verify-capture "$workdir/other-musl-route-probe.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
  fail "explicit musl route probe allowance accepted a non-Katch destination"
fi
grep -F '192.0.2.10' "$workdir/out" >/dev/null ||
  fail "IPv4 musl route probe to another IP was not reported"
grep -F '::ffff:192.0.2.11' "$workdir/out" >/dev/null ||
  fail "IPv4-mapped musl route probe to another IP was not reported"

cat > "$workdir/sendto-musl-route-probe.log" <<EOF
133 sendto(3, "probe", 5, MSG_NOSIGNAL, {sa_family=AF_INET, sin_port=htons(65535), sin_addr=inet_addr("$capture_katch_ip")}, 16) = 0
EOF
if KATCH_ALLOW_MUSL_ROUTE_PROBE=1 KATCH_PORT=$capture_katch_port \
  "$ENTRYPOINT" --verify-capture "$workdir/sendto-musl-route-probe.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
  fail "explicit musl route probe allowance accepted a non-connect syscall"
fi
grep -F 'sendto(' "$workdir/out" >/dev/null || fail "non-connect musl route probe was not reported"

cat > "$workdir/wrong-port-musl-route-probe.log" <<EOF
134 connect(3, {sa_family=AF_INET, sin_port=htons(65534), sin_addr=inet_addr("$capture_katch_ip")}, 16) = 0
EOF
if KATCH_ALLOW_MUSL_ROUTE_PROBE=1 KATCH_PORT=$capture_katch_port \
  "$ENTRYPOINT" --verify-capture "$workdir/wrong-port-musl-route-probe.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
  fail "explicit musl route probe allowance accepted the wrong port"
fi
grep -F 'sin_port=htons(65534)' "$workdir/out" >/dev/null || fail "wrong-port musl route probe was not reported"

if KATCH_ALLOW_MUSL_ROUTE_PROBE=true KATCH_PORT=$capture_katch_port \
  "$ENTRYPOINT" --verify-capture "$workdir/katch-musl-route-probe-success.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
  fail "entrypoint accepted a non-binary musl route probe setting"
fi
grep -F 'KATCH_ALLOW_MUSL_ROUTE_PROBE must be 0 or 1' "$workdir/out" >/dev/null ||
  fail "entrypoint did not report the invalid musl route probe setting"

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

cat > "$workdir/nested-dns-probe.log" <<'EOF'
138 connect(3, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr("172.17.0.1")}, 16) = 0
EOF
if KATCH_PORT=$capture_katch_port "$ENTRYPOINT" --verify-capture "$workdir/nested-dns-probe.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
  fail "successful nested daemon DNS probe outside loopback was accepted"
fi
grep -q '172.17.0.1' "$workdir/out" || fail "nested daemon DNS probe was not reported"

# Go's RFC 6724 address sort connects a UDP socket to every candidate address on
# port 53 to learn the source address; it sends nothing. The entrypoint's
# IPv4-mapped hosts entry gives KATCH_HOST two addresses, so every Go client
# emits one such probe per address before dialing Katch. In a standard case the
# container resolver is authoritative: when Katch is not one of its nameservers
# a connect to Katch:53 can only be that route probe, never a DNS query.
cat > "$workdir/go-route-probe.log" <<EOF
160 connect(7, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr("$capture_katch_ip")}, 16) = 0
160 connect(7, {sa_family=AF_INET6, sin6_port=htons(53), sin6_flowinfo=htonl(0), inet_pton(AF_INET6, "::ffff:$capture_katch_ip", &sin6_addr), sin6_scope_id=0}, 28) = 0
160 connect(7, {sa_family=AF_INET, sin_port=htons($capture_katch_port), sin_addr=inet_addr("$capture_katch_ip")}, 16) = -1 EINPROGRESS (Operation now in progress)
EOF
printf 'nameserver 192.168.8.141\nnameserver 192.168.8.1\nsearch lan\n' > "$workdir/resolvers-without-katch"
KATCH_ROUTE_PROBE_RESOLVERS=$workdir/resolvers-without-katch KATCH_PORT=$capture_katch_port \
  "$ENTRYPOINT" --verify-capture "$workdir/go-route-probe.log" "$capture_katch_ip" ||
  fail "Go route probes to Katch:53 were rejected although Katch is not a nameserver"

for resolver_line in "nameserver $capture_katch_ip" "nameserver ::ffff:$capture_katch_ip"; do
  printf 'nameserver 192.168.8.141\n%s\n' "$resolver_line" > "$workdir/resolvers-with-katch"
  if KATCH_ROUTE_PROBE_RESOLVERS=$workdir/resolvers-with-katch KATCH_PORT=$capture_katch_port \
    "$ENTRYPOINT" --verify-capture "$workdir/go-route-probe.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
    fail "Katch:53 connect was accepted while Katch is a nameserver ($resolver_line)"
  fi
  grep -F 'htons(53)' "$workdir/out" >/dev/null || fail "Katch:53 connect with Katch as nameserver was not reported"
done

if KATCH_ROUTE_PROBE_RESOLVERS=$workdir/no-such-resolvers KATCH_PORT=$capture_katch_port \
  "$ENTRYPOINT" --verify-capture "$workdir/go-route-probe.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
  fail "Go route probe was accepted without a readable resolver snapshot"
fi

# The allowance keys on the syscall strace names after the pid, not on the text
# "connect(" appearing anywhere: a printed buffer can carry that text too.
cat > "$workdir/route-probe-disguised.log" <<EOF
161 sendto(7, " connect(x", 0, MSG_NOSIGNAL, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr("$capture_katch_ip")}, 16) = 0
EOF
if KATCH_ROUTE_PROBE_RESOLVERS=$workdir/resolvers-without-katch KATCH_PORT=$capture_katch_port \
  "$ENTRYPOINT" --verify-capture "$workdir/route-probe-disguised.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
  fail "a sendto whose payload contains connect( entered the route probe allowance"
fi
grep -F 'sendto' "$workdir/out" >/dev/null || fail "the disguised sendto was not reported"

# Concurrent Go threads make strace split a probe; its result arrives on the same
# pid after unrelated threads' resumed lines. This interleaving is copied from a
# real warm goproxy capture. Unrelated resumed lines carry only a result for a
# destination already audited on their own unfinished half.
cat > "$workdir/go-route-probe-split.log" <<EOF
1147 connect(9, {sa_family=AF_INET, sin_port=htons($capture_katch_port), sin_addr=inet_addr("$capture_katch_ip")}, 16 <unfinished ...>
1146 <... connect resumed>)            = -1 EINPROGRESS (Operation now in progress)
1145 connect(10, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr("$capture_katch_ip")}, 16 <unfinished ...>
1147 <... connect resumed>)            = -1 EINPROGRESS (Operation now in progress)
1138 connect(11, {sa_family=AF_INET, sin_port=htons($capture_katch_port), sin_addr=inet_addr("$capture_katch_ip")}, 16 <unfinished ...>
1149 connect(12, {sa_family=AF_INET, sin_port=htons($capture_katch_port), sin_addr=inet_addr("$capture_katch_ip")}, 16 <unfinished ...>
1145 <... connect resumed>)            = 0
EOF
KATCH_ROUTE_PROBE_RESOLVERS=$workdir/resolvers-without-katch KATCH_PORT=$capture_katch_port \
  "$ENTRYPOINT" --verify-capture "$workdir/go-route-probe-split.log" "$capture_katch_ip" ||
  fail "split Go route probe paired by pid across interleaved threads was rejected"

for rejected in \
  "161 connect(7, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr(\"$capture_katch_ip\")}, 16) = -1 EPERM (Operation not permitted)" \
  "162 sendto(7, \"query\", 5, MSG_NOSIGNAL, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr(\"$capture_katch_ip\")}, 16) = 5" \
  "163 connect(7, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr(\"8.8.8.8\")}, 16) = 0" \
  "164 connect(7, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr(\"$capture_katch_ip\")}, 16 <unfinished ...>
164 <... connect resumed>)            = -1 ECONNREFUSED (Connection refused)" \
  "165 connect(7, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr(\"$capture_katch_ip\")}, 16 <unfinished ...>"
do
  printf '%s\n' "$rejected" > "$workdir/go-route-probe-reject.log"
  if KATCH_ROUTE_PROBE_RESOLVERS=$workdir/resolvers-without-katch KATCH_PORT=$capture_katch_port \
    "$ENTRYPOINT" --verify-capture "$workdir/go-route-probe-reject.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
    fail "route probe allowance accepted: $rejected"
  fi
  grep -F 'non-katch connection attempt' "$workdir/out" >/dev/null || fail "route probe allowance did not report: $rejected"
done

cat > "$workdir/allowed-blocked-dns-probe.log" <<EOF
139 connect(3, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr("$capture_katch_ip")}, 16) = 0
140 connect(3, {sa_family=AF_INET6, sin6_port=htons(53), inet_pton(AF_INET6, "::ffff:$capture_katch_ip", &sin6_addr), sin6_scope_id=0}, 28) = 0
EOF
KATCH_ALLOW_BLOCKED_DNS_PROBE=1 KATCH_PORT=$capture_katch_port \
  "$ENTRYPOINT" --verify-capture "$workdir/allowed-blocked-dns-probe.log" "$capture_katch_ip" "$workdir/blocked-dns-probes.log" ||
  fail "explicit Docker blocked DNS probes to the exact Katch IP were rejected"
cmp -s "$workdir/allowed-blocked-dns-probe.log" "$workdir/blocked-dns-probes.log" ||
  fail "accepted blocked DNS probes were not preserved as separate evidence"

cat > "$workdir/split-blocked-dns-probe.log" <<EOF
146 connect(3, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr("$capture_katch_ip")}, 16 <unfinished ...>
147 connect(3, {sa_family=AF_INET6, sin6_port=htons(53), inet_pton(AF_INET6, "::ffff:$capture_katch_ip", &sin6_addr), sin6_scope_id=0}, 28 <unfinished ...>
148 connect(3, {sa_family=AF_INET, sin_addr=inet_addr("$capture_katch_ip"), sin_port=htons($capture_katch_port)}, 16) = 0
146 <... connect resumed>) = 0
147 <... connect resumed>) = 0
149 connect(3, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr("$capture_katch_ip")}, 16) = 0
EOF
cat > "$workdir/split-blocked-dns-probe.expected" <<EOF
146 connect(3, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr("$capture_katch_ip")}, 16 <unfinished ...>
146 <... connect resumed>) = 0
147 connect(3, {sa_family=AF_INET6, sin6_port=htons(53), inet_pton(AF_INET6, "::ffff:$capture_katch_ip", &sin6_addr), sin6_scope_id=0}, 28 <unfinished ...>
147 <... connect resumed>) = 0
149 connect(3, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr("$capture_katch_ip")}, 16) = 0
EOF
KATCH_ALLOW_BLOCKED_DNS_PROBE=1 KATCH_PORT=$capture_katch_port \
  "$ENTRYPOINT" --verify-capture "$workdir/split-blocked-dns-probe.log" "$capture_katch_ip" "$workdir/split-blocked-dns-probes.log" ||
  fail "same-PID split blocked DNS probes were rejected"
cmp -s "$workdir/split-blocked-dns-probe.expected" "$workdir/split-blocked-dns-probes.log" ||
  fail "split blocked DNS probe evidence did not preserve both lines of each pair"

if KATCH_PORT=$capture_katch_port \
  "$ENTRYPOINT" --verify-capture "$workdir/split-blocked-dns-probe.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
  fail "default capture audit accepted split blocked DNS probes"
fi

cat > "$workdir/orphan-split-blocked-dns-probe.log" <<EOF
150 connect(3, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr("$capture_katch_ip")}, 16 <unfinished ...>
EOF
if KATCH_ALLOW_BLOCKED_DNS_PROBE=1 KATCH_PORT=$capture_katch_port \
  "$ENTRYPOINT" --verify-capture "$workdir/orphan-split-blocked-dns-probe.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
  fail "explicit blocked DNS probe allowance accepted an orphan unfinished connect"
fi
grep -F '<unfinished ...>' "$workdir/out" >/dev/null || fail "orphan unfinished blocked DNS probe was not reported"

cat > "$workdir/wrong-pid-split-blocked-dns-probe.log" <<EOF
151 connect(3, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr("$capture_katch_ip")}, 16 <unfinished ...>
152 <... connect resumed>) = 0
EOF
if KATCH_ALLOW_BLOCKED_DNS_PROBE=1 KATCH_PORT=$capture_katch_port \
  "$ENTRYPOINT" --verify-capture "$workdir/wrong-pid-split-blocked-dns-probe.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
  fail "explicit blocked DNS probe allowance paired a resumed connect from the wrong PID"
fi
# 151's probe never pairs with its own half and is reported as unpaired at the end; 152's
# resumed line carries only a result and cannot complete another PID's probe.
grep -F '151 connect(3,' "$workdir/out" >/dev/null ||
  fail "wrong-PID blocked DNS probe was not reported as unpaired"

# The interleaving from the real Docker warm phase (2026-09-21, 9969a1c): strace split a
# Katch connect, and its resumed half arrived while another PID's blocked DNS probe was still
# unpaired. The resumed line has no destination; its unfinished half was already audited
# (the Katch port here), so a pending probe elsewhere must not reject it.
cat > "$workdir/interleaved-katch-connect-during-blocked-dns-probe.log" <<EOF
98 connect(27, {sa_family=AF_INET, sin_port=htons($capture_katch_port), sin_addr=inet_addr("$capture_katch_ip")}, 16 <unfinished ...>
60 connect(31, {sa_family=AF_INET, sin_port=htons($capture_katch_port), sin_addr=inet_addr("$capture_katch_ip")}, 16 <unfinished ...>
80 connect(30, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr("$capture_katch_ip")}, 16 <unfinished ...>
98 <... connect resumed>) = -1 EINPROGRESS (Operation in progress)
80 <... connect resumed>) = 0
60 <... connect resumed>) = -1 EINPROGRESS (Operation in progress)
EOF
KATCH_ALLOW_BLOCKED_DNS_PROBE=1 KATCH_PORT=$capture_katch_port \
  "$ENTRYPOINT" --verify-capture "$workdir/interleaved-katch-connect-during-blocked-dns-probe.log" "$capture_katch_ip" >"$workdir/out" 2>&1 ||
  fail "a split Katch connect resumed while another PID's blocked DNS probe was pending was rejected: $(cat "$workdir/out")"

cat > "$workdir/failed-split-blocked-dns-probe.log" <<EOF
153 connect(3, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr("$capture_katch_ip")}, 16 <unfinished ...>
153 <... connect resumed>) = -1 EPERM (Operation not permitted)
EOF
if KATCH_ALLOW_BLOCKED_DNS_PROBE=1 KATCH_PORT=$capture_katch_port \
  "$ENTRYPOINT" --verify-capture "$workdir/failed-split-blocked-dns-probe.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
  fail "explicit blocked DNS probe allowance accepted a failed resumed connect"
fi
grep -F '= -1 EPERM' "$workdir/out" >/dev/null || fail "failed resumed blocked DNS probe was not reported"

cat > "$workdir/wrong-syscall-split-blocked-dns-probe.log" <<EOF
154 connect(3, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr("$capture_katch_ip")}, 16 <unfinished ...>
154 <... sendto resumed>) = 0
EOF
if KATCH_ALLOW_BLOCKED_DNS_PROBE=1 KATCH_PORT=$capture_katch_port \
  "$ENTRYPOINT" --verify-capture "$workdir/wrong-syscall-split-blocked-dns-probe.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
  fail "explicit blocked DNS probe allowance accepted a resumed wrong syscall"
fi
grep -F '<... sendto resumed>) = 0' "$workdir/out" >/dev/null ||
  fail "wrong resumed syscall for blocked DNS probe was not reported"

cat > "$workdir/external-split-blocked-dns-probe.log" <<'EOF'
155 connect(3, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr("8.8.8.8")}, 16 <unfinished ...>
155 <... connect resumed>) = 0
EOF
if KATCH_ALLOW_BLOCKED_DNS_PROBE=1 KATCH_PORT=$capture_katch_port \
  "$ENTRYPOINT" --verify-capture "$workdir/external-split-blocked-dns-probe.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
  fail "explicit blocked DNS probe allowance accepted a split external destination"
fi
grep -F '8.8.8.8' "$workdir/out" >/dev/null || fail "split external blocked DNS probe was not reported"

cat > "$workdir/wrong-port-split-blocked-dns-probe.log" <<EOF
156 connect(3, {sa_family=AF_INET, sin_port=htons(5353), sin_addr=inet_addr("$capture_katch_ip")}, 16 <unfinished ...>
156 <... connect resumed>) = 0
EOF
if KATCH_ALLOW_BLOCKED_DNS_PROBE=1 KATCH_PORT=$capture_katch_port \
  "$ENTRYPOINT" --verify-capture "$workdir/wrong-port-split-blocked-dns-probe.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
  fail "explicit blocked DNS probe allowance accepted a split wrong-port destination"
fi
grep -F 'sin_port=htons(5353)' "$workdir/out" >/dev/null ||
  fail "split wrong-port blocked DNS probe was not reported"

cat > "$workdir/external-blocked-dns-probe.log" <<'EOF'
141 connect(3, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr("8.8.8.8")}, 16) = 0
EOF
if KATCH_ALLOW_BLOCKED_DNS_PROBE=1 KATCH_PORT=$capture_katch_port \
  "$ENTRYPOINT" --verify-capture "$workdir/external-blocked-dns-probe.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
  fail "explicit blocked DNS probe allowance accepted an external destination"
fi
grep -F '8.8.8.8' "$workdir/out" >/dev/null || fail "external blocked DNS probe was not reported"

cat > "$workdir/failed-blocked-dns-probe.log" <<EOF
142 connect(3, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr("$capture_katch_ip")}, 16) = -1 EPERM (Operation not permitted)
143 connect(3, {sa_family=AF_INET6, sin6_port=htons(53), inet_pton(AF_INET6, "::ffff:$capture_katch_ip", &sin6_addr)}, 28) = 1
EOF
if KATCH_ALLOW_BLOCKED_DNS_PROBE=1 KATCH_PORT=$capture_katch_port \
  "$ENTRYPOINT" --verify-capture "$workdir/failed-blocked-dns-probe.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
  fail "explicit blocked DNS probe allowance accepted a nonzero or failed result"
fi
grep -F '= -1 EPERM' "$workdir/out" >/dev/null || fail "failed blocked DNS probe was not reported"
grep -F '= 1' "$workdir/out" >/dev/null || fail "nonzero blocked DNS probe was not reported"

cat > "$workdir/wrong-port-blocked-dns-probe.log" <<EOF
144 connect(3, {sa_family=AF_INET, sin_port=htons(5353), sin_addr=inet_addr("$capture_katch_ip")}, 16) = 0
EOF
if KATCH_ALLOW_BLOCKED_DNS_PROBE=1 KATCH_PORT=$capture_katch_port \
  "$ENTRYPOINT" --verify-capture "$workdir/wrong-port-blocked-dns-probe.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
  fail "explicit blocked DNS probe allowance accepted the wrong port"
fi
grep -F 'sin_port=htons(5353)' "$workdir/out" >/dev/null || fail "wrong-port blocked DNS probe was not reported"

cat > "$workdir/sendto-blocked-dns-probe.log" <<EOF
145 sendto(3, "query", 5, MSG_NOSIGNAL, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr("$capture_katch_ip")}, 16) = 0
EOF
if KATCH_ALLOW_BLOCKED_DNS_PROBE=1 KATCH_PORT=$capture_katch_port \
  "$ENTRYPOINT" --verify-capture "$workdir/sendto-blocked-dns-probe.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
  fail "explicit blocked DNS probe allowance accepted a non-connect syscall"
fi
grep -F 'sendto(' "$workdir/out" >/dev/null || fail "non-connect blocked DNS probe was not reported"

if KATCH_ALLOW_BLOCKED_DNS_PROBE=true KATCH_PORT=$capture_katch_port \
  "$ENTRYPOINT" --verify-capture "$workdir/allowed-blocked-dns-probe.log" "$capture_katch_ip" >"$workdir/out" 2>&1; then
  fail "entrypoint accepted a non-binary blocked DNS probe setting"
fi
grep -F 'KATCH_ALLOW_BLOCKED_DNS_PROBE must be 0 or 1' "$workdir/out" >/dev/null ||
  fail "entrypoint did not report the invalid blocked DNS probe setting"

resolver_file=$workdir/resolv.conf
resolver_evidence=$workdir/resolver-evidence
printf '%s\n' 'nameserver 172.17.0.1' 'search container.invalid' > "$resolver_file"
"$ENTRYPOINT" --install-loopback-resolver "$resolver_file" "$resolver_evidence"
expected_loopback_resolver=$(printf '%s\n' 'nameserver 127.0.0.1' 'options timeout:1 attempts:1')
[ "$(cat "$resolver_file")" = "$expected_loopback_resolver" ] ||
  fail "loopback resolver helper did not install the bounded resolver configuration"
[ "$(cat "$resolver_evidence/resolv.conf.before")" = "$(printf '%s\n' 'nameserver 172.17.0.1' 'search container.invalid')" ] ||
  fail "loopback resolver helper did not preserve the original resolver evidence"
[ "$(cat "$resolver_evidence/resolv.conf.after")" = "$expected_loopback_resolver" ] ||
  fail "loopback resolver helper did not preserve the active resolver evidence"

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
    [ -z "${STRACE_CAPTURE_FIXTURE:-}" ] || cat "$STRACE_CAPTURE_FIXTURE" > "$capture"
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

# End to end through the real entrypoint: a standard case snapshots its resolver
# and the Go route probes that its own IPv4-mapped hosts entry provokes pass the
# audit. This host's resolver never lists the fixture Katch address.
cat > "$workdir/entrypoint-go-probe.log" <<'EOF'
170 connect(7, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr("172.18.0.2")}, 16) = 0
170 connect(7, {sa_family=AF_INET6, sin6_port=htons(53), inet_pton(AF_INET6, "::ffff:172.18.0.2", &sin6_addr), sin6_scope_id=0}, 28) = 0
170 connect(7, {sa_family=AF_INET, sin_port=htons(8080), sin_addr=inet_addr("172.18.0.2")}, 16) = -1 EINPROGRESS (Operation now in progress)
EOF
rm -f "$workdir/host-mapping.state"
if ! run_entrypoint "$workdir/go-probe-artifacts" env -u KATCH_CLIENT_USER -u KATCH_CLIENT_CA_CERT \
  STRACE_CAPTURE_FIXTURE="$workdir/entrypoint-go-probe.log" >"$workdir/out" 2>&1; then
  cat "$workdir/out" >&2
  fail "standard case rejected the Go route probes its own hosts mapping provokes"
fi
[ -s "$workdir/go-probe-artifacts/resolv.conf.before" ] ||
  fail "standard case did not snapshot its resolver before the client ran"
[ -s "$workdir/go-probe-artifacts/resolvers.audited" ] ||
  fail "standard case did not keep the resolver state it audited against"

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
privileged_harness_case=$workdir/privileged-harness-case.yaml
blocked_dns_harness_case=$workdir/blocked-dns-harness-case.yaml
musl_route_harness_case=$workdir/musl-route-harness-case.yaml
second_harness_case=$workdir/second-harness-case.yaml
printf '%s\n' '{"name":"ca-contract","image":"example/client:1","setup":"true","run":"true","assert":"true","required_upstreams":["example.invalid"]}' > "$harness_case"
printf '%s\n' '{"name":"privileged-contract","image":"example/client:1","privileged":true,"setup":"true","run":"true","assert":"true","required_upstreams":["example.invalid"]}' > "$privileged_harness_case"
printf '%s\n' '{"name":"docker-registry-regression","image":"example/client:1","privileged":true,"allow_blocked_dns_probe":true,"setup":"true","run":"true","assert":"true","required_upstreams":["example.invalid"]}' > "$blocked_dns_harness_case"
printf '%s\n' '{"name":"apk","image":"example/client:1","allow_musl_route_probe":true,"setup":"true","run":"true","assert":"true","required_upstreams":["example.invalid"]}' > "$musl_route_harness_case"
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
[ "$(grep -Fc 'ARG=--cap-add' "$workdir/runtime.log")" -eq 4 ] ||
  fail "standard cases do not retain NET_ADMIN and NET_RAW in both phases"
[ "$(grep -Fc 'ARG=NET_ADMIN' "$workdir/runtime.log")" -eq 2 ] || fail "standard cases lack NET_ADMIN"
[ "$(grep -Fc 'ARG=NET_RAW' "$workdir/runtime.log")" -eq 2 ] || fail "standard cases lack NET_RAW"
[ "$(grep -Fc 'ARG=--security-opt' "$workdir/runtime.log")" -eq 2 ] ||
  fail "standard cases do not retain no-new-privileges"
[ "$(grep -Fc 'ARG=no-new-privileges' "$workdir/runtime.log")" -eq 2 ] ||
  fail "standard cases have the wrong security option"
if grep -Fx 'ARG=--privileged' "$workdir/runtime.log"; then
  fail "standard cases unexpectedly run privileged"
fi
if grep -Fx 'ARG=KATCH_LOOPBACK_RESOLVER=1' "$workdir/runtime.log"; then
  fail "standard cases unexpectedly enable the loopback resolver"
fi
if grep -Fx 'ARG=KATCH_ALLOW_BLOCKED_DNS_PROBE=1' "$workdir/runtime.log"; then
  fail "standard cases unexpectedly allow blocked DNS probes"
fi
if grep -Fx 'ARG=KATCH_ALLOW_MUSL_ROUTE_PROBE=1' "$workdir/runtime.log"; then
  fail "standard cases unexpectedly allow musl route probes"
fi

: > "$workdir/runtime.log"
printf '%s\n' 0 > "$workdir/metric-state"
env PATH="$harness_bin:$PATH" RUNTIME_LOG="$workdir/runtime.log" METRIC_STATE="$workdir/metric-state" \
  CONTAINER_RUNTIME=fake-runtime ARTIFACT_ROOT="$workdir/harness-privileged" \
  KATCH_URL=https://katch.invalid KATCH_METRICS_URL=http://metrics.invalid \
  KATCH_HOST=katch.invalid KATCH_PORT=443 \
  "$HARNESS" "$privileged_harness_case" >"$workdir/out" 2>&1
[ "$(grep -Fxc 'ARG=--privileged' "$workdir/runtime.log")" -eq 2 ] ||
  fail "privileged cases do not pass --privileged in both phases"
[ "$(grep -Fxc 'ARG=KATCH_LOOPBACK_RESOLVER=1' "$workdir/runtime.log")" -eq 2 ] ||
  fail "privileged cases do not require the loopback resolver in both phases"
if grep -E '^ARG=(--security-opt|no-new-privileges|--cap-add|NET_ADMIN|NET_RAW)$' "$workdir/runtime.log"; then
  fail "privileged cases retain incompatible standard isolation flags"
fi
if grep -Fx 'ARG=KATCH_ALLOW_BLOCKED_DNS_PROBE=1' "$workdir/runtime.log"; then
  fail "privilege alone enables the blocked DNS probe allowance"
fi
if grep -Fx 'ARG=KATCH_ALLOW_MUSL_ROUTE_PROBE=1' "$workdir/runtime.log"; then
  fail "privilege alone enables the musl route probe allowance"
fi

: > "$workdir/runtime.log"
printf '%s\n' 0 > "$workdir/metric-state"
env PATH="$harness_bin:$PATH" RUNTIME_LOG="$workdir/runtime.log" METRIC_STATE="$workdir/metric-state" \
  CONTAINER_RUNTIME=fake-runtime ARTIFACT_ROOT="$workdir/harness-blocked-dns" \
  KATCH_URL=https://katch.invalid KATCH_METRICS_URL=http://metrics.invalid \
  KATCH_HOST=katch.invalid KATCH_PORT=443 \
  "$HARNESS" "$blocked_dns_harness_case" >"$workdir/out" 2>&1
[ "$(grep -Fxc 'ARG=KATCH_ALLOW_BLOCKED_DNS_PROBE=1' "$workdir/runtime.log")" -eq 2 ] ||
  fail "case field does not enable blocked DNS probe auditing in both phases"
if grep -Fx 'ARG=KATCH_ALLOW_MUSL_ROUTE_PROBE=1' "$workdir/runtime.log"; then
  fail "Docker DNS probe allowance also enables the APK musl route probe allowance"
fi

: > "$workdir/runtime.log"
printf '%s\n' 0 > "$workdir/metric-state"
env PATH="$harness_bin:$PATH" RUNTIME_LOG="$workdir/runtime.log" METRIC_STATE="$workdir/metric-state" \
  CONTAINER_RUNTIME=fake-runtime ARTIFACT_ROOT="$workdir/harness-musl-route" \
  KATCH_URL=https://katch.invalid KATCH_METRICS_URL=http://metrics.invalid \
  KATCH_HOST=katch.invalid KATCH_PORT=443 \
  "$HARNESS" "$musl_route_harness_case" >"$workdir/out" 2>&1
[ "$(grep -Fxc 'ARG=KATCH_ALLOW_MUSL_ROUTE_PROBE=1' "$workdir/runtime.log")" -eq 2 ] ||
  fail "APK case field does not enable musl route probe auditing in both phases"
if grep -Fx 'ARG=KATCH_ALLOW_BLOCKED_DNS_PROBE=1' "$workdir/runtime.log"; then
  fail "APK musl route probe allowance also enables the Docker DNS probe allowance"
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

printf '%s\n' '{"name":"bad-privileged","image":"busybox:1","privileged":"yes","setup":"true","run":"true","assert":"true","required_upstreams":["example.invalid"]}' > "$bad_case"
if "$HARNESS" --check "$bad_case" >"$workdir/out" 2>&1; then
  fail "case schema accepted a non-boolean privileged value"
fi

printf '%s\n' '{"name":"bad-dns-probe","image":"busybox:1","allow_blocked_dns_probe":true,"setup":"true","run":"true","assert":"true","required_upstreams":["example.invalid"]}' > "$bad_case"
if "$HARNESS" --check "$bad_case" >"$workdir/out" 2>&1; then
  fail "case schema allowed a non-Docker case to opt into blocked DNS probes"
fi

printf '%s\n' '{"name":"docker-registry-regression","image":"busybox:1","allow_blocked_dns_probe":"yes","setup":"true","run":"true","assert":"true","required_upstreams":["example.invalid"]}' > "$bad_case"
if "$HARNESS" --check "$bad_case" >"$workdir/out" 2>&1; then
  fail "case schema accepted a non-boolean blocked DNS probe allowance"
fi

printf '%s\n' '{"name":"not-apk","image":"busybox:1","allow_musl_route_probe":true,"setup":"true","run":"true","assert":"true","required_upstreams":["example.invalid"]}' > "$bad_case"
if "$HARNESS" --check "$bad_case" >"$workdir/out" 2>&1; then
  fail "case schema allowed a non-APK case to opt into musl route probes"
fi

printf '%s\n' '{"name":"apk","image":"busybox:1","setup":"true","run":"true","assert":"true","required_upstreams":["example.invalid"]}' > "$bad_case"
if "$HARNESS" --check "$bad_case" >"$workdir/out" 2>&1; then
  fail "case schema allowed APK to omit the musl route probe allowance"
fi

printf '%s\n' '{"name":"apk","image":"busybox:1","allow_musl_route_probe":false,"setup":"true","run":"true","assert":"true","required_upstreams":["example.invalid"]}' > "$bad_case"
if "$HARNESS" --check "$bad_case" >"$workdir/out" 2>&1; then
  fail "case schema accepted a false APK musl route probe allowance"
fi

printf '%s\n' '{"name":"apk","image":"busybox:1","allow_musl_route_probe":"yes","setup":"true","run":"true","assert":"true","required_upstreams":["example.invalid"]}' > "$bad_case"
if "$HARNESS" --check "$bad_case" >"$workdir/out" 2>&1; then
  fail "case schema accepted a non-boolean musl route probe allowance"
fi

printf '%s\n' "harness self-tests passed"
