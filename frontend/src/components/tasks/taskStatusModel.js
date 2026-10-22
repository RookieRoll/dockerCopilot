export const TASK_STATUS_LABELS = Object.freeze({
  queued: '排队中',
  running: '执行中',
  succeeded: '已完成',
  failed: '失败',
  interrupted: '已中断',
})

export function taskStatusLabel(status) {
  return TASK_STATUS_LABELS[status] || '状态未知'
}

function textValue(value) {
  if (typeof value === 'string') return value
  if (value && typeof value === 'object') return typeof value.message === 'string' ? value.message : ''
  return ''
}

export function taskDiagnosticEntries(task) {
  if (!task) return []
  const entries = []
  const detail = textValue(task.detailMsg)
  const error = textValue(task.error)
  if (detail) entries.push({ label: '详细说明', value: detail, tone: task.status === 'failed' || task.status === 'interrupted' ? 'danger' : 'normal' })
  if (error) entries.push({ label: '错误', value: error, tone: 'danger' })
  if (task.recovery && (task.recovery.status || task.recovery.detail)) {
    entries.push({
      label: '恢复结果',
      value: [task.recovery.status, task.recovery.detail].filter(Boolean).join(' · '),
      tone: task.recovery.status === 'unknown' || task.recovery.status === 'failed' ? 'danger' : 'normal',
    })
  }
  const warnings = Array.isArray(task.warnings) ? task.warnings.map(textValue).filter(Boolean) : []
  if (warnings.length) entries.push({ label: '警告', value: warnings, tone: 'danger' })
  return entries
}

export function taskIdentifiers(task) {
  if (!task) return { taskId: '', requestId: '' }
  return {
    taskId: typeof (task.taskID || task.taskId) === 'string' ? (task.taskID || task.taskId) : '',
    requestId: typeof (task.requestID || task.requestId) === 'string' ? (task.requestID || task.requestId) : '',
  }
}

export function taskBatchSummary(task) {
  if (!Array.isArray(task?.items) || task.items.length === 0) return null
  const count = (field, status) => Number.isFinite(Number(task[field]))
    ? Number(task[field])
    : task.items.filter((item) => item?.status === status).length
  return {
    success: count('successCount', 'succeeded'),
    failed: count('failedCount', 'failed'),
    skipped: count('skippedCount', 'skipped'),
    total: task.items.length,
  }
}

export function taskSubmissionOutcome(status) {
  const definitelyRejected = Number.isFinite(status) && status >= 400 && status < 500 && status !== 408 && status !== 429
  return definitelyRejected
    ? { title: '提交未成功', message: '服务已拒绝该请求，请查看错误信息。' }
    : { title: '提交结果待确认', message: '服务可能已接受请求；请先检查任务状态，不要自动重复提交。' }
}
