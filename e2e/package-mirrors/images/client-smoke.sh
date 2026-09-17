#!/bin/sh
set -eu

expect_version() {
  client=$1
  expected=$2
  actual=$3
  if [ "$actual" != "$expected" ]; then
    printf '%s\n' "$client version mismatch: expected $expected, got $actual" >&2
    exit 1
  fi
  printf '%s=%s\n' "$client" "$actual"
}

case ${KATCH_CLIENT_FLAVOR:?KATCH_CLIENT_FLAVOR is required} in
  node)
    expect_version node 22.20.0 "$(node --version | sed 's/^v//')"
    expect_version npm 11.12.1 "$(npm --version)"
    expect_version pnpm 11.9.0 "$(pnpm --version)"
    expect_version yarn 1.22.22 "$(yarn --version)"
    expect_version yarn-berry 4.10.3 "$(yarn-berry --version)"
    expect_version bun 1.3.11 "$(bun --version)"
    ;;
  go)
    expect_version go 1.26.0 "$(go version | awk '{ sub(/^go/, "", $3); print $3 }')"
    ;;
  python)
    expect_version python 3.13.7 "$(python --version | awk '{ print $2 }')"
    expect_version pip 25.2 "$(python -m pip --version | awk '{ print $2 }')"
    expect_version uv 0.8.17 "$(uv --version | awk '{ print $2 }')"
    expect_version poetry 2.2.1 "$(poetry --version | sed -n 's/^Poetry (version \([^)]*\))$/\1/p')"
    ;;
  rust)
    expect_version rust 1.89.0 "$(rustc --version | awk '{ print $2 }')"
    expect_version cargo 1.89.0 "$(cargo --version | awk '{ print $2 }')"
    ;;
  jvm)
    expect_version java 21.0.8 "$(java -version 2>&1 | awk -F '"' 'NR == 1 { print $2 }')"
    expect_version maven 3.9.11 "$(mvn --version | awk 'NR == 1 { print $3 }')"
    expect_version gradle 9.0.0 "$(gradle --version | awk '$1 == "Gradle" { print $2 }')"
    expect_version sbt 1.11.6 "$(sbt --numeric-version)"
    ;;
  dotnet)
    expect_version dotnet 8.0.414 "$(dotnet --version)"
    ;;
  ruby)
    expect_version ruby 3.4.5 "$(ruby --version | awk '{ print $2 }')"
    expect_version bundler 2.7.1 "$(bundle --version | awk '{ print $3 }')"
    ;;
  debian)
    expect_version apt 2.6.1 "$(apt-get --version | awk 'NR == 1 { print $2 }')"
    ;;
  rpm)
    expect_version dnf 4.14.0 "$(dnf --version | awk 'NR == 1 { print $1 }')"
    expect_version yum 4.14.0 "$(yum --version | awk 'NR == 1 { print $1 }')"
    ;;
  alpine)
    expect_version apk 2.14.9 "$(apk --version | awk '{ sub(/,.*/, "", $2); print $2 }')"
    ;;
  composer)
    expect_version composer 2.9.5 "$(composer --version --no-ansi | awk '{ print $3 }')"
    ;;
  homebrew)
    expect_version brew 4.6.20 "$(brew --version | awk 'NR == 1 { print $2 }')"
    ;;
  *)
    printf '%s\n' "unknown client flavor: $KATCH_CLIENT_FLAVOR" >&2
    exit 64
    ;;
esac
