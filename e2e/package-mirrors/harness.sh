#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
CASE_DIR=${CASE_DIR:-$ROOT/e2e/package-mirrors/cases}
ARTIFACT_ROOT=${ARTIFACT_ROOT:-${TMPDIR:-/tmp}/katch-package-mirrors-artifacts}
RUNTIME=${CONTAINER_RUNTIME:-}
CLIENT_CA_CERT=/run/katch-test-ca.crt

usage() {
  printf '%s\n' "usage: $0 --check CASE... | $0 [CASE...]" >&2
  exit 64
}

need() {
  command -v "$1" >/dev/null 2>&1 || {
    printf '%s\n' "required command unavailable: $1" >&2
    exit 69
  }
}

validate_case() {
  case_file=$1
  jq -e '
    type == "object" and
    (keys | sort) == (["assert", "image", "name", "required_upstreams", "run", "setup"] | sort) and
    (.name | type == "string" and test("^[a-z0-9][a-z0-9-]{0,62}$")) and
    (.image | type == "string" and test("^[A-Za-z0-9./_-]+(:[A-Za-z0-9._-]+|@sha256:[a-f0-9]{64})$") and (endswith(":latest") | not)) and
    (.setup | type == "string" and length > 0) and
    (.run | type == "string" and length > 0) and
    (.assert | type == "string" and length > 0) and
    (.required_upstreams | type == "array" and length > 0 and all(.[]; type == "string" and test("^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$"))) and
    ((.required_upstreams | unique | length) == (.required_upstreams | length))
  ' "$case_file" >/dev/null || {
    printf '%s\n' "invalid case: $case_file" >&2
    return 1
  }
}

case_files() {
  if [ "$#" -eq 0 ]; then
    find "$CASE_DIR" -maxdepth 1 -type f -name '*.yaml' -print | LC_ALL=C sort
    return
  fi
  for value in "$@"; do
    if [ -f "$value" ]; then
      printf '%s\n' "$value"
    else
      printf '%s\n' "$CASE_DIR/$value.yaml"
    fi
  done
}

need jq
if [ "${1:-}" = "--check" ]; then
  shift
  [ "$#" -gt 0 ] || usage
  for file in "$@"; do
    validate_case "$file"
  done
  printf '%s\n' "package mirror cases valid"
  exit
fi

if [ -n "${KATCH_CA_CERT:-}" ]; then
  case $KATCH_CA_CERT in
    /*) ;;
    *)
      printf '%s\n' "KATCH_CA_CERT must be an absolute readable regular file: $KATCH_CA_CERT" >&2
      exit 66
      ;;
  esac
  if [ ! -f "$KATCH_CA_CERT" ] || [ ! -r "$KATCH_CA_CERT" ]; then
    printf '%s\n' "KATCH_CA_CERT must be an absolute readable regular file: $KATCH_CA_CERT" >&2
    exit 66
  fi
fi

: "${KATCH_URL:?KATCH_URL must be reachable from client containers}"
: "${KATCH_METRICS_URL:?KATCH_METRICS_URL is required for origin counts}"
: "${KATCH_HOST:?KATCH_HOST is the container-resolvable katch host}"
: "${KATCH_PORT:?KATCH_PORT is the katch listener port}"

if [ -z "$RUNTIME" ]; then
  if command -v docker >/dev/null 2>&1; then
    RUNTIME=docker
  elif command -v podman >/dev/null 2>&1; then
    RUNTIME=podman
  else
    printf '%s\n' "blocked: neither docker nor podman is installed" >&2
    exit 69
  fi
fi
need "$RUNTIME"
need curl

run_id=$(date -u '+%Y%m%dT%H%M%SZ').$$
run_root=$ARTIFACT_ROOT/$run_id
mkdir -p "$run_root"

metric_total() {
  metrics=$1
  upstreams=$2
  awk '
    NR == FNR { wanted[$1] = 1; next }
    /^katch_origin_requests_total\{/ {
      line = $0
      if (match(line, /upstream="[^"]+"/)) {
        host = substr(line, RSTART + 10, RLENGTH - 11)
        if (host in wanted) {
          sub(/^.*} /, "", line)
          total += line + 0
        }
      }
    }
    END { printf "%.0f\n", total + 0 }
  ' "$upstreams" "$metrics"
}

snapshot_metrics() {
  output=$1
  curl --fail --silent --show-error --max-time 10 "$KATCH_METRICS_URL" > "$output"
}

run_phase() {
  case_file=$1
  phase=$2
  case_root=$3
  image=$(jq -r '.image' "$case_file")
  phase_root=$case_root/$phase
  shared=$case_root/shared
  scripts=$phase_root/case
  artifacts=$phase_root/artifacts
  mkdir -p "$scripts" "$artifacts"
  chmod 0777 "$artifacts"
  jq -r '.setup' "$case_file" > "$scripts/setup"
  jq -r '.run' "$case_file" > "$scripts/run"
  jq -r '.assert' "$case_file" > "$scripts/assert"
  chmod 0555 "$scripts/setup" "$scripts/run" "$scripts/assert"

  set -- "$RUNTIME" run --rm \
    --cap-add NET_ADMIN --cap-add NET_RAW \
    --security-opt no-new-privileges \
    --add-host "$KATCH_HOST:${KATCH_ADD_HOST:-host-gateway}" \
    --mount "type=bind,src=$scripts,dst=/case,readonly" \
    --mount "type=bind,src=$artifacts,dst=/artifacts" \
    --mount "type=bind,src=$shared,dst=/shared"
  if [ -n "${KATCH_CA_CERT:-}" ]; then
    set -- "$@" \
      --mount "type=bind,src=$KATCH_CA_CERT,dst=$CLIENT_CA_CERT,readonly" \
      --env "KATCH_CLIENT_CA_CERT=$CLIENT_CA_CERT"
  fi
  set -- "$@" \
    --env "KATCH_URL=$KATCH_URL" \
    --env "KATCH_BASE_URL=$KATCH_URL" \
    --env "KATCH_HOST=$KATCH_HOST" \
    --env "KATCH_PORT=$KATCH_PORT" \
    --env "KATCH_PHASE=$phase" \
    --env KATCH_ARTIFACTS=/artifacts \
    --env KATCH_SHARED=/shared \
    "$image"
  "$@" > "$phase_root/client.log" 2>&1
}

files=$(mktemp "${TMPDIR:-/tmp}/katch-case-list.XXXXXX")
trap 'rm -f "$files"' EXIT HUP INT TERM
case_files "$@" > "$files"
[ -s "$files" ] || usage

while IFS= read -r case_file; do
  validate_case "$case_file"
  name=$(jq -r '.name' "$case_file")
  case_root=$run_root/$name
  mkdir -p "$case_root/shared"
  chmod 0777 "$case_root/shared"
  upstreams=$case_root/required-upstreams
  jq -r '.required_upstreams[]' "$case_file" > "$upstreams"

  snapshot_metrics "$case_root/origin.before"
  run_phase "$case_file" cold "$case_root"
  snapshot_metrics "$case_root/origin.after-cold"
  run_phase "$case_file" warm "$case_root"
  snapshot_metrics "$case_root/origin.after-warm"

  before=$(metric_total "$case_root/origin.before" "$upstreams")
  after_cold=$(metric_total "$case_root/origin.after-cold" "$upstreams")
  after_warm=$(metric_total "$case_root/origin.after-warm" "$upstreams")
  cold=$((after_cold - before))
  warm=$((after_warm - after_cold))
  printf 'case=%s cold_origin=%s warm_origin=%s artifacts=%s\n' "$name" "$cold" "$warm" "$case_root"
  [ "$cold" -gt 0 ] || {
    printf '%s\n' "case $name made no cold origin request" >&2
    exit 1
  }
  [ "$warm" -eq 0 ] || {
    printf '%s\n' "case $name made $warm warm origin requests" >&2
    exit 1
  }
done < "$files"
