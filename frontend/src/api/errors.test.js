import test from 'node:test'
import assert from 'node:assert/strict'
import apiClient, { progressAPI, taskAPI } from './client.js'
import { ApiError, createBusinessApiError, normalizeHttpError } from './errors.js'

test('HTTP and business errors retain status, stable code, request ID, and cause', () => {
  const requestCause = new Error('socket reset')
  const http = normalizeHttpError(Object.assign(new Error('request failed'), {
    cause: requestCause,
    response: { status: 503, data: { code: 50001, errorCode: 'TASK_STORE_UNAVAILABLE', requestId: 'req-503', msg: 'storage unavailable' }, headers: {} },
  }))
  assert.ok(http instanceof ApiError)
  assert.equal(http.status, 503)
  assert.equal(http.errorCode, 'TASK_STORE_UNAVAILABLE')
  assert.equal(http.requestId, 'req-503')
  assert.equal(http.cause, requestCause)

  const business = createBusinessApiError({ status: 200, data: { code: 50001, requestId: 'req-business', msg: 'rejected' }, headers: {} })
  assert.equal(business.status, 200)
  assert.equal(business.errorCode, 50001)
  assert.equal(business.requestId, 'req-business')
})

test('task APIs preserve endpoint paths, encode task IDs, and reject business-code failures', async () => {
  const originalAdapter = apiClient.defaults.adapter
  const originalStorage = globalThis.localStorage
  const storage = new Map()
  globalThis.localStorage = { getItem: (key) => storage.get(key) ?? null, removeItem: (key) => storage.delete(key) }
  const requests = []
  apiClient.defaults.adapter = async (config) => {
    requests.push(config)
    return { data: { code: 200, data: config.url.includes('/api/tasks') ? { items: [] } : { taskID: 'task id', status: 'running' } }, status: 200, statusText: 'OK', headers: {}, config }
  }
  try {
    await progressAPI.getProgress('id/with space')
    await taskAPI.listTasks({ containerID: 'container-1', page: 2, limit: 50 })
    assert.equal(requests[0].url, '/api/progress/id%2Fwith%20space')
    assert.equal(requests[1].url, '/api/tasks')
    assert.deepEqual(requests[1].params, { containerID: 'container-1', page: 2, limit: 50 })

    apiClient.defaults.adapter = async (config) => ({ data: { code: 50001, errorCode: 'TASK_UNKNOWN', requestId: 'req-task', msg: 'unknown task' }, status: 200, statusText: 'OK', headers: {}, config })
    await assert.rejects(taskAPI.getTask('missing'), (error) => error instanceof ApiError && error.status === 200 && error.errorCode === 'TASK_UNKNOWN' && error.requestId === 'req-task')

    apiClient.defaults.adapter = async (config) => ({ data: { code: 0, data: { taskID: 'legacy-zero' } }, status: 200, statusText: 'OK', headers: {}, config })
    const oldCode = await taskAPI.getTask('legacy-zero')
    assert.equal(oldCode.data.code, 0)
  } finally {
    apiClient.defaults.adapter = originalAdapter
    globalThis.localStorage = originalStorage
  }
})
