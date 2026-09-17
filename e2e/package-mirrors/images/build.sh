#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../../.." && pwd)
CONTRACT=$ROOT/e2e/package-mirrors/images/contract.json
RUNTIME=${CONTAINER_RUNTIME:-docker}

command -v jq >/dev/null 2>&1 || {
  printf '%s\n' 'required command unavailable: jq' >&2
  exit 69
}
command -v "$RUNTIME" >/dev/null 2>&1 || {
  printf '%s\n' "required container runtime unavailable: $RUNTIME" >&2
  exit 69
}

jq -c '.[]' "$CONTRACT" | while IFS= read -r row; do
  image=$(printf '%s' "$row" | jq -r '.image')
  dockerfile=$(printf '%s' "$row" | jq -r '.dockerfile')
  target=$(printf '%s' "$row" | jq -r '.target')
  "$RUNTIME" build \
    --file "$ROOT/e2e/package-mirrors/images/$dockerfile" \
    --target "$target" \
    --tag "$image" \
    "$ROOT"
  "$RUNTIME" run --rm --network none \
    --entrypoint /usr/local/bin/katch-client-smoke \
    "$image"
done
