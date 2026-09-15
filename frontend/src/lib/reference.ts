/**
 * 把使用者写下的一行资源引用解析成「哪个上游、哪类资源、该敲哪条命令」。
 *
 * 这里是纯函数，不碰 fetch、不碰 DOM：拉取助手整页的正确性都压在这一段字符串
 * 处理上，它必须能被穷举地表驱动测试。上游是否受支持由**接口给出的那一份上游表**
 * 回答，而不是客户端里写死的表——写死的表会在站长加了一个上游之后静默地说
 * 「不支持」，而那正是这台镜像站最该答对的问题。
 */

/** 上游能服务的协议，与后端 upstream_entity 的 Protocol* 常量同一套取值。 */
export type UpstreamProtocol = 'registry' | 'static' | 'git'

/**
 * 一条引用识别成了哪种形态。
 *
 * 它是**这一次识别**的结论，不是上游开着的协议集合：一条上游可以同时开几种，
 * 但一行引用只对应一条命令，要么 docker pull 要么 curl。git 不在里面——clone
 * 的地址照抄就行，助手没有第三条命令要拼。
 */
export type ReferenceKind = 'registry' | 'static'

/** 解析时用得上的上游字段，由公开上游列表接口提供。 */
export interface UpstreamRef {
  host: string
  protocols: UpstreamProtocol[]
  libraryCompletion: boolean
}

export interface ParseOptions {
  /**
   * 公开接口给出的上游表。
   *
   * `null` 表示这份表拿不到（站长把公开首页关掉了，匿名调用方收到 404）。
   * 那种情况下助手不退化成一块错误提示，而是照常拼命令，只是不再声称
   * 「这个上游收录了」——判断权在后端，前端只呈现它给到的东西。
   */
  upstreams: UpstreamRef[] | null
  /** 当前站点地址，形如 `https://katch.dev`。命令里的主机名取自它而不是写死的域名。 */
  origin: string
}

export type ParsedReference =
  | { status: 'empty' }
  | { status: 'unknown'; host: string }
  | {
      status: 'ok'
      host: string
      kind: ReferenceKind
      /** 主机名是否在上游表里核对过。拿不到上游表时为 false。 */
      verified: boolean
      /** 识别出的完整资源引用，形如 `docker.io/library/redis:7`。 */
      target: string
      /** 可直接复制执行的拉取命令。 */
      command: string
      /** 写进配置长期生效时用的前缀，形如 `https://katch.dev/docker.io`。 */
      prefix: string
    }

/**
 * 裸镜像名（`redis:7`）归哪个 registry。
 *
 * 这是 OCI 客户端自己的默认值，不是 katch 的上游表：docker 把不带主机名的引用
 * 解析到 docker.io 是协议侧的既成事实。它仍然要在上游表里查得到才算识别成功。
 */
const DEFAULT_REGISTRY = 'docker.io'

/** `library/` 补全只对单段仓库名成立：`redis` 要补，`grafana/grafana` 不能补。 */
const LIBRARY_NAMESPACE = 'library/'

const SCHEME = /^[a-zA-Z][a-zA-Z0-9+.-]*:\/\//

export function parseReference(input: string, options: ParseOptions): ParsedReference {
  const { upstreams, origin } = options
  const site = origin.replace(/\/+$/, '')
  const siteHost = site.replace(SCHEME, '')

  const raw = input.trim()
  const scheme = SCHEME.exec(raw)
  const body = collapseSlashes((scheme ? raw.slice(scheme[0].length) : raw).replace(/^\/+/, ''))
  if (body === '') {
    return { status: 'empty' }
  }

  const slash = body.indexOf('/')
  const head = slash === -1 ? body : body.slice(0, slash)
  const tail = slash === -1 ? '' : body.slice(slash)

  const find = (host: string) => upstreams?.find((u) => u.host === host) ?? null
  // 第一段是主机名的三种依据：写了协议、后面还有路径且这一段长得像主机名、
  // 或者它就明明白白是上游表里的一条记录（有人只写了主机名）。
  const headIsHost = scheme !== null || (slash !== -1 && looksLikeHost(head)) || !!find(head)

  const host = headIsHost ? head : DEFAULT_REGISTRY
  const entry = find(host)
  if (upstreams !== null && entry === null) {
    return { status: 'unknown', host }
  }
  if (upstreams !== null && !headIsHost && !entry!.protocols.includes('registry')) {
    // 裸镜像名只可能落到 registry 上：默认上游没开 registry 时它认不出来。
    return { status: 'unknown', host }
  }

  const path = headIsHost ? tail : '/' + body
  const kind = resolveKind(entry, scheme !== null, path)
  const verified = entry !== null

  if (kind === 'registry') {
    const libraryCompletion = entry ? entry.libraryCompletion : host === DEFAULT_REGISTRY
    const repo = path.replace(/^\/+/, '')
    if (repo === '') {
      // 只写了 registry 主机名，还没写要拉什么——没什么可识别的。
      return { status: 'empty' }
    }
    const completed = libraryCompletion && !repo.includes('/') ? LIBRARY_NAMESPACE + repo : repo
    const target = `${host}/${completed}`
    return {
      status: 'ok',
      host,
      kind,
      verified,
      target,
      command: `docker pull ${siteHost}/${target}`,
      prefix: `${site}/${host}`,
    }
  }

  const target = `${host}${path}`
  // -O 取远端文件名，路径落在目录上时没有文件名可取，那条命令会直接报错。
  const download = path !== '' && !path.endsWith('/') ? '-fLO' : '-fL'
  return {
    status: 'ok',
    host,
    kind,
    verified,
    target,
    command: `curl ${download} ${site}/${target}`,
    prefix: `${site}/${host}`,
  }
}

/**
 * 第一段像不像主机名：含点或含冒号（端口）。
 *
 * 与后端 dispatch.Classify 的判据一致（决策 2：公网主机名必然含点），差别只在
 * 这里还认端口形态，因为使用者会把 `localhost:5000/x` 这种私有 registry 粘进来。
 */
function looksLikeHost(segment: string): boolean {
  return segment.includes('.') || segment.includes(':') || segment === 'localhost'
}

/**
 * 这一行引用落到哪种形态上。
 *
 * 先按写法猜，再拿上游开着的协议去校：一条同时开了 static 与 git 的记录，
 * 静态资源照常拼成下载命令；一条只开 registry 的记录，即使写法不像镜像引用
 * （比如没写 tag）也只能是镜像引用。上游表拿不到时只剩猜这一条路。
 *
 * registry 优先于 static，与「裸镜像名只落到 registry」那一条同向：两种都开着
 * 的记录上，猜不出来的写法按镜像引用处理。
 */
function resolveKind(entry: UpstreamRef | null, hadScheme: boolean, path: string): ReferenceKind {
  const guessed = guessKind(hadScheme, path)
  if (entry === null || entry.protocols.includes(guessed)) {
    return guessed
  }
  return entry.protocols.includes('registry') ? 'registry' : 'static'
}

/**
 * 只从写法上猜这是哪种引用。
 *
 * 只有两条依据：写了协议的是资源地址；末段带 tag 或 digest 的是镜像引用。
 * 上游表拿不到时这就是全部依据，猜错的代价是一条拉不动的命令，而不是一个假的
 * 「不支持」——所以宁可猜。
 */
function guessKind(hadScheme: boolean, path: string): ReferenceKind {
  if (hadScheme) {
    return 'static'
  }
  const last = path.slice(path.lastIndexOf('/') + 1)
  return last.includes(':') || last.includes('@') ? 'registry' : 'static'
}

/** 粘贴来的地址常带着重复斜杠，它们在上游那侧是同一个对象。 */
function collapseSlashes(path: string): string {
  return path.replace(/\/{2,}/g, '/')
}
