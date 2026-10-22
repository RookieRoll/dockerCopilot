import { useCallback, useEffect, useRef, useState } from 'react'
import { ApiError, normalizeHttpError } from '../api/errors.js'
import { taskAPI } from '../api/client.js'
import { useQueryClient } from '@tanstack/react-query'
import { createTaskPollingController, taskListPayload, taskPayload, validateTaskRecord } from './taskPolling.js'

export const ACTIVE_TASK_STORAGE_KEY = 'docker_copilot_active_task'

export function taskStorageKeyForScope({ storageKey = ACTIVE_TASK_STORAGE_KEY, containerId, containerName } = {}) {
  const scope = containerId ? `container-id:${containerId}` : (containerName ? `container-name:${containerName}` : '')
  return scope ? `${storageKey}:${encodeURIComponent(scope)}` : storageKey
}

export function taskSessionScopeKey(options = {}) {
  const storageScope = taskStorageKeyForScope(options)
  return `${storageScope}:task:${encodeURIComponent(options.taskId || '')}`
}

export function createTaskScopeGuard(initialScope = '') {
  return {
    current: initialScope,
    isCurrent(scope) { return this.current === scope },
  }
}

function readStoredTaskId(key) {
  try { return globalThis.localStorage?.getItem(key) || '' } catch { return '' }
}

function writeStoredTaskId(key, taskId) {
  try {
    if (taskId) globalThis.localStorage?.setItem(key, taskId)
    else globalThis.localStorage?.removeItem(key)
  } catch {
    // Tracking continues in memory when browser storage is unavailable.
  }
}

export function acceptedTaskId(response) {
  const value = taskPayload(response)
  const id = value?.taskID ?? value?.taskId ?? response?.taskID ?? response?.taskId
  return typeof id === 'string' ? id : ''
}

export async function requestTaskAcceptance(request) {
  const response = typeof request === 'function' ? await request() : await request
  const id = acceptedTaskId(response)
  if (!id) throw new ApiError('Accepted response did not contain a task ID')
  return { taskID: id, response }
}

export function createTaskTerminalHandler({ queryClient, queryKeysRef, handledRef, storageKey, onTerminal, isCurrentScope = () => true }) {
  return (value, fallbackTaskId = '') => {
    if (!isCurrentScope()) return
    const id = value.taskID ?? value.taskId ?? fallbackTaskId
    if (id && !handledRef.current.has(id)) {
      handledRef.current.add(id)
      if (readStoredTaskId(storageKey) === id) writeStoredTaskId(storageKey, '')
      for (const queryKey of queryKeysRef.current) {
        Promise.resolve(queryClient.invalidateQueries({ queryKey })).catch(() => {})
      }
      onTerminal?.(value)
    }
  }
}

export async function findActiveTaskByContainer({ listTasks, containerId, containerName, signal }) {
  const base = {
    ...(containerId ? { containerID: containerId } : {}),
    ...(containerName ? { containerName } : {}),
    page: 1,
    limit: 100,
  }
  const [queuedResponse, runningResponse] = await Promise.all([
    listTasks({ ...base, status: 'queued' }, { signal }),
    listTasks({ ...base, status: 'running' }, { signal }),
  ])
  const queuedRecords = taskListPayload(queuedResponse).map((candidate) => validateTaskRecord(candidate, queuedResponse))
  const runningRecords = taskListPayload(runningResponse).map((candidate) => validateTaskRecord(candidate, runningResponse))
  const queued = queuedRecords.filter((candidate) => candidate.status === 'queued')
  const running = runningRecords.filter((candidate) => candidate.status === 'running')
  const candidates = [...queued, ...running]
  candidates.sort((a, b) => {
    const timeDifference = Date.parse(b.createdAt || '') - Date.parse(a.createdAt || '')
    if (Number.isFinite(timeDifference) && timeDifference !== 0) return timeDifference
    return (b.taskID ?? b.taskId).localeCompare(a.taskID ?? a.taskId)
  })
  return candidates[0] ?? null
}

export function useTask({
  taskId: initialTaskId,
  containerId,
  containerName,
  storageKey = ACTIVE_TASK_STORAGE_KEY,
  invalidateKeys = [],
  onTerminal,
} = {}) {
  const queryClient = useQueryClient()
  const keysRef = useRef(invalidateKeys)
  keysRef.current = invalidateKeys
  const scopedStorageKey = taskStorageKeyForScope({ storageKey, containerId, containerName })
  const sessionScopeKey = taskSessionScopeKey({ storageKey, containerId, containerName, taskId: initialTaskId })
  const scopeGuardRef = useRef(null)
  if (!scopeGuardRef.current) scopeGuardRef.current = createTaskScopeGuard(sessionScopeKey)
  scopeGuardRef.current.current = sessionScopeKey
  const [stateScopeKey, setStateScopeKey] = useState(sessionScopeKey)
  const [taskId, setTaskId] = useState(() => initialTaskId || readStoredTaskId(scopedStorageKey))
  const [task, setTask] = useState(null)
  const [connectionError, setConnectionError] = useState(null)
  const [unauthorized, setUnauthorized] = useState(false)
  const [submitError, setSubmitError] = useState(null)
  const [isTracking, setIsTracking] = useState(false)
  const controllerRef = useRef(null)
  const taskIdRef = useRef(taskId)
  const callbacksRef = useRef({ onTerminal })
  const invalidatedRef = useRef(new Set())
  callbacksRef.current = { onTerminal }

  const track = useCallback((id) => {
    if (!id) return
    if (!scopeGuardRef.current.isCurrent(sessionScopeKey)) {
      writeStoredTaskId(scopedStorageKey, id)
      return
    }
    taskIdRef.current = id
    writeStoredTaskId(scopedStorageKey, id)
    setStateScopeKey(sessionScopeKey)
    setTaskId(id)
    setTask(null)
    setConnectionError(null)
    setUnauthorized(false)
    setSubmitError(null)
    setIsTracking(true)
    controllerRef.current?.start(id)
  }, [scopedStorageKey, sessionScopeKey])

  useEffect(() => {
    const terminalHandler = createTaskTerminalHandler({
      queryClient,
      queryKeysRef: keysRef,
      handledRef: invalidatedRef,
      storageKey: scopedStorageKey,
      isCurrentScope: () => scopeGuardRef.current.isCurrent(sessionScopeKey),
      onTerminal: (value) => callbacksRef.current.onTerminal?.(value),
    })
    const controller = createTaskPollingController({
      getTask: (id, config) => taskAPI.getTask(id, config),
      findTask: async ({ signal } = {}) => {
        if (!containerId && !containerName) return null
        return findActiveTaskByContainer({ listTasks: (params, config) => taskAPI.listTasks(params, config), containerId, containerName, signal })
      },
      isCurrentScope: () => scopeGuardRef.current.isCurrent(sessionScopeKey),
      onUpdate: (value) => {
        if (!scopeGuardRef.current.isCurrent(sessionScopeKey)) return
        setTask(value)
        const id = value.taskID ?? value.taskId
        if (id && id !== taskIdRef.current) {
          taskIdRef.current = id
          writeStoredTaskId(scopedStorageKey, id)
          setTaskId(id)
        }
        setIsTracking(true)
      },
      onConnectionError: setConnectionError,
      onUnauthorized: (error) => {
        if (!scopeGuardRef.current.isCurrent(sessionScopeKey)) return
        setUnauthorized(true)
        setConnectionError(error)
        setIsTracking(false)
      },
      onTerminal: (value) => {
        if (!scopeGuardRef.current.isCurrent(sessionScopeKey)) return
        setTask(value)
        setIsTracking(false)
        terminalHandler(value, controller.getTaskId())
      },
    })
    controllerRef.current = controller
    const id = initialTaskId || readStoredTaskId(scopedStorageKey)
    taskIdRef.current = id
    setStateScopeKey(sessionScopeKey)
    setTaskId(id)
    setTask(null)
    setConnectionError(null)
    setUnauthorized(false)
    setSubmitError(null)
    if (id || containerId || containerName) {
      setIsTracking(true)
      controller.start(id)
    } else {
      setIsTracking(false)
    }
    return () => {
      controller.stop()
      if (controllerRef.current === controller) controllerRef.current = null
    }
  }, [containerId, containerName, initialTaskId, queryClient, scopedStorageKey, sessionScopeKey, track])

  const submit = useCallback(async (request) => {
    if (!scopeGuardRef.current.isCurrent(sessionScopeKey)) return requestTaskAcceptance(request)
    setSubmitError(null)
    setConnectionError(null)
    setUnauthorized(false)
    try {
      // A function is invoked exactly once; failures never replay a POST.
      const accepted = await requestTaskAcceptance(request)
      if (!scopeGuardRef.current.isCurrent(sessionScopeKey)) {
        writeStoredTaskId(scopedStorageKey, accepted.taskID)
        return accepted
      }
      track(accepted.taskID)
      return accepted
    } catch (cause) {
      const error = normalizeHttpError(cause)
      if (scopeGuardRef.current.isCurrent(sessionScopeKey)) setSubmitError(error)
      throw error
    }
  }, [scopedStorageKey, sessionScopeKey, track])

  const clear = useCallback(() => {
    if (!scopeGuardRef.current.isCurrent(sessionScopeKey)) return
    controllerRef.current?.stop()
    writeStoredTaskId(scopedStorageKey, '')
    taskIdRef.current = ''
    setTaskId('')
    setTask(null)
    setConnectionError(null)
    setUnauthorized(false)
    setSubmitError(null)
    setIsTracking(false)
  }, [scopedStorageKey, sessionScopeKey])

  const currentScope = stateScopeKey === sessionScopeKey
  return {
    task: currentScope ? task : null,
    taskId: currentScope ? taskId : (initialTaskId || ''),
    isTracking: currentScope ? isTracking : Boolean(initialTaskId),
    connectionError: currentScope ? connectionError : null,
    unauthorized: currentScope ? unauthorized : false,
    submitError: currentScope ? submitError : null,
    submit, track, clear,
  }
}
