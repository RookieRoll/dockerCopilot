const ACTIVE_STATUSES = new Set(['queued', 'running'])
const TERMINAL_STATUSES = new Set(['succeeded', 'failed', 'interrupted'])

export function newProgramUpdateRequestId(cryptoSource = globalThis.crypto) {
  // getRandomValues also works on HTTP LAN deployments where randomUUID is
  // unavailable because the page is not a secure context.
  const bytes = cryptoSource.getRandomValues(new Uint8Array(16))
  bytes[6] = (bytes[6] & 15) | 64
  bytes[8] = (bytes[8] & 63) | 128
  const hex = Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join('')
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`
}

function submissionIsDefinitelyRejected(error) {
  const status = error?.status
  return Number.isFinite(status) && status >= 400 && status < 500 && status !== 408 && status !== 429
}

// A saved task ID without a terminal server record is unresolved. In particular,
// a lost connection during restart must never make the update look complete or
// permit an automatic second PUT.
export function programUpdateState({ task, taskId = '', isSubmitting = false, submitError, discoveryPending = false } = {}) {
  if (isSubmitting) return 'submitting'
  if (ACTIVE_STATUSES.has(task?.status)) return 'active'
  if (submitError && !submissionIsDefinitelyRejected(submitError)) return 'uncertain'
  if (submitError && submissionIsDefinitelyRejected(submitError)) return 'rejected'
  if (discoveryPending && !TERMINAL_STATUSES.has(task?.status)) return 'uncertain'
  if (taskId && !TERMINAL_STATUSES.has(task?.status)) return 'uncertain'
  if (task?.status === 'succeeded') return 'succeeded'
  if (task?.status === 'failed') return 'failed'
  if (task?.status === 'interrupted') return 'interrupted'
  return 'idle'
}
