import test from 'node:test'
import assert from 'node:assert/strict'
import apiClient, { containerAPI } from './client.js'

test('batch container API sends one task request and preserves optional update cleanup', async () => {
  const originalAdapter = apiClient.defaults.adapter
  const originalStorage = globalThis.localStorage
  globalThis.localStorage = { getItem: () => null, setItem: () => {}, removeItem: () => {} }
  const requests = []
  apiClient.defaults.adapter = async (config) => {
    requests.push(config)
    return { data: { code: 200, data: { taskID: `task-${requests.length}` } }, status: 200, statusText: 'OK', headers: {}, config }
  }
  try {
    await containerAPI.batchContainers('stop', ['id-1', 'id-2'])
    await containerAPI.batchContainers('update', ['id-3'], false)
    await containerAPI.batchContainers('update', ['id-4'], true)
    assert.deepEqual(requests.map(({ method, url }) => [method, url]), [
      ['post', '/api/containers/batch'],
      ['post', '/api/containers/batch'],
      ['post', '/api/containers/batch'],
    ])
    assert.deepEqual(requests.map(({ data }) => JSON.parse(data)), [
      { action: 'stop', containerIDs: ['id-1', 'id-2'] },
      { action: 'update', containerIDs: ['id-3'], deleteOldContainer: false },
      { action: 'update', containerIDs: ['id-4'], deleteOldContainer: true },
    ])
  } finally {
    apiClient.defaults.adapter = originalAdapter
    globalThis.localStorage = originalStorage
  }
})
