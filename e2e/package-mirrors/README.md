# Package mirror runtime harness

The harness runs each case twice in a fresh, locally built client image. Every image starts as root in the common entrypoint so it can install an IPv4/IPv6 OUTPUT firewall and start `strace`; the command phase then runs as the image contract user with matching `HOME`, `USER`, and `LOGNAME`. Debian APT, CentOS DNF/YUM, Alpine APK, Docker, and Podman stay root because they modify system package, daemon, or container storage state.

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
| docker | Docker 29.2.1 daemon and client | `root` |
| podman | Podman 5.6.2 rootful client | `root` |

The Node base already supplies Yarn Classic 1.22.22. Yarn Berry is installed privately under `/opt/yarn-berry` and exposed only as `yarn-berry`, so the two CLIs cannot replace each other's symlinks.

Build every target and run its exact-version smoke with container networking disabled:

```sh
make package-mirror-image
```

Build and smoke only the dedicated registry clients:

```sh
make package-mirror-registry-images
```

Run deterministic schema, image-contract, embedded-shell, isolation, and connection-capture tests without Docker or Podman:

```sh
make test-package-mirror-harness
```

## Runtime acceptance

Every case uses a fresh workdir for each phase. The cold phase runs the normal client workflow to prove metadata resolution and artifact download through katch and must make at least one origin request. The warm phase must perform a real client installation while making zero origin requests.

RubyGems compact-index responses such as `/versions` and `/info/rake` can arrive already older than their 60-second freshness lifetime, so a protocol-correct cache must revalidate them. The RubyGems/Bundler warm phase therefore avoids mutable metadata: it writes a deterministic lockfile, fetches the exact immutable `rake-13.2.1.gem` through katch with `Gem::RemoteFetcher`, then performs fresh native `gem install --local` and frozen `bundle install --local` operations. This still proves a real katch artifact-cache hit without treating stale metadata as reusable.

The Composer case registers `repo.packagist.org`, `api.github.com`, and `codeload.github.com` as explicit `static / composer` upstreams. It disables Composer's install notification and security-advisory blocking, and does not opt into audit, because those optional public services are outside the read-only package mirror contract; package TLS and dist verification remain enabled. `api.github.com` redirects full-commit-SHA dist downloads to codeload, and the warm phase must reuse the cached `Authorization` representation without origin requests. These controls are runtime prerequisites, not evidence that the matrix has passed.

## Running cases

Case files use the JSON-compatible subset of YAML and the fixed required schema `{name,image,setup,run,assert,required_upstreams}`. They may set the boolean `privileged` field, which defaults to `false`. Only `docker-registry-regression` may set the required `allow_blocked_dns_probe: true` exception described below; `apk` must similarly set `allow_musl_route_probe: true`, and every other case must omit it. The harness writes commands to read-only mounted scripts and never interpolates them through `eval`.

After katch has `site_domain` set to the exact client-visible URL and compatible upstreams enabled, run one or more cases:

```sh
KATCH_URL=http://host.docker.internal:8080 \
KATCH_METRICS_URL=http://127.0.0.1:8080/metrics \
KATCH_HOST=host.docker.internal KATCH_PORT=8080 \
  e2e/package-mirrors/harness.sh npm
```

The harness supplies the same value as both `KATCH_URL` and `KATCH_BASE_URL`. `KATCH_HOST` is injected into `/etc/hosts` with Docker/Podman's `host-gateway`, so bootstrap does not need public DNS. Set `KATCH_ADD_HOST` to a static katch IPv4 address when katch is attached through another bridge.

Each case also receives `KATCH_SHARED=/shared`, backed by a writable `${run_root}/${case}/shared` directory mounted read-write into both its cold and warm containers. The run and case paths are unique, so state cannot cross runs or cases. The directory is empty initially and has no effect unless a case explicitly stores state there; phase workdirs, artifacts, and package installation directories remain separate. The NuGet case opts in only for its documented HTTP metadata cache via `NUGET_HTTP_CACHE_PATH="$KATCH_SHARED/nuget-http-cache"`. It deliberately keeps `--packages "$PWD/packages"` in each fresh phase workdir so the warm phase downloads and validates the nupkg through katch again.

For an HTTPS test endpoint signed by a private test CA, set `KATCH_CA_CERT` to an absolute path to a readable regular certificate file on the host:

```sh
KATCH_CA_CERT=/absolute/path/to/ca.crt \
KATCH_URL=https://host.docker.internal:8443 \
KATCH_METRICS_URL=http://127.0.0.1:8080/metrics \
KATCH_HOST=host.docker.internal KATCH_PORT=8443 \
  e2e/package-mirrors/harness.sh nuget
```

The harness mounts that file read-only at `/run/katch-test-ca.crt` and passes only the mounted path to the client entrypoint. The root entrypoint installs it into the image's system trust store before firewall tracing and before switching to `client` or `linuxbrew`. Leaving `KATCH_CA_CERT` unset adds neither the mount nor the client environment variable. This trust path is required for HTTPS NuGet repository-signature resources; signature validation remains enabled.

Artifacts include per-phase client output, connection records, firewall snapshots, and Prometheus snapshots under `${ARTIFACT_ROOT:-${TMPDIR:-/tmp}/katch-package-mirrors-artifacts}`. The Docker regression also retains accepted blocked route-probe lines in `artifacts/blocked-dns-probes.log`. Root is required only inside disposable standard client containers for `NET_ADMIN`. Standard cases receive only `NET_ADMIN`, `NET_RAW`, and `no-new-privileges`.

The Docker and Podman registry regressions are the only cases with `privileged: true`. For those cases the harness passes `--privileged` and omits the incompatible `no-new-privileges` option and individual capability grants. Before either nested client starts, the entrypoint preserves `/etc/hosts` and the original resolver configuration as artifacts, verifies the exact `KATCH_HOST` static IPv4 mapping, and replaces `/etc/resolv.conf` with only `nameserver 127.0.0.1` plus one-second, single-attempt bounds. No DNS listener or firewall exception is present. Docker's daemon rewrites that loopback resolver to its host gateway; on the privileged dind runner that gateway is the exact Katch IP, and daemon Go threads can report `connect(...:53) = 0` for a UDP route probe even though OUTPUT rejects transfer. Only the Docker case opts into accepting that exact syscall, destination, port, and result tuple; every other DNS destination or result still fails audit, and Podman has no exception. APK's musl resolver likewise emits a successful `connect` route probe to the exact Katch IPv4 or IPv4-mapped address on port 65535 in both phases. Only APK opts into accepting that exact tuple; the default audit and every other syscall, address, port, or result reject it. This is an audit exception only and adds no firewall allowance. Standard cases have one more, for the same reason in a different client library: the entrypoint gives `KATCH_HOST` both an IPv4 and an IPv4-mapped `/etc/hosts` entry, and Go's RFC 6724 address sort connects a UDP socket to each candidate address on port 53 to learn its source address, sending nothing. A trace of `connect,sendto` cannot tell that probe from a DNS query, so the entrypoint snapshots `/etc/resolv.conf` before the client runs and accepts a completed `connect(...) = 0` to the exact Katch address on port 53 only when neither that snapshot nor the resolver after the run lists Katch as a nameserver (`resolv.conf.before` and `resolvers.audited` in the phase artifacts). Failed, split, `sendto`, and non-Katch attempts are still rejected, and privileged cases never receive this allowance: their nested daemons point their own resolver at the gateway, which is Katch. Privileged containers can control kernel-facing resources and can compromise the host if their image or case script is untrusted, so run these pinned images only on a dedicated trusted runner. They still receive no host daemon socket, daemon storage mount, or host container storage. The firewall is installed before the case shell starts; the nested daemon/storage and native client then run in that same container network namespace under the entrypoint's `strace`, with fresh phase-local storage.

The npm case generates new lockfiles and rejects leaked public registry URLs. Existing lockfiles are not silently claimed compatible: use registry-host replacement where the client supports it, otherwise regenerate them through katch.

Homebrew uses `${KATCH_URL%/}/registry/ghcr.io` as its artifact domain and enables `HOMEBREW_ARTIFACT_DOMAIN_NO_FALLBACK`. Homebrew 4.6 appends `/v2/<repository>/...` itself, so `/registry/<host>/v2/...` is the explicit boundary that dispatch removes before the registry adapter adds the single upstream `/v2`; a real repository beginning with `v2/` remains untouched. GHCR serves blob bytes through signed redirects to `pkg-containers.githubusercontent.com`, so that exact host must be registered and enabled as `static / none` alongside `ghcr.io` as `registry / none`. Katch does not auto-create this CDN upstream, and a wildcard `githubusercontent.com` registration is neither required nor accepted.

The Homebrew cold phase covers all three protocol surfaces explicitly. With `HOMEBREW_NO_INSTALL_FROM_API=1` set from process startup, it invokes `brew ruby` and Homebrew's `Homebrew::API.fetch_json_api_file(Homebrew::API::Internal.formula_endpoint)` path through `HOMEBREW_API_DOMAIN`. Homebrew therefore downloads only the native system-specific formula JWS (for this image, `internal/formula.x86_64_linux.jws.json`), verifies its PS512 signature with Homebrew's embedded key, and emits compact jq name/version evidence with both the source and Bottle SHA-256 checksums; it does not preload the global formula JWS or fetch the irrelevant cask JWS. The cold phase then probes the mirrored Homebrew/core Git HEAD and installs the standard Bottle from the image-pinned core tap through Katch. The warm phase skips the mutable API and Git checks, keeps the same tap mode, and installs the same formula/Bottle identity. This is deliberate: API/JWS and Git remain governed by their honest HTTP and Git freshness rules, while the immutable Bottle can be reused with zero origin requests. API cache, tap state, package state, `HOME`, and the Cellar remain phase-local rather than being shared to manufacture a warm hit. The pinned client validates the exact cold evidence shape, nonempty version, lowercase 64-hex checksums, Tap evidence, native installed-formula list and JSON, and Bottle install receipt without hardcoding the current jq version. It deliberately does not execute the formula binary because upstream bottles may advance their minimum glibc beyond the pinned image's OS ABI.

The Git regression uses an ordinary full clone in each fresh phase. After the cold clone and `git fsck`, it polls `git ls-remote` through Katch for up to five minutes, keeping a fresh curl trace per attempt, until an exact case-insensitive `X-Katch-Git: local` response proves that the background bare mirror is ready. Those polls remain part of the cold origin count. The independent warm clone must carry the same local header and make zero origin requests; no checkout or Git client state is shared between phases. It contains no Docker or Podman diagnostics.

The dedicated Docker regression starts Docker 29.2.1's `dockerd` only after the OUTPUT firewall is active, on a phase-local Unix socket with fresh `vfs` storage, no bridge, and daemon iptables disabled. The dedicated Podman regression uses rootful Podman 5.6.2 with phase-local `vfs` root and runroot. Both require `KATCH_CA_CERT`, install that exact certificate for `${KATCH_HOST}:${KATCH_PORT}`, pull `ghcr.io/stefanprodan/podinfo` through katch without disabling TLS, retain native digest verification evidence, and run the pulled `./podinfo --version`. Docker uses 6.9.2 and Podman uses 6.9.1 so a combined run cannot turn the second cold phase into an origin-free tag reuse. Both cases require enabled `ghcr.io` registry and `pkg-containers.githubusercontent.com` static upstreams.

A missing outer container runtime, privileged-container support, nested daemon prerequisites, or test CA is a blocked runtime verification, not a pass. Deterministic tests remain runnable without Docker or Podman; the full image build and runtime case matrix must run on a trusted Linux amd64 runner such as `coding.local`.
