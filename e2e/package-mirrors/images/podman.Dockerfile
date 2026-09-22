FROM quay.io/podman/stable:v5.6.2@sha256:28c72e39a70b8a6e2b567efe1b34e53850ea77b4c7c1538e41fe5a138055566d AS podman
USER root
RUN dnf install -y ca-certificates iptables-nft shadow-utils strace util-linux \
    && dnf clean all \
    && install -d /work \
    && command -v podman >/dev/null \
    && command -v iptables >/dev/null \
    && command -v ip6tables >/dev/null \
    && command -v strace >/dev/null \
    && command -v runuser >/dev/null
COPY e2e/package-mirrors/client-entrypoint.sh /usr/local/bin/katch-client-entrypoint
COPY e2e/package-mirrors/images/client-smoke.sh /usr/local/bin/katch-client-smoke
ENV KATCH_CLIENT_FLAVOR=podman KATCH_CLIENT_USER=root
RUN chmod 0555 /usr/local/bin/katch-client-entrypoint /usr/local/bin/katch-client-smoke \
    && /usr/local/bin/katch-client-smoke
USER root
WORKDIR /work
ENTRYPOINT ["/usr/local/bin/katch-client-entrypoint"]
