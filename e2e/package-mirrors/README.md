# Package mirror runtime harness

The harness runs each case twice in a fresh client container. The client namespace permits TCP only to katch and loopback; katch itself remains outside that namespace and retains origin egress. `strace` records every DNS/socket `connect()` attempt, and any destination other than katch or loopback fails the case even when the client command succeeds.

Case files use the JSON-compatible subset of YAML and the fixed schema `{name,image,setup,run,assert,required_upstreams}`. Treat case commands as repository code: the harness writes them to mounted scripts and never interpolates them through `eval`. Images must use a non-`latest` tag or a digest.

Build and check the harness:

```sh
make package-mirror-image
make test-package-mirror-harness
```

Run configured cases after katch has `site_domain` set to the exact client-visible URL and enabled compatible upstreams:

```sh
KATCH_URL=http://host.docker.internal:8080 \
KATCH_METRICS_URL=http://127.0.0.1:8080/metrics \
KATCH_HOST=host.docker.internal KATCH_PORT=8080 \
  e2e/package-mirrors/harness.sh npm
```

`KATCH_HOST` is injected into the client container's `/etc/hosts` with Docker/Podman's `host-gateway`, so bootstrap never needs public DNS. Set `KATCH_ADD_HOST` to a static katch IPv4 address when katch is attached through another bridge. The harness stores per-phase client logs, connection records, firewall snapshots, and Prometheus snapshots under `${ARTIFACT_ROOT:-${TMPDIR:-/tmp}/katch-package-mirrors-artifacts}`. It requires root only inside the disposable client container for `NET_ADMIN`; host root is neither required nor accepted as a substitute.

The npm case covers npm, pnpm, Yarn Classic, Yarn Berry, and Bun with native installation/import checks. Generated lockfiles are checked for leaked public registry URLs. Existing lockfiles are not silently claimed compatible: use registry-host replacement where the client supports it, otherwise regenerate them through katch.

The shared regression case always exercises Git. Docker and Podman are reported as blocked unless a future pinned image provides a daemon that is isolated in the same restricted namespace; a host daemon is deliberately not mounted because that would bypass the client firewall.

A missing container runtime is a blocked runtime verification, not a pass. `tests/harness_test.sh` remains deterministic without a runtime and checks schema rejection plus non-katch DNS/connect detection; CI also builds the pinned client image and executes the same checks.
