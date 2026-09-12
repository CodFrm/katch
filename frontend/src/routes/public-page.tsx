import { PullAssistant } from '@/components/pull-assistant'
import { SiteHeader } from '@/components/site-header'
import { SiteMetrics } from '@/components/site-metrics'
import { UpstreamTable } from '@/components/upstream-table'
import { useSiteData } from '@/hooks/use-site-data'

/**
 * 前台：拉取助手。
 *
 * 侧栏指标与页脚上游表是站点的名片，站长可以用「公开首页」设置整块关掉。关掉之后
 * 匿名调用方拿到 404，这一页不变成一块错误提示——它首先是个拉取助手，名片没有了
 * 就不画名片。要不要给人看是后端的决定，前端只渲染拿到的东西。
 */
export function PublicPage() {
  const { upstreams, overview, version, loaded } = useSiteData()

  return (
    <div className="bg-background flex min-h-screen flex-col">
      <SiteHeader version={version} />
      <main className="flex flex-1 items-start gap-[72px] p-12">
        <PullAssistant upstreams={upstreams} origin={window.location.origin} ready={loaded} />
        {overview && <SiteMetrics overview={overview} />}
      </main>
      {upstreams && upstreams.length > 0 && <UpstreamTable upstreams={upstreams} />}
    </div>
  )
}
