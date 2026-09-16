FROM oven/bun:1.3.11@sha256:0733e50325078969732ebe3b15ce4c4be5082f18c4ac1a0f0ca4839c2e4e42a7 AS bun

FROM node:22.20.0-bookworm-slim@sha256:b21fe589dfbe5cc39365d0544b9be3f1f33f55f3c86c87a76ff65a02f8f5848e

ARG NPM_VERSION=11.12.1
ARG PNPM_VERSION=11.9.0
ARG YARN_CLASSIC_VERSION=1.22.22
ARG YARN_BERRY_VERSION=4.10.3

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
       ca-certificates git iproute2 iptables jq strace util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && npm install --global \
       "npm@${NPM_VERSION}" \
       "pnpm@${PNPM_VERSION}" \
       "yarn@${YARN_CLASSIC_VERSION}" \
       "@yarnpkg/cli-dist@${YARN_BERRY_VERSION}" \
    && ln -s /usr/local/lib/node_modules/@yarnpkg/cli-dist/bin/yarn.js /usr/local/bin/yarn-berry \
    && useradd --create-home --uid 10001 client \
    && install -d -o client -g client /work

COPY --from=bun /usr/local/bin/bun /usr/local/bin/bun
COPY e2e/package-mirrors/client-entrypoint.sh /usr/local/bin/katch-client-entrypoint

RUN chmod 0555 /usr/local/bin/katch-client-entrypoint

WORKDIR /work
ENTRYPOINT ["/usr/local/bin/katch-client-entrypoint"]
