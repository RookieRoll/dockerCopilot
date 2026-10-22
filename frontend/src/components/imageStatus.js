export function imageUpdatePresentation(check) {
  if (!check || check.known !== true) {
    return { kind: 'unknown', label: '远端更新检查未知', detail: check?.error || '' }
  }
  if (check.stale === true) {
    return { kind: 'stale', label: '更新检查已陈旧', detail: check.error || '' }
  }
  if (check.needUpdate === true) {
    return { kind: 'update', label: '远端发现新镜像', detail: '' }
  }
  return { kind: 'current', label: '检查正常，未发现更新', detail: '' }
}
