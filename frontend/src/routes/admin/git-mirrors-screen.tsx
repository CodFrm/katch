import { useTranslation } from 'react-i18next'

import { ScreenHeader } from '@/components/admin/admin-shell'
import { useAdminAction } from '@/hooks/use-admin-action'
import { useGitMirrors } from '@/hooks/use-admin-data'
import { deleteGitMirror, type GitMirrorItem } from '@/lib/api'
import { formatBytes, formatCount, formatStamp } from '@/lib/format'
import { cn } from '@/lib/utils'

/** 每种状态的点该是什么颜色，同 upstream-table 的「点 + 文字」样式。 */
const STATE_TONES: Record<GitMirrorItem['state'], string> = {
  pending: 'bg-ink-3',
  ready: 'bg-ok',
  failed: 'bg-destructive',
  rejected: 'bg-warn',
}

/**
 * git 本地镜像：谁被拉过就镜像谁（决策 7），这里是站长能看到、能删掉的那一面。
 *
 * 不做搜索与分页：镜像的规模有总配额顶着，不会长成对象缓存那种几十万条的表
 * （同 useGitMirrors 的理由）。删除也不需要二次确认——同规则表的删除，
 * 被删的仓库只是回到「没有镜像」的状态，下次拉取会重新走穿透并在后台重建。
 */
export function GitMirrorsScreen({
  adminKey,
  onUnauthorized,
}: {
  adminKey: string
  onUnauthorized: () => void
}) {
  const { t } = useTranslation()
  const mirrors = useGitMirrors(adminKey, onUnauthorized)
  const action = useAdminAction(onUnauthorized)

  function remove(mirror: GitMirrorItem) {
    void action.run(
      () => deleteGitMirror(adminKey, mirror.id),
      () => mirrors.reload()
    )
  }

  return (
    <>
      <ScreenHeader
        title={t('admin.git.title')}
        subtitle={
          <span className="text-muted-foreground font-mono">
            {t('admin.git.count', { count: formatCount(mirrors.data.length) })}
          </span>
        }
      />

      <div className="flex flex-col gap-4 px-8 py-5">
        {action.errorKey && (
          <p role="alert" className="text-destructive text-[12.5px]">
            {t(action.errorKey)}
          </p>
        )}

        <table aria-label={t('admin.git.title')} className="w-full text-left">
          <thead>
            <tr className="border-border text-ink-3 border-b text-[11px] tracking-[0.12em]">
              <th scope="col" className="py-2 font-normal">
                {t('admin.git.columns.repo')}
              </th>
              <th scope="col" className="w-[110px] py-2 font-normal">
                {t('admin.git.columns.state')}
              </th>
              <th scope="col" className="w-[100px] py-2 font-normal">
                {t('admin.git.columns.size')}
              </th>
              <th scope="col" className="w-[130px] py-2 font-normal">
                {t('admin.git.columns.lastSync')}
              </th>
              <th scope="col" className="w-[130px] py-2 font-normal">
                {t('admin.git.columns.lastAccess')}
              </th>
              <th scope="col" className="w-[70px] py-2 font-normal">
                {t('admin.git.columns.operations')}
              </th>
            </tr>
          </thead>
          <tbody>
            {mirrors.data.map((mirror) => (
              <MirrorRow key={mirror.id} mirror={mirror} onDelete={() => remove(mirror)} />
            ))}
          </tbody>
        </table>
        {mirrors.data.length === 0 && (
          <p className="text-muted-foreground text-[13px]">{t('admin.git.empty')}</p>
        )}
      </div>
    </>
  )
}

function MirrorRow({ mirror, onDelete }: { mirror: GitMirrorItem; onDelete: () => void }) {
  const { t } = useTranslation()
  const size = formatBytes(mirror.size_bytes)

  function stampOf(seconds: number): string {
    if (seconds <= 0) {
      return t('admin.git.neverSynced')
    }
    const stamp = formatStamp(seconds)
    return stamp.day === 'today'
      ? stamp.time
      : stamp.day === 'yesterday'
        ? t('admin.events.yesterday', { time: stamp.time })
        : `${stamp.date} ${stamp.time}`
  }

  return (
    <tr className="border-border border-b">
      <td className="text-foreground py-2.5 font-mono text-[13px]">
        {mirror.host}
        {mirror.repo}
      </td>
      <td className="py-2.5">
        <span className={cn('flex items-center gap-[7px] text-[12.5px]', 'text-foreground')}>
          <span
            aria-hidden="true"
            className={cn('size-1.5 rounded-full', STATE_TONES[mirror.state])}
          />
          {t(`admin.git.state.${mirror.state}`)}
        </span>
      </td>
      <td className="text-muted-foreground py-2.5 font-mono text-[13px]">
        {size.value} {size.unit}
      </td>
      <td className="text-ink-3 py-2.5 font-mono text-xs">{stampOf(mirror.last_sync_at)}</td>
      <td className="text-ink-3 py-2.5 font-mono text-xs">{stampOf(mirror.last_access_at)}</td>
      <td className="py-2.5">
        <button
          type="button"
          onClick={onDelete}
          className="text-muted-foreground hover:text-destructive text-[12.5px]"
        >
          {t('admin.git.delete')}
        </button>
      </td>
    </tr>
  )
}
