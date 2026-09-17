FROM homebrew/brew@sha256:b0072bfdebf5934ae24b93b44a1928a88057399b3283ffa0177bb86084fdedfd

USER root

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
       iptables strace util-linux \
    && rm -rf /var/lib/apt/lists/*

COPY e2e/package-mirrors/client-entrypoint.sh /usr/local/bin/katch-client-entrypoint

RUN chmod 0555 /usr/local/bin/katch-client-entrypoint

ENV KATCH_CLIENT_USER=linuxbrew

WORKDIR /home/linuxbrew
ENTRYPOINT ["/usr/local/bin/katch-client-entrypoint"]
