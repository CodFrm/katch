FROM homebrew/brew@sha256:b0072bfdebf5934ae24b93b44a1928a88057399b3283ffa0177bb86084fdedfd AS homebrew

USER root

RUN rm -f /etc/apt/sources.list.d/github-cli.list /etc/apt/sources.list.d/github-cli.sources \
    && apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
       iptables strace util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && command -v iptables >/dev/null \
    && command -v ip6tables >/dev/null \
    && command -v strace >/dev/null \
    && command -v runuser >/dev/null

COPY e2e/package-mirrors/client-entrypoint.sh /usr/local/bin/katch-client-entrypoint
COPY e2e/package-mirrors/images/client-smoke.sh /usr/local/bin/katch-client-smoke

ENV KATCH_CLIENT_FLAVOR=homebrew KATCH_CLIENT_USER=linuxbrew

RUN chmod 0555 /usr/local/bin/katch-client-entrypoint /usr/local/bin/katch-client-smoke \
    && /usr/local/bin/katch-client-smoke

USER root

WORKDIR /home/linuxbrew
ENTRYPOINT ["/usr/local/bin/katch-client-entrypoint"]
