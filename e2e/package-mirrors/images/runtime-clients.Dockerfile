FROM oven/bun:1.3.11@sha256:0733e50325078969732ebe3b15ce4c4be5082f18c4ac1a0f0ca4839c2e4e42a7 AS bun-dist

FROM node:22.20.0-bookworm-slim@sha256:b21fe589dfbe5cc39365d0544b9be3f1f33f55f3c86c87a76ff65a02f8f5848e AS node
# The base owns Yarn Classic's /usr/local/bin symlinks. Berry stays private.
ARG NPM_VERSION=11.12.1
ARG PNPM_VERSION=11.9.0
ARG YARN_BERRY_VERSION=4.10.3
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
       ca-certificates git iproute2 iptables jq strace util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && npm install --global "npm@${NPM_VERSION}" "pnpm@${PNPM_VERSION}" \
    && npm install --prefix /opt/yarn-berry "@yarnpkg/cli-dist@${YARN_BERRY_VERSION}" \
    && ln -s /opt/yarn-berry/node_modules/@yarnpkg/cli-dist/bin/yarn.js /usr/local/bin/yarn-berry \
    && useradd --create-home --uid 10001 client \
    && install -d -o client -g client /work \
    && command -v iptables >/dev/null \
    && command -v ip6tables >/dev/null \
    && command -v strace >/dev/null \
    && command -v runuser >/dev/null
COPY --from=bun-dist /usr/local/bin/bun /usr/local/bin/bun
COPY e2e/package-mirrors/client-entrypoint.sh /usr/local/bin/katch-client-entrypoint
COPY e2e/package-mirrors/images/client-smoke.sh /usr/local/bin/katch-client-smoke
ENV KATCH_CLIENT_FLAVOR=node KATCH_CLIENT_USER=client
RUN chmod 0555 /usr/local/bin/katch-client-entrypoint /usr/local/bin/katch-client-smoke \
    && /usr/local/bin/katch-client-smoke
USER root
WORKDIR /work
ENTRYPOINT ["/usr/local/bin/katch-client-entrypoint"]

FROM golang:1.26.0-bookworm AS go
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
       ca-certificates git iproute2 iptables jq strace util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 10001 client \
    && install -d -o client -g client /work \
    && command -v iptables >/dev/null \
    && command -v ip6tables >/dev/null \
    && command -v strace >/dev/null \
    && command -v runuser >/dev/null
COPY e2e/package-mirrors/client-entrypoint.sh /usr/local/bin/katch-client-entrypoint
COPY e2e/package-mirrors/images/client-smoke.sh /usr/local/bin/katch-client-smoke
ENV KATCH_CLIENT_FLAVOR=go KATCH_CLIENT_USER=client
RUN chmod 0555 /usr/local/bin/katch-client-entrypoint /usr/local/bin/katch-client-smoke \
    && /usr/local/bin/katch-client-smoke
USER root
WORKDIR /work
ENTRYPOINT ["/usr/local/bin/katch-client-entrypoint"]

FROM python:3.13.7-bookworm AS python
ARG PIP_VERSION=25.2
ARG UV_VERSION=0.8.17
ARG POETRY_VERSION=2.2.1
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
       ca-certificates git iproute2 iptables jq strace util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && python -m pip install --no-cache-dir \
       "pip==${PIP_VERSION}" "uv==${UV_VERSION}" "poetry==${POETRY_VERSION}" \
    && useradd --create-home --uid 10001 client \
    && install -d -o client -g client /work \
    && command -v iptables >/dev/null \
    && command -v ip6tables >/dev/null \
    && command -v strace >/dev/null \
    && command -v runuser >/dev/null
COPY e2e/package-mirrors/client-entrypoint.sh /usr/local/bin/katch-client-entrypoint
COPY e2e/package-mirrors/images/client-smoke.sh /usr/local/bin/katch-client-smoke
ENV KATCH_CLIENT_FLAVOR=python KATCH_CLIENT_USER=client
RUN chmod 0555 /usr/local/bin/katch-client-entrypoint /usr/local/bin/katch-client-smoke \
    && /usr/local/bin/katch-client-smoke
USER root
WORKDIR /work
ENTRYPOINT ["/usr/local/bin/katch-client-entrypoint"]

FROM rust:1.89.0-bookworm AS rust
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
       ca-certificates git iproute2 iptables jq strace util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 10001 client \
    && install -d -o client -g client /work \
    && command -v iptables >/dev/null \
    && command -v ip6tables >/dev/null \
    && command -v strace >/dev/null \
    && command -v runuser >/dev/null
COPY e2e/package-mirrors/client-entrypoint.sh /usr/local/bin/katch-client-entrypoint
COPY e2e/package-mirrors/images/client-smoke.sh /usr/local/bin/katch-client-smoke
ENV KATCH_CLIENT_FLAVOR=rust KATCH_CLIENT_USER=client
RUN chmod 0555 /usr/local/bin/katch-client-entrypoint /usr/local/bin/katch-client-smoke \
    && /usr/local/bin/katch-client-smoke
USER root
WORKDIR /work
ENTRYPOINT ["/usr/local/bin/katch-client-entrypoint"]

FROM maven:3.9.11-eclipse-temurin-21 AS maven-dist
FROM gradle:9.0.0-jdk21-noble AS gradle-dist
FROM sbtscala/scala-sbt:eclipse-temurin-21.0.8_9_1.11.6_3.7.3 AS jvm
USER root
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
       ca-certificates git iproute2 iptables jq strace util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 10001 client \
    && install -d -o client -g client /work \
    && command -v iptables >/dev/null \
    && command -v ip6tables >/dev/null \
    && command -v strace >/dev/null \
    && command -v runuser >/dev/null
COPY --from=maven-dist /usr/share/maven /usr/share/maven
COPY --from=gradle-dist /opt/gradle /opt/gradle
RUN ln -s /usr/share/maven/bin/mvn /usr/local/bin/mvn \
    && ln -s /opt/gradle/bin/gradle /usr/local/bin/gradle
COPY e2e/package-mirrors/client-entrypoint.sh /usr/local/bin/katch-client-entrypoint
COPY e2e/package-mirrors/images/client-smoke.sh /usr/local/bin/katch-client-smoke
ENV KATCH_CLIENT_FLAVOR=jvm KATCH_CLIENT_USER=client
RUN chmod 0555 /usr/local/bin/katch-client-entrypoint /usr/local/bin/katch-client-smoke \
    && /usr/local/bin/katch-client-smoke
USER root
WORKDIR /work
ENTRYPOINT ["/usr/local/bin/katch-client-entrypoint"]

FROM mcr.microsoft.com/dotnet/sdk:8.0.414-bookworm-slim AS dotnet
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
       ca-certificates git iproute2 iptables jq strace util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 10001 client \
    && install -d -o client -g client /work \
    && command -v iptables >/dev/null \
    && command -v ip6tables >/dev/null \
    && command -v strace >/dev/null \
    && command -v runuser >/dev/null
COPY e2e/package-mirrors/client-entrypoint.sh /usr/local/bin/katch-client-entrypoint
COPY e2e/package-mirrors/images/client-smoke.sh /usr/local/bin/katch-client-smoke
ENV KATCH_CLIENT_FLAVOR=dotnet KATCH_CLIENT_USER=client
RUN chmod 0555 /usr/local/bin/katch-client-entrypoint /usr/local/bin/katch-client-smoke \
    && /usr/local/bin/katch-client-smoke
USER root
WORKDIR /work
ENTRYPOINT ["/usr/local/bin/katch-client-entrypoint"]

FROM ruby:3.4.5-bookworm AS ruby
ARG BUNDLER_VERSION=2.7.1
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
       ca-certificates git iproute2 iptables jq strace util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && gem install bundler --version "${BUNDLER_VERSION}" --no-document \
    && useradd --create-home --uid 10001 client \
    && install -d -o client -g client /work \
    && command -v iptables >/dev/null \
    && command -v ip6tables >/dev/null \
    && command -v strace >/dev/null \
    && command -v runuser >/dev/null
COPY e2e/package-mirrors/client-entrypoint.sh /usr/local/bin/katch-client-entrypoint
COPY e2e/package-mirrors/images/client-smoke.sh /usr/local/bin/katch-client-smoke
ENV KATCH_CLIENT_FLAVOR=ruby KATCH_CLIENT_USER=client
RUN chmod 0555 /usr/local/bin/katch-client-entrypoint /usr/local/bin/katch-client-smoke \
    && /usr/local/bin/katch-client-smoke
USER root
WORKDIR /work
ENTRYPOINT ["/usr/local/bin/katch-client-entrypoint"]

FROM debian:12.11-slim AS debian
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
       ca-certificates debian-archive-keyring gpgv iproute2 iptables strace util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && install -d /work \
    && command -v iptables >/dev/null \
    && command -v ip6tables >/dev/null \
    && command -v strace >/dev/null \
    && command -v runuser >/dev/null
COPY e2e/package-mirrors/client-entrypoint.sh /usr/local/bin/katch-client-entrypoint
COPY e2e/package-mirrors/images/client-smoke.sh /usr/local/bin/katch-client-smoke
ENV KATCH_CLIENT_FLAVOR=debian KATCH_CLIENT_USER=root
RUN chmod 0555 /usr/local/bin/katch-client-entrypoint /usr/local/bin/katch-client-smoke \
    && /usr/local/bin/katch-client-smoke
USER root
WORKDIR /work
ENTRYPOINT ["/usr/local/bin/katch-client-entrypoint"]

FROM quay.io/centos/centos:stream9 AS rpm
RUN dnf install -y ca-certificates iproute iptables-nft jq strace util-linux \
    && dnf clean all \
    && install -d /work \
    && command -v iptables >/dev/null \
    && command -v ip6tables >/dev/null \
    && command -v strace >/dev/null \
    && command -v runuser >/dev/null
COPY e2e/package-mirrors/client-entrypoint.sh /usr/local/bin/katch-client-entrypoint
COPY e2e/package-mirrors/images/client-smoke.sh /usr/local/bin/katch-client-smoke
ENV KATCH_CLIENT_FLAVOR=rpm KATCH_CLIENT_USER=root
RUN chmod 0555 /usr/local/bin/katch-client-entrypoint /usr/local/bin/katch-client-smoke \
    && /usr/local/bin/katch-client-smoke
USER root
WORKDIR /work
ENTRYPOINT ["/usr/local/bin/katch-client-entrypoint"]

FROM alpine:3.22.1 AS alpine
RUN apk add --no-cache ca-certificates iproute2 iptables jq strace util-linux \
    && install -d /work \
    && command -v iptables >/dev/null \
    && command -v ip6tables >/dev/null \
    && command -v strace >/dev/null \
    && command -v runuser >/dev/null
COPY e2e/package-mirrors/client-entrypoint.sh /usr/local/bin/katch-client-entrypoint
COPY e2e/package-mirrors/images/client-smoke.sh /usr/local/bin/katch-client-smoke
ENV KATCH_CLIENT_FLAVOR=alpine KATCH_CLIENT_USER=root
RUN chmod 0555 /usr/local/bin/katch-client-entrypoint /usr/local/bin/katch-client-smoke \
    && /usr/local/bin/katch-client-smoke
USER root
WORKDIR /work
ENTRYPOINT ["/usr/local/bin/katch-client-entrypoint"]

FROM composer:2.9.5 AS composer
USER root
RUN apk add --no-cache git iproute2 iptables jq shadow strace util-linux \
    && adduser -D -u 10001 client \
    && install -d -o client -g client /work \
    && command -v iptables >/dev/null \
    && command -v ip6tables >/dev/null \
    && command -v strace >/dev/null \
    && command -v runuser >/dev/null
COPY e2e/package-mirrors/client-entrypoint.sh /usr/local/bin/katch-client-entrypoint
COPY e2e/package-mirrors/images/client-smoke.sh /usr/local/bin/katch-client-smoke
ENV KATCH_CLIENT_FLAVOR=composer KATCH_CLIENT_USER=client
RUN chmod 0555 /usr/local/bin/katch-client-entrypoint /usr/local/bin/katch-client-smoke \
    && /usr/local/bin/katch-client-smoke
USER root
WORKDIR /work
ENTRYPOINT ["/usr/local/bin/katch-client-entrypoint"]
