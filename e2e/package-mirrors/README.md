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

## Runtime acceptance

Every case uses a fresh workdir for each phase. The cold phase runs the normal client workflow to prove metadata resolution and artifact download through katch and must make at least one origin request. The warm phase must perform a real client installation while making zero origin requests.

RubyGems compact-index responses such as `/versions` and `/info/rake` can arrive already older than their 60-second freshness lifetime, so a protocol-correct cache must revalidate them. The RubyGems/Bundler warm phase therefore avoids mutable metadata: it writes a deterministic lockfile, fetches the exact immutable `rake-13.2.1.gem` through katch with `Gem::RemoteFetcher`, then performs fresh native `gem install --local` and frozen `bundle install --local` operations. This still proves a real katch artifact-cache hit without treating stale metadata as reusable.

The Composer case registers `repo.packagist.org`, `api.github.com`, and `codeload.github.com` as explicit `static / composer` upstreams. It disables Composer's install notification and security-advisory blocking, and does not opt into audit, because those optional public services are outside the read-only package mirror contract; package TLS and dist verification remain enabled. `api.github.com` redirects full-commit-SHA dist downloads to codeload, and the warm phase must reuse the cached `Authorization` representation without origin requests. These controls are runtime prerequisites, not evidence that the matrix has passed.

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

Artifacts include per-phase client output, connection records, firewall snapshots, and Prometheus snapshots under `${ARTIFACT_ROOT:-${TMPDIR:-/tmp}/katch-package-mirrors-artifacts}`. Root is required only inside disposable client containers for `NET_ADMIN`; host root is not used.

The npm case generates new lockfiles and rejects leaked public registry URLs. Existing lockfiles are not silently claimed compatible: use registry-host replacement where the client supports it, otherwise regenerate them through katch.

Homebrew uses `${KATCH_URL%/}/registry/ghcr.io` as its artifact domain and enables `HOMEBREW_ARTIFACT_DOMAIN_NO_FALLBACK`. Homebrew 4.6 appends `/v2/<repository>/...` itself, so `/registry/<host>/v2/...` is the explicit boundary that dispatch removes before the registry adapter adds the single upstream `/v2`; a real repository beginning with `v2/` remains untouched. GHCR serves blob bytes through signed redirects to `pkg-containers.githubusercontent.com`, so that exact host must be registered and enabled as `static / none` alongside `ghcr.io` as `registry / none`. Katch does not auto-create this CDN upstream, and a wildcard `githubusercontent.com` registration is neither required nor accepted.

The Homebrew cold phase covers all three protocol surfaces explicitly. With `HOMEBREW_NO_INSTALL_FROM_API=1` set from process startup, it invokes `brew ruby` and Homebrew's `Homebrew::API.fetch_json_api_file(Homebrew::API::Internal.formula_endpoint)` path through `HOMEBREW_API_DOMAIN`. Homebrew therefore downloads only the native system-specific formula JWS (for this image, `internal/formula.x86_64_linux.jws.json`), verifies its PS512 signature with Homebrew's embedded key, and emits compact jq name/version evidence with both the source and Bottle SHA-256 checksums; it does not preload the global formula JWS or fetch the irrelevant cask JWS. The cold phase then probes the mirrored Homebrew/core Git HEAD and installs the standard Bottle from the image-pinned core tap through Katch. The warm phase skips the mutable API and Git checks, keeps the same tap mode, and installs the same formula/Bottle identity. This is deliberate: API/JWS and Git remain governed by their honest HTTP and Git freshness rules, while the immutable Bottle can be reused with zero origin requests. API cache, tap state, package state, `HOME`, and the Cellar remain phase-local rather than being shared to manufacture a warm hit. The pinned client validates the exact cold evidence shape, nonempty version, lowercase 64-hex checksums, Tap evidence, native installed-formula list and JSON, and Bottle install receipt without hardcoding the current jq version. It deliberately does not execute the formula binary because upstream bottles may advance their minimum glibc beyond the pinned image's OS ABI. The shared regression case always runs Git. Docker and Podman remain explicitly blocked because no daemon is present in the restricted client namespace; a host daemon would bypass the firewall.

A missing container runtime is a blocked runtime verification, not a pass. The deterministic test remains runnable without a container runtime; the full image build and runtime case matrix must run on `coding.local`.
