import { useState, useCallback } from 'react'
import { useQuery } from '@tanstack/react-query'
import { versionAPI } from '../api/client.js'
import { useTask } from './useTask.js'
import { programUpdateState, newProgramUpdateRequestId } from './programUpdateModel.js'
import { normalizeHttpError } from '../api/errors.js'
import { shouldUpdate } from './versionUtils.js'

export const PROGRAM_UPDATE_TASK_STORAGE_KEY = 'docker_copilot_program_update_task'
const PROGRAM_UPDATE_ATTEMPT_STORAGE_KEY = 'docker_copilot_program_update_pending_acceptance'

function readPendingAcceptance() {
  try {
    const value = globalThis.localStorage?.getItem(PROGRAM_UPDATE_ATTEMPT_STORAGE_KEY)
    return value || ''
  } catch { return '' }
}

function writePendingAcceptance(startedAt) {
  try {
    if (startedAt) globalThis.localStorage?.setItem(PROGRAM_UPDATE_ATTEMPT_STORAGE_KEY, String(startedAt))
    else globalThis.localStorage?.removeItem(PROGRAM_UPDATE_ATTEMPT_STORAGE_KEY)
  } catch {
    // Task tracking remains available in memory when browser storage is blocked.
  }
}

function requireVersionPayload(response, kind) {
  if (response?.data?.code !== 0 && response?.data?.code !== 200) {
    throw new Error(response?.data?.msg || `${kind}版本接口返回错误`)
  }
  const value = response.data.data
  const version = typeof value === 'string'
    ? value
    : kind === '本地' ? value?.version : value?.remoteVersion
  if (typeof version !== 'string' || !version) throw new Error(`${kind}版本信息缺失`)
  return { version, buildDate: typeof value === 'object' && value ? value.buildDate || '' : '' }
}

/**
 * 版本检查 Hook
 * 用于检查后端版本，并提示用户是否有更新
 */
export function useVersionCheck({ enabled = true } = {}) {
  const [showUpdatePrompt, setShowUpdatePrompt] = useState(false)
  const [isSubmittingUpdate, setIsSubmittingUpdate] = useState(false)
  const [programTaskRequestId, setProgramTaskRequestId] = useState(readPendingAcceptance)
  const handleProgramTaskTerminal = useCallback((task) => {
    writePendingAcceptance(false)
    setProgramTaskRequestId('')
    if (task?.status === 'succeeded') {
      // A queued/accepted task is not an installation result. Reload only after
      // the backend has durably confirmed the handoff and reports success.
      globalThis.setTimeout?.(() => globalThis.location?.reload(), 3000)
    } else if (task?.status === 'failed' || task?.status === 'interrupted') {
      globalThis.alert?.('后端更新失败，请手动重启应用')
    }
  }, [])
  const programTask = useTask({
    enabled,
    storageKey: PROGRAM_UPDATE_TASK_STORAGE_KEY,
    taskType: 'program-update',
    discoverTaskType: Boolean(programTaskRequestId),
    taskRequestId: programTaskRequestId,
    invalidateKeys: [['version']],
    onTerminal: handleProgramTaskTerminal,
  })

  // 查询后端版本信息
  const { data: versionData, refetch } = useQuery({
    queryKey: ['version'],
    enabled,
    queryFn: async () => {
      try {
        const localResponse = await versionAPI.getVersion('local')
        const local = requireVersionPayload(localResponse, '本地')
        const result = {
          backendVersion: local.version,
          remoteVersion: 'unknown',
          buildDate: local.buildDate,
          hasBackendUpdate: false,
          localError: '',
          remoteError: '',
        }
        try {
          const remoteResponse = await versionAPI.getVersion('remote')
          const remote = requireVersionPayload(remoteResponse, '远端')
          result.remoteVersion = remote.version
          result.hasBackendUpdate = shouldUpdate(local.version, remote.version)
        } catch (error) {
          result.remoteError = normalizeHttpError(error).message
        }
        return result
      } catch (error) {
        const message = normalizeHttpError(error).message
        return {
          backendVersion: 'unknown',
          remoteVersion: 'unknown',
          buildDate: '',
          hasBackendUpdate: false,
          localError: message,
          remoteError: '',
        }
      }
    },
    refetchInterval: 60000, // 每分钟自动刷新
    refetchOnWindowFocus: false,
    staleTime: 30000 // 30秒内不重新请求
  })

  // 更新后端
  const updateBackend = useCallback(async () => {
    const state = programUpdateState({ task: programTask.task, taskId: programTask.taskId, isSubmitting: isSubmittingUpdate, submitError: programTask.submitError, discoveryPending: Boolean(programTaskRequestId) })
    if (state === 'submitting' || state === 'active' || state === 'uncertain') return null
    const attemptRequestId = newProgramUpdateRequestId()
    setIsSubmittingUpdate(true)
    writePendingAcceptance(attemptRequestId)
    try {
      const accepted = await programTask.submit(() => versionAPI.updateProgram(attemptRequestId))
      writePendingAcceptance(false)
      setProgramTaskRequestId('')
      setShowUpdatePrompt(true)
      return accepted
    } catch (error) {
      // A failed POST can have an unknown outcome. Keep tracking its request ID
      // without replaying it; only a definite rejection gets the old error
      // prompt. The legacy UI has no task-status panel.
      const normalized = normalizeHttpError(error)
      const definitelyRejected = Number.isFinite(normalized.status) && normalized.status >= 400 && normalized.status < 500 && normalized.status !== 408 && normalized.status !== 429
      if (definitelyRejected) {
        writePendingAcceptance(false)
        setProgramTaskRequestId('')
        globalThis.alert?.('后端更新失败，请手动重启应用')
      } else {
        setProgramTaskRequestId(attemptRequestId)
      }
      return null
    } finally {
      setIsSubmittingUpdate(false)
    }
  }, [isSubmittingUpdate, programTask.submit, programTask.submitError, programTask.task, programTask.taskId, programTaskRequestId])

  // 手动检查更新
  const checkForUpdates = useCallback(async () => {
    await refetch()
  }, [refetch])

  const updateState = programUpdateState({ task: programTask.task, taskId: programTask.taskId, isSubmitting: isSubmittingUpdate, submitError: programTask.submitError, discoveryPending: Boolean(programTaskRequestId) })

  return {
    // 状态
    showUpdatePrompt,
    
    // 版本数据
    backendVersion: versionData?.backendVersion,
    remoteVersion: versionData?.remoteVersion,
    buildDate: versionData?.buildDate,
    hasBackendUpdate: versionData?.hasBackendUpdate,
    isUpdating: updateState === 'submitting' || updateState === 'active' || updateState === 'uncertain',
    
    // 方法
    setShowUpdatePrompt,
    updateBackend,
    checkForUpdates
  }
}
