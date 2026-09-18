FROM docker:29.2.1-dind@sha256:68f6d9ab84623d1116c5432a3b924a07ee09960e6129ca1cb03ef14010588cb4 AS docker
RUN apk add --no-cache ca-certificates iptables shadow strace util-linux \
    && install -d /work \
    && command -v docker >/dev/null \
    && command -v dockerd >/dev/null \
    && command -v iptables >/dev/null \
    && command -v ip6tables >/dev/null \
    && command -v strace >/dev/null \
    && command -v runuser >/dev/null
COPY e2e/package-mirrors/client-entrypoint.sh /usr/local/bin/katch-client-entrypoint
COPY e2e/package-mirrors/images/client-smoke.sh /usr/local/bin/katch-client-smoke
ENV KATCH_CLIENT_FLAVOR=docker KATCH_CLIENT_USER=root
RUN chmod 0555 /usr/local/bin/katch-client-entrypoint /usr/local/bin/katch-client-smoke \
    && /usr/local/bin/katch-client-smoke
USER root
WORKDIR /work
ENTRYPOINT ["/usr/local/bin/katch-client-entrypoint"]
