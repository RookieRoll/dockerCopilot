import React from 'react'
import { AlertTriangle, CheckCircle2, CircleDashed, Clipboard, LoaderCircle, XCircle } from 'lucide-react'
import { taskDiagnosticEntries, taskIdentifiers, taskStatusLabel, taskSubmissionOutcome } from './taskStatusModel.js'

const STATUS_STYLES = {
  queued: 'border-blue-200 bg-blue-50 text-blue-800 dark:border-blue-900 dark:bg-blue-950/40 dark:text-blue-200',
  running: 'border-blue-200 bg-blue-50 text-blue-800 dark:border-blue-900 dark:bg-blue-950/40 dark:text-blue-200',
  succeeded: 'border-green-200 bg-green-50 text-green-800 dark:border-green-900 dark:bg-green-950/40 dark:text-green-200',
  failed: 'border-red-200 bg-red-50 text-red-800 dark:border-red-900 dark:bg-red-950/40 dark:text-red-200',
  interrupted: 'border-amber-200 bg-amber-50 text-amber-900 dark:border-amber-900 dark:bg-amber-950/40 dark:text-amber-200',
}

const STATUS_ICONS = {
  queued: CircleDashed,
  running: LoaderCircle,
  succeeded: CheckCircle2,
  failed: XCircle,
  interrupted: AlertTriangle,
}

function CopyValue({ label, value }) {
  const [copied, setCopied] = React.useState(false)
  if (!value) return null

  const copy = async (event) => {
    event.stopPropagation()
    try {
      if (navigator.clipboard?.writeText) {
        await navigator.clipboard.writeText(String(value))
      } else {
        const input = document.createElement('textarea')
        input.value = String(value)
        input.setAttribute('readonly', '')
        input.style.position = 'fixed'
        input.style.opacity = '0'
        document.body.appendChild(input)
        input.select()
        const copied = document.execCommand('copy')
        input.remove()
        if (!copied) throw new Error('Clipboard copy unavailable')
      }
      setCopied(true)
      window.setTimeout(() => setCopied(false), 1400)
    } catch {
      setCopied(false)
    }
  }

  return (
    <span className="inline-flex max-w-full items-center gap-1.5 rounded bg-white/70 px-2 py-1 font-mono text-[11px] dark:bg-black/20">
      <span className="shrink-0 font-sans font-medium">{label}</span>
      <code className="min-w-0 truncate" title={value}>{value}</code>
      <button
        type="button"
        className="inline-flex shrink-0 items-center gap-1 rounded px-1 py-0.5 font-sans text-gray-600 hover:bg-black/5 focus-visible:outline focus-visible:outline-2 focus-visible:outline-primary-500 dark:text-gray-300 dark:hover:bg-white/10"
        aria-label={`复制${label}`}
        onClick={copy}
      >
        <Clipboard className="h-3 w-3" aria-hidden="true" />
        {copied ? '已复制' : '复制'}
      </button>
    </span>
  )
}

function TextBlock({ label, children, tone = 'normal' }) {
  if (!children) return null
  const toneClass = tone === 'danger'
    ? 'border-red-200 bg-white/60 dark:border-red-900 dark:bg-black/10'
    : 'border-gray-200 bg-white/60 dark:border-gray-700 dark:bg-black/10'
  return (
    <div className={`rounded-md border p-2 ${toneClass}`}>
      <div className="mb-1 text-[11px] font-semibold uppercase tracking-wide opacity-70">{label}</div>
      <div className="break-words whitespace-pre-wrap text-xs leading-relaxed">{children}</div>
    </div>
  )
}

export function TaskStatusPanel({ task, taskId = '', connectionError, unauthorized = false, submitError, isTracking = false, containerName = '' }) {
  if (!task && !connectionError && !unauthorized && !submitError && !isTracking) return null

  const status = task?.status
  const Icon = STATUS_ICONS[status] || (connectionError || unauthorized || submitError ? AlertTriangle : CircleDashed)
  const active = status === 'queued' || status === 'running'
  const tone = STATUS_STYLES[status] || 'border-gray-200 bg-gray-50 text-gray-800 dark:border-gray-700 dark:bg-gray-900/60 dark:text-gray-200'
  const diagnostics = taskDiagnosticEntries(task)
  const identifiers = taskIdentifiers(task)
  const message = typeof task?.message === 'string' ? task.message : ''
  const submitOutcome = taskSubmissionOutcome(submitError?.status)

  return (
    <section className="my-4 rounded-xl border border-gray-200 bg-white shadow-sm dark:border-gray-700 dark:bg-gray-800" aria-label="任务状态" data-testid="task-status-panel">
      <div className={`flex flex-wrap items-center justify-between gap-3 rounded-t-xl border-b px-4 py-3 ${tone}`}>
        <div className="flex min-w-0 items-center gap-2">
          <Icon className={`h-4 w-4 shrink-0 ${active && status === 'running' ? 'animate-spin' : ''}`} aria-hidden="true" />
          <div className="min-w-0">
            <h3 className="text-sm font-semibold">{status ? `任务${taskStatusLabel(status)}` : connectionError || unauthorized ? '任务状态待确认' : submitError ? submitOutcome.title : '容器更新任务'}</h3>
            {containerName && <p className="truncate text-xs opacity-80">容器：{containerName}</p>}
          </div>
        </div>
        {task?.percentage !== undefined && active && (
          <span className="shrink-0 text-xs font-semibold">{Math.max(0, Math.min(100, Number(task.percentage) || 0))}%</span>
        )}
      </div>

      <div className="space-y-3 p-4">
        {!task && isTracking && !connectionError && !unauthorized && <p className="text-sm text-gray-600 dark:text-gray-300">正在查询任务状态…</p>}
        {task?.stage && <p className="text-sm"><span className="font-medium">阶段：</span>{task.stage}</p>}
        {message && <p className="text-sm text-gray-700 dark:text-gray-200">{message}</p>}

        {(connectionError || unauthorized) && (
          <div role="alert" className="rounded-md border border-amber-300 bg-amber-50 p-3 text-sm text-amber-900 dark:border-amber-800 dark:bg-amber-950/40 dark:text-amber-200">
            <p className="font-semibold">{unauthorized ? '登录状态已失效，任务结果待确认' : '连接中断，任务结果待确认'}</p>
            <p className="mt-1">{unauthorized ? '请重新登录后再查询；服务已停止本页轮询。任务结果需要重新确认，请勿据此重复提交。' : '页面会按退避间隔继续查询；网络或查询错误不代表任务失败，请勿据此重复提交。'}</p>
            {connectionError?.message && <p className="mt-2 break-words text-xs">查询错误：{connectionError.message}</p>}
            <CopyValue label="请求 ID" value={connectionError?.requestId} />
          </div>
        )}

        {submitError && (
          <div role="alert" className="rounded-md border border-amber-300 bg-amber-50 p-3 text-sm text-amber-900 dark:border-amber-800 dark:bg-amber-950/40 dark:text-amber-200">
            <p className="font-semibold">{submitOutcome.title}</p>
            <p className="mt-1">{submitOutcome.message}</p>
            {submitError.message && <p className="mt-2 break-words text-xs">{submitError.message}</p>}
            <CopyValue label="请求 ID" value={submitError.requestId} />
          </div>
        )}

        {(task || taskId) && (
          <div className="flex flex-wrap gap-2">
            <CopyValue label="任务 ID" value={identifiers.taskId || taskId} />
            <CopyValue label="请求 ID" value={identifiers.requestId} />
          </div>
        )}

        {diagnostics.map(({ label, value, tone: entryTone }) => (
          <TextBlock key={label} label={label} tone={entryTone}>
            {Array.isArray(value)
              ? <ul className="list-disc space-y-1 pl-4">{value.map((warning, index) => <li key={`${index}-${warning}`}>{warning}</li>)}</ul>
              : value}
          </TextBlock>
        ))}
        {task?.requiresAttention && <p className="text-sm font-medium text-amber-800 dark:text-amber-300">该任务仍需人工核对处理。</p>}
      </div>
    </section>
  )
}

export default TaskStatusPanel
