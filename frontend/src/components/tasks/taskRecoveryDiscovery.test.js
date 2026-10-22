import test from 'node:test'
import assert from 'node:assert/strict'
import { startTaskRecoveryDiscovery } from './taskRecoveryDiscovery.js'

function deferred() {
  let resolve
  const promise = new Promise((done) => { resolve = done })
  return { promise, resolve }
}

test('StrictMode-style cleanup and delayed discovery use the latest container list', async () => {
  const first = deferred()
  const second = deferred()
  let containers = [{ id: 'old-id', name: 'web' }]
  const applied = []
  const firstCleanup = startTaskRecoveryDiscovery(() => first.promise, () => containers, (records, latest) => applied.push({ records, latest }))
  await Promise.resolve()
  firstCleanup()

  const secondCleanup = startTaskRecoveryDiscovery(() => second.promise, () => containers, (records, latest) => applied.push({ records, latest }))
  await Promise.resolve()
  containers = [{ id: 'replacement-id', name: 'web' }]
  first.resolve([{ taskID: 'stale' }])
  await Promise.resolve()
  assert.equal(applied.length, 0, 'the cleaned-up StrictMode effect must ignore its late result')

  second.resolve([{ taskID: 'active' }])
  await second.promise
  await Promise.resolve()
  assert.equal(applied.length, 1)
  assert.equal(applied[0].records[0].taskID, 'active')
  assert.equal(applied[0].latest[0].id, 'replacement-id', 'the delayed response must read the latest container list')
  secondCleanup()
})
