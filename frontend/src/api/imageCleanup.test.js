import test from 'node:test'
import assert from 'node:assert/strict'
import apiClient, { imageAPI } from './client.js'

test('image cleanup submits one server task request for the selected policy', async () => {
  const originalAdapter = apiClient.defaults.adapter
  const originalStorage = globalThis.localStorage
  globalThis.localStorage = { getItem: () => null, setItem: () => {}, removeItem: () => {} }
  const requests = []
  apiClient.defaults.adapter = async (config) => {
    requests.push(config)
    return { data: { code: 200, msg: 'accepted', data: { taskID: 'cleanup-task' } }, status: 200, statusText: 'OK', headers: {}, config }
  }
  try {
    const response = await imageAPI.cleanupImages('unused')
    assert.deepEqual([requests.length, requests[0].method, requests[0].url, JSON.parse(requests[0].data)], [
      1, 'post', '/api/images/cleanup', { policy: 'unused' },
    ])
    assert.equal(response.data.data.taskID, 'cleanup-task')
  } finally {
    apiClient.defaults.adapter = originalAdapter
    globalThis.localStorage = originalStorage
  }
})
