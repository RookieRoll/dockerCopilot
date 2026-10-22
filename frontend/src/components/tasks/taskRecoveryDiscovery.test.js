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

test('discovery failure is surfaced and retries with capped exponential backoff', async () => {
  const scheduled = []
  const errors = []
  const applied = []
  const attempts = [
    Promise.reject(new Error('offline')),
    Promise.reject(new Error('still offline')),
    Promise.resolve([{ taskID: 'recovered' }]),
  ]
  const cleanup = startTaskRecoveryDiscovery(
    () => attempts.shift(),
    () => [{ id: 'new-id', name: 'web' }],
    (records, containers) => applied.push({ records, containers }),
    {
      initialDelayMs: 5,
      maxDelayMs: 8,
      onError: (error) => errors.push(error?.message || null),
      schedule: (callback, delay) => {
        const entry = { callback, delay, cancelled: false }
        scheduled.push(entry)
        return entry
      },
      cancelSchedule: (entry) => { entry.cancelled = true },
    },
  )
  await Promise.resolve()
  await Promise.resolve()
  assert.deepEqual(errors, ['offline'])
  assert.equal(scheduled[0].delay, 5)

  scheduled[0].callback()
  await Promise.resolve()
  await Promise.resolve()
  assert.deepEqual(errors, ['offline', 'still offline'])
  assert.equal(scheduled[1].delay, 8, 'the capped delay should not grow further')

  scheduled[1].callback()
  await Promise.resolve()
  await Promise.resolve()
  assert.deepEqual(errors, ['offline', 'still offline', null])
  assert.equal(applied[0].records[0].taskID, 'recovered')
  assert.equal(applied[0].containers[0].id, 'new-id')
  cleanup()
})

test('cleanup aborts an in-flight discovery and prevents later retries', async () => {
  const requests = []
  const errors = []
  const cleanup = startTaskRecoveryDiscovery(({ signal }) => new Promise((_, reject) => {
    requests.push(signal)
    signal.addEventListener('abort', () => reject(new Error('aborted')))
  }), () => [], () => {}, { onError: (error) => errors.push(error) })
  await Promise.resolve()
  cleanup()
  await Promise.resolve()
  assert.equal(requests[0].aborted, true)
  assert.equal(errors.length, 0, 'an intentional effect cleanup is not a user-facing discovery failure')
})
