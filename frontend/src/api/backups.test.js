import test from 'node:test'
import assert from 'node:assert/strict'
import apiClient, { containerAPI } from './client.js'

test('backup actions use async routes and match the restore route contract', async () => {
  const originalAdapter = apiClient.defaults.adapter
  const originalStorage = globalThis.localStorage
  globalThis.localStorage = { getItem: () => null, setItem: () => {}, removeItem: () => {} }
  const requests = []
  apiClient.defaults.adapter = async (config) => {
    requests.push(config)
    return {
      data: { code: 200, msg: 'accepted', data: { taskID: 'task-1' } },
      status: 200,
      statusText: 'OK',
      headers: {},
      config,
    }
  }
  try {
    await containerAPI.backupContainerAsync()
    await containerAPI.backupToComposeAsync()
    await containerAPI.restoreContainer('backup file.json')
    assert.deepEqual(requests.map(({ method, url }) => [method, url]), [
      ['post', '/api/container/backup'],
      ['post', '/api/container/backup2compose'],
      ['post', '/api/container/backups/restore'],
    ])
    assert.deepEqual(JSON.parse(requests[2].data), { filename: 'backup file.json' })
  } finally {
    apiClient.defaults.adapter = originalAdapter
    globalThis.localStorage = originalStorage
  }
})
