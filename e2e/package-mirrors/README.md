# Package mirror runtime harness

The harness runs each case twice in a fresh, locally built client image. Every image starts as root in the common entrypoint so it can install an IPv4/IPv6 OUTPUT firewall and start `strace`; the command phase then runs as the image contract user with matching `HOME`, `USER`, and `LOGNAME`. Only the Debian APT, CentOS DNF/YUM, and Alpine APK cases stay root because they modify the container package database.

The client namespace permits TCP only to katch and loopback. Katch remains outside that namespace and retains origin egress. `strace` records every DNS/socket connection attempt, and any destination other than katch or loopback fails the case even when the client command succeeds. The harness never mounts a host Docker or Podman socket.

## Image contract

`images/contract.json` is the source of truth for case-to-image mapping, build targets, runtime users, and exact client smoke versions. The current matrix is:

| Image target | Clients | Command user |
| --- | --- | --- |
| node | Node 22.20.0, npm 11.12.1, pnpm 11.9.0, Yarn 1.22.22, Yarn Berry 4.10.3, Bun 1.3.11, Git | `client` |
| go | Go 1.26.0 | `client` |
| python | Python 3.13.7, pip 25.2, uv 0.8.17, Poetry 2.2.1 | `client` |
| rust | Rust/Cargo 1.89.0 | `client` |
| jvm | JDK 21.0.8, Maven 3.9.11, Gradle 9.0.0, sbt 1.11.6 | `client` |
| dotnet | .NET SDK 8.0.414 | `client` |
| ruby | Ruby 3.4.5, Bundler 2.7.1 | `client` |
| debian | APT 2.6.1 | `root` |
| rpm | DNF/YUM 4.14.0 | `root` |
| alpine | APK 2.14.9 | `root` |
| composer | Composer 2.9.5 | `client` |
| homebrew | Homebrew 4.6.20 | `linuxbrew` |

The Node base already supplies Yarn Classic 1.22.22. Yarn Berry is installed privately under `/opt/yarn-berry` and exposed only as `yarn-berry`, so the two CLIs cannot replace each other's symlinks.

Build every target and run its exact-version smoke with container networking disabled:

```sh
make package-mirror-image
```

Run deterministic schema, image-contract, embedded-shell, isolation, and connection-capture tests without Docker or Podman:

```sh
make test-package-mirror-harness
```

## Running cases

Case files use the JSON-compatible subset of YAML and the fixed schema `{name,image,setup,run,assert,required_upstreams}`. The harness writes commands to read-only mounted scripts and never interpolates them through `eval`.

After katch has `site_domain` set to the exact client-visible URL and compatible upstreams enabled, run one or more cases:

```sh
KATCH_URL=http://host.docker.internal:8080 \
KATCH_METRICS_URL=http://127.0.0.1:8080/metrics \
KATCH_HOST=host.docker.internal KATCH_PORT=8080 \
  e2e/package-mirrors/harness.sh npm
```

The harness supplies the same value as both `KATCH_URL` and `KATCH_BASE_URL`. `KATCH_HOST` is injected into `/etc/hosts` with Docker/Podman's `host-gateway`, so bootstrap does not need public DNS. Set `KATCH_ADD_HOST` to a static katch IPv4 address when katch is attached through another bridge.

For an HTTPS test endpoint signed by a private test CA, set `KATCH_CA_CERT` to an absolute path to a readable regular certificate file on the host:

```sh
KATCH_CA_CERT=/absolute/path/to/ca.crt \
KATCH_URL=https://host.docker.internal:8443 \
KATCH_METRICS_URL=http://127.0.0.1:8080/metrics \
KATCH_HOST=host.docker.internal KATCH_PORT=8443 \
  e2e/package-mirrors/harness.sh nuget
```

The harness mounts that file read-only at `/run/katch-test-ca.crt` and passes only the mounted path to the client entrypoint. The root entrypoint installs it into the image's system trust store before firewall tracing and before switching to `client` or `linuxbrew`. Leaving `KATCH_CA_CERT` unset adds neither the mount nor the client environment variable. This trust path is required for HTTPS NuGet repository-signature resources; signature validation remains enabled.

Artifacts include per-phase client output, connection records, firewall snapshots, and Prometheus snapshots under `${ARTIFACT_ROOT:-${TMPDIR:-/tmp}/katch-package-mirrors-artifacts}`. Root is required only inside disposable client containers for `NET_ADMIN`; host root is not used.

The npm case generates new lockfiles and rejects leaked public registry URLs. Existing lockfiles are not silently claimed compatible: use registry-host replacement where the client supports it, otherwise regenerate them through katch.

Homebrew keeps the approved bottle route `${KATCH_URL%/}/v2/ghcr.io`, enables `HOMEBREW_ARTIFACT_DOMAIN_NO_FALLBACK`, and exercises Tap Git separately. The shared regression case always runs Git. Docker and Podman remain explicitly blocked because no daemon is present in the restricted client namespace; a host daemon would bypass the firewall.

A missing container runtime is a blocked runtime verification, not a pass. The deterministic test remains runnable without a container runtime; the full image build and runtime case matrix must run on `coding.local`.
