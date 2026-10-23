import { ApiError, normalizeHttpError } from '../api/errors.js'

export const ACTIVE_TASK_STATUSES = new Set(['queued', 'running'])
export const TERMINAL_TASK_STATUSES = new Set(['succeeded', 'failed', 'interrupted'])

export function taskPayload(response) {
  const body = response?.data
  return body && typeof body === 'object' && Object.hasOwn(body, 'code') ? body.data : body
}

export function taskListPayload(response) {
  const value = taskPayload(response)
  if (Array.isArray(value)) return value
  if (Array.isArray(value?.items)) return value.items
  throw new ApiError('Task list response has an invalid format', { errorCode: 'INVALID_TASK_RESPONSE', response })
}

export function validateTaskRecord(task, response) {
  if (!task || typeof task !== 'object' || Array.isArray(task)) {
    throw new ApiError('Task response has an invalid format', { errorCode: 'INVALID_TASK_RESPONSE', response })
  }
  const id = task.taskID ?? task.taskId
  if (typeof id !== 'string' || id.length === 0 || (!ACTIVE_TASK_STATUSES.has(task.status) && !TERMINAL_TASK_STATUSES.has(task.status))) {
    throw new ApiError('Task response is missing a valid ID or status', { errorCode: 'INVALID_TASK_RESPONSE', response })
  }
  return task
}

export function createTaskPollingController({
  getTask,
  findTask,
  onUpdate = () => {},
  onConnectionError = () => {},
  onUnauthorized = () => {},
  onTerminal = () => {},
  isCurrentScope = () => true,
  intervalMs = 2000,
  maxRetryMs = 30000,
  setTimer = setTimeout,
  clearTimer = clearTimeout,
}) {
  let timer = null
  let taskId = ''
  let stopped = true
  let failures = 0
  let terminalTaskId = ''
  let requestController = null

  const schedule = (delay) => {
    if (stopped || timer !== null) return
    timer = setTimer(() => {
      timer = null
      void poll()
    }, delay)
  }

  const poll = async () => {
    if (stopped) return
    const currentRequest = new AbortController()
    requestController = currentRequest
    try {
      const response = taskId
        ? await getTask(taskId, { signal: currentRequest.signal })
        : await findTask?.({ signal: currentRequest.signal })
      if (stopped || currentRequest.signal.aborted || !isCurrentScope()) return
      const task = taskId ? taskPayload(response) : response
      if (!task) {
        if (taskId) throw new ApiError('Task response did not contain a task record', { errorCode: 'INVALID_TASK_RESPONSE', response })
        failures = 0
        onConnectionError(null)
        schedule(intervalMs)
        return
      }
      validateTaskRecord(task, response)
      const foundId = task.taskID ?? task.taskId
      if (!taskId && foundId) taskId = foundId
      failures = 0
      onConnectionError(null)
      onUpdate(task)
      if (TERMINAL_TASK_STATUSES.has(task.status)) {
        if (terminalTaskId !== (foundId || taskId)) {
          terminalTaskId = foundId || taskId
          stopped = true
          onTerminal(task)
        }
        return
      }
      schedule(intervalMs)
    } catch (cause) {
      if (stopped || currentRequest.signal.aborted || !isCurrentScope()) return
      const error = normalizeHttpError(cause)
      if (taskId && error.status === 410 && error.errorCode === 'OPERATION_RESULT_EXPIRED') {
        const expired = {
          taskID: taskId,
          status: 'interrupted',
          isDone: true,
          error: error.message,
          recovery: { status: 'unknown' },
        }
        stopped = true
        terminalTaskId = taskId
        onConnectionError(null)
        onUpdate(expired)
        onTerminal(expired)
        return
      }
      if (error.status === 401) {
        stopped = true
        currentRequest.abort()
        onUnauthorized(error)
        return
      }
      failures += 1
      onConnectionError(error)
      const delay = Math.min(intervalMs * (2 ** Math.min(failures, 30)), maxRetryMs)
      schedule(delay)
    } finally {
      if (requestController === currentRequest) requestController = null
    }
  }

  return {
    start(id = '') {
      this.stop()
      taskId = id || ''
      stopped = false
      failures = 0
      terminalTaskId = ''
      void poll()
    },
    stop() {
      stopped = true
      requestController?.abort()
      requestController = null
      if (timer !== null) clearTimer(timer)
      timer = null
    },
    getTaskId() { return taskId },
  }
}
