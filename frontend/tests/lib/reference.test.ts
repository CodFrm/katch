import { describe, expect, it } from 'vitest'

import { parseReference, type UpstreamRef } from '@/lib/reference'

// 上游表来自公开接口，不是客户端里写死的表——这里模拟接口给出的那一份。
const upstreams: UpstreamRef[] = [
  { host: 'docker.io', protocols: ['registry'], libraryCompletion: true },
  { host: 'ghcr.io', protocols: ['registry'], libraryCompletion: false },
  { host: 'deb.debian.org', protocols: ['static'], libraryCompletion: false },
  { host: 'proxy.golang.org', protocols: ['static'], libraryCompletion: false },
  // 一条同时开了两种协议的记录：多开一种不该把原本那一种挤掉。
  { host: 'github.com', protocols: ['static', 'git'], libraryCompletion: false },
]

const origin = 'https://katch.dev'

describe('parseReference 识别', () => {
  const cases: {
    name: string
    input: string
    host: string
    kind: 'registry' | 'static'
    target: string
    command: string
  }[] = [
    {
      name: '裸镜像名落到默认 registry，并补全 library 命名空间',
      input: 'redis:7',
      host: 'docker.io',
      kind: 'registry',
      target: 'docker.io/library/redis:7',
      command: 'docker pull katch.dev/docker.io/library/redis:7',
    },
    {
      name: '带命名空间的裸镜像名不补 library',
      input: 'grafana/grafana:11.2.0',
      host: 'docker.io',
      kind: 'registry',
      target: 'docker.io/grafana/grafana:11.2.0',
      command: 'docker pull katch.dev/docker.io/grafana/grafana:11.2.0',
    },
    {
      name: '镜像名里的点不会被当成主机名',
      input: 'nginx:1.25',
      host: 'docker.io',
      kind: 'registry',
      target: 'docker.io/library/nginx:1.25',
      command: 'docker pull katch.dev/docker.io/library/nginx:1.25',
    },
    {
      name: 'digest 形态原样保留',
      input: 'redis@sha256:abc123',
      host: 'docker.io',
      kind: 'registry',
      target: 'docker.io/library/redis@sha256:abc123',
      command: 'docker pull katch.dev/docker.io/library/redis@sha256:abc123',
    },
    {
      name: '显式写出 docker.io 的镜像同样补全 library',
      input: 'docker.io/redis:7',
      host: 'docker.io',
      kind: 'registry',
      target: 'docker.io/library/redis:7',
      command: 'docker pull katch.dev/docker.io/library/redis:7',
    },
    {
      name: '不做 library 补全的 registry 原样带过去',
      input: 'ghcr.io/cago-frame/cago:v1',
      host: 'ghcr.io',
      kind: 'registry',
      target: 'ghcr.io/cago-frame/cago:v1',
      command: 'docker pull katch.dev/ghcr.io/cago-frame/cago:v1',
    },
    {
      name: '粘贴的完整 URL 去掉协议后按上游段识别',
      input: 'https://deb.debian.org/debian/dists/bookworm/InRelease',
      host: 'deb.debian.org',
      kind: 'static',
      target: 'deb.debian.org/debian/dists/bookworm/InRelease',
      command: 'curl -fLO https://katch.dev/deb.debian.org/debian/dists/bookworm/InRelease',
    },
    {
      name: '不带协议的静态资源路径也能识别',
      input: 'proxy.golang.org/github.com/gin-gonic/gin/@v/list',
      host: 'proxy.golang.org',
      kind: 'static',
      target: 'proxy.golang.org/github.com/gin-gonic/gin/@v/list',
      command: 'curl -fLO https://katch.dev/proxy.golang.org/github.com/gin-gonic/gin/@v/list',
    },
    {
      name: '只写上游主机名时给出目录形态的命令',
      input: 'https://deb.debian.org/',
      host: 'deb.debian.org',
      kind: 'static',
      target: 'deb.debian.org/',
      command: 'curl -fL https://katch.dev/deb.debian.org/',
    },
    {
      name: '同时开了 static 与 git 的上游，静态资源照常识别成下载命令',
      input: 'https://github.com/foo/bar/releases/download/v1/x.tgz',
      host: 'github.com',
      kind: 'static',
      target: 'github.com/foo/bar/releases/download/v1/x.tgz',
      command: 'curl -fLO https://katch.dev/github.com/foo/bar/releases/download/v1/x.tgz',
    },
    {
      name: '两侧空白与多余斜杠不影响识别',
      input: '  https://deb.debian.org//debian/x  ',
      host: 'deb.debian.org',
      kind: 'static',
      target: 'deb.debian.org/debian/x',
      command: 'curl -fLO https://katch.dev/deb.debian.org/debian/x',
    },
  ]

  for (const c of cases) {
    it(c.name, () => {
      const got = parseReference(c.input, { upstreams, origin })
      expect(got).toMatchObject({
        status: 'ok',
        host: c.host,
        kind: c.kind,
        target: c.target,
        command: c.command,
        verified: true,
      })
    })
  }

  it('识别结果带上写进配置的前缀', () => {
    const got = parseReference('redis:7', { upstreams, origin })
    expect(got).toMatchObject({ prefix: 'https://katch.dev/docker.io' })
  })
})

describe('parseReference 认不出来的输入', () => {
  it('空输入不给任何识别结果', () => {
    expect(parseReference('   ', { upstreams, origin })).toEqual({ status: 'empty' })
  })

  it('不在上游表里的主机报未收录，而不是拼出一条会 404 的命令', () => {
    expect(parseReference('quay.io/prometheus/busybox:latest', { upstreams, origin })).toEqual({
      status: 'unknown',
      host: 'quay.io',
    })
  })

  it('默认 registry 那条记录没开 registry 协议时，裸镜像名认不出来', () => {
    // 裸镜像名只可能落到 registry 上：默认上游只开静态资源时它认不出来。
    const staticDockerIo: UpstreamRef[] = upstreams.map((u) =>
      u.host === 'docker.io' ? { ...u, protocols: ['static'] } : u
    )
    expect(parseReference('redis:7', { upstreams: staticDockerIo, origin })).toEqual({
      status: 'unknown',
      host: 'docker.io',
    })
  })

  it('上游表里没有默认 registry 时，裸镜像名也认不出来', () => {
    const withoutDockerIo = upstreams.filter((u) => u.host !== 'docker.io')
    expect(parseReference('redis:7', { upstreams: withoutDockerIo, origin })).toEqual({
      status: 'unknown',
      host: 'docker.io',
    })
  })

  it('停用后被移出列表的主机同样报未收录', () => {
    const withoutDebian = upstreams.filter((u) => u.host !== 'deb.debian.org')
    expect(
      parseReference('https://deb.debian.org/debian/x', { upstreams: withoutDebian, origin })
    ).toEqual({ status: 'unknown', host: 'deb.debian.org' })
  })
})

// 上游列表被站长关掉时（公开首页设置为 off，匿名调用方拿到 404），
// 拉取助手仍然要能用：识别不再有据可依，但命令照拼，只是不声称「已收录」。
describe('parseReference 拿不到上游表时降级', () => {
  it('裸镜像名仍按默认 registry 拼出命令，但标记为未经核对', () => {
    expect(parseReference('redis:7', { upstreams: null, origin })).toMatchObject({
      status: 'ok',
      host: 'docker.io',
      kind: 'registry',
      target: 'docker.io/library/redis:7',
      command: 'docker pull katch.dev/docker.io/library/redis:7',
      verified: false,
    })
  })

  it('带 tag 的主机形态按 registry 拼，不做 library 补全', () => {
    expect(parseReference('ghcr.io/foo/bar:1', { upstreams: null, origin })).toMatchObject({
      status: 'ok',
      host: 'ghcr.io',
      kind: 'registry',
      target: 'ghcr.io/foo/bar:1',
      command: 'docker pull katch.dev/ghcr.io/foo/bar:1',
      verified: false,
    })
  })

  it('带协议的地址按静态资源拼', () => {
    expect(parseReference('https://quay.io/a/b', { upstreams: null, origin })).toMatchObject({
      status: 'ok',
      host: 'quay.io',
      kind: 'static',
      command: 'curl -fLO https://katch.dev/quay.io/a/b',
      verified: false,
    })
  })
})

describe('parseReference 跟随站点自身的地址', () => {
  it('命令里的主机名取自当前站点，而不是写死的域名', () => {
    expect(parseReference('redis:7', { upstreams, origin: 'http://127.0.0.1:8080' })).toMatchObject(
      {
        command: 'docker pull 127.0.0.1:8080/docker.io/library/redis:7',
        prefix: 'http://127.0.0.1:8080/docker.io',
      }
    )
  })

  it('静态资源的命令保留站点自身的协议', () => {
    expect(
      parseReference('deb.debian.org/debian/x', { upstreams, origin: 'http://127.0.0.1:8080' })
    ).toMatchObject({ command: 'curl -fLO http://127.0.0.1:8080/deb.debian.org/debian/x' })
  })
})
