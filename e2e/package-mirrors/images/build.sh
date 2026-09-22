#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../../.." && pwd)
CONTRACT=$ROOT/e2e/package-mirrors/images/contract.json
RUNTIME=${CONTAINER_RUNTIME:-docker}
REQUESTED_TARGETS=$*

command -v jq >/dev/null 2>&1 || {
  printf '%s\n' 'required command unavailable: jq' >&2
  exit 69
}
command -v "$RUNTIME" >/dev/null 2>&1 || {
  printf '%s\n' "required container runtime unavailable: $RUNTIME" >&2
  exit 69
}

for requested_target in $REQUESTED_TARGETS; do
  jq -e --arg target "$requested_target" 'any(.[]; .target == $target)' "$CONTRACT" >/dev/null || {
    printf '%s\n' "unknown package mirror image target: $requested_target" >&2
    exit 64
  }
done

jq -c '.[]' "$CONTRACT" | while IFS= read -r row; do
  target=$(printf '%s' "$row" | jq -r '.target')
  if [ -n "$REQUESTED_TARGETS" ]; then
    case " $REQUESTED_TARGETS " in
      *" $target "*) ;;
      *) continue ;;
    esac
  fi
  image=$(printf '%s' "$row" | jq -r '.image')
  dockerfile=$(printf '%s' "$row" | jq -r '.dockerfile')
  "$RUNTIME" build \
    --file "$ROOT/e2e/package-mirrors/images/$dockerfile" \
    --target "$target" \
    --tag "$image" \
    "$ROOT"
  "$RUNTIME" run --rm --network none \
    --entrypoint /usr/local/bin/katch-client-smoke \
    "$image"
done
