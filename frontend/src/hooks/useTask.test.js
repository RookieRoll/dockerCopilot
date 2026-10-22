import test from 'node:test'
import assert from 'node:assert/strict'
import { QueryClient } from '@tanstack/react-query'
import { ApiError } from '../api/errors.js'
import { createTaskPollingController } from './taskPolling.js'
import { acceptedTaskId, createTaskScopeGuard, createTaskTerminalHandler, findActiveTaskByContainer, findRecentTaskByType, requestTaskAcceptance, taskSessionScopeKey, taskStorageKeyForScope } from './useTask.js'

function fakeTimers() {
  let nextId = 1
  const timers = new Map()
  return {
    timers,
    setTimer(fn, delay) {
      const id = nextId++
      timers.set(id, { fn, delay })
      return id
    },
    clearTimer(id) { timers.delete(id) },
    async fire() {
      const entry = timers.entries().next().value
      assert.ok(entry, 'expected a scheduled timer')
      timers.delete(entry[0])
      entry[1].fn()
      await flush()
    },
  }
}

async function flush() {
  await new Promise((resolve) => setImmediate(resolve))
}

test('task acceptance supports legacy envelopes and never replays a failed submit', async () => {
  assert.equal(acceptedTaskId({ data: { code: 200, data: { taskID: 'accepted-1' } } }), 'accepted-1')
  assert.equal(acceptedTaskId({ data: { code: 0, data: { taskId: 'accepted-2' } } }), 'accepted-2')
  let calls = 0
  const accepted = await requestTaskAcceptance(async () => {
    calls += 1
    return { data: { code: 200, data: { taskID: 'accepted-3' } } }
  })
  assert.equal(accepted.taskID, 'accepted-3')
  assert.equal(calls, 1)

  calls = 0
  await assert.rejects(requestTaskAcceptance(async () => {
    calls += 1
    throw new ApiError('submission outcome unknown', { status: 503, requestId: 'request-1' })
  }), (error) => error.status === 503 && error.requestId === 'request-1')
  assert.equal(calls, 1)
})

test('unknown program update acceptance requires the exact request identity even for recent terminal tasks', async () => {
  const requestId = '123e4567-e89b-42d3-a456-426614174000'
  const calls = []
  const found = await findRecentTaskByType({
    type: 'program-update',
    requestId,
    listTasks: async (params) => {
      calls.push(params)
      return { data: { code: 0, data: { items: [
        { taskID: 'old-task', type: 'program-update', status: 'succeeded', createdAt: '2026-10-22T10:00:09.000Z', requestID: 'another-request' },
        { taskID: 'new-task', type: 'program-update', status: 'interrupted', createdAt: '2026-10-22T10:00:10.000Z', requestID: requestId },
      ] } } }
    },
  })
  assert.equal(found.taskID, 'new-task')
  assert.deepEqual(calls, [{ type: 'program-update', page: 1, limit: 100 }])

  const noMatch = await findRecentTaskByType({
    type: 'program-update',
    requestId,
    listTasks: async () => ({ data: { code: 0, data: { items: [
      { taskID: 'old-task', type: 'program-update', status: 'succeeded', createdAt: '2026-10-22T10:00:10.000Z', requestID: 'another-request' },
    ] } } }),
  })
  assert.equal(noMatch, null)
  assert.equal(await findRecentTaskByType({ type: 'program-update', listTasks: () => { throw new Error('must not search without identity') } }), null)
})

test('polling runs beyond 60 attempts and treats failed/interrupted as task results', async () => {
  const clock = fakeTimers()
  let polls = 0
  const terminal = []
  let latest
  const controller = createTaskPollingController({
    getTask: async () => {
      polls += 1
      return { data: { code: 0, data: { taskID: 'task-long', status: polls < 75 ? 'running' : 'failed', detailMsg: 'operation failed', recovery: { status: 'unknown' }, warnings: ['cleanup warning'] } } }
    },
    onUpdate: (task) => { latest = task },
    onTerminal: (task) => terminal.push(task),
    intervalMs: 1,
    setTimer: clock.setTimer,
    clearTimer: clock.clearTimer,
  })
  controller.start('task-long')
  await flush()
  for (let i = 1; i < 75; i += 1) await clock.fire()
  assert.equal(polls, 75)
  assert.equal(latest.status, 'failed')
  assert.equal(latest.detailMsg, 'operation failed')
  assert.deepEqual(latest.recovery, { status: 'unknown' })
  assert.deepEqual(latest.warnings, ['cleanup warning'])
  assert.equal(terminal.length, 1)
  assert.equal(clock.timers.size, 0)
})

test('network failures back off within a bound and then continue polling', async () => {
  const clock = fakeTimers()
  const errors = []
  let calls = 0
  const controller = createTaskPollingController({
    getTask: async () => {
      calls += 1
      if (calls === 1) throw new Error('offline')
      return { data: { code: 200, data: { taskID: 'task-network', status: 'running' } } }
    },
    onConnectionError: (error) => errors.push(error),
    intervalMs: 10,
    maxRetryMs: 25,
    setTimer: clock.setTimer,
    clearTimer: clock.clearTimer,
  })
  controller.start('task-network')
  await flush()
  assert.equal(clock.timers.values().next().value.delay, 20)
  await clock.fire()
  assert.equal(calls, 2)
  assert.equal(errors[0].message, 'offline')
  assert.equal(errors.at(-1), null)
  assert.equal(clock.timers.values().next().value.delay, 10)
  controller.stop()
  assert.equal(clock.timers.size, 0)
})

test('401 becomes visible unauthorized state and stops polling', async () => {
  const clock = fakeTimers()
  let unauthorized
  let connectionError
  let signal
  const controller = createTaskPollingController({
    getTask: async (_id, config) => { signal = config.signal; throw new ApiError('login required', { status: 401, errorCode: 40101, requestId: 'req-auth' }) },
    onUnauthorized: (error) => { unauthorized = error },
    onConnectionError: (error) => { connectionError = error },
    setTimer: clock.setTimer,
    clearTimer: clock.clearTimer,
  })
  controller.start('task-auth')
  await flush()
  assert.equal(unauthorized.status, 401)
  assert.equal(unauthorized.requestId, 'req-auth')
  assert.equal(connectionError, undefined)
  assert.equal(signal.aborted, true)
  assert.equal(clock.timers.size, 0)
})

test('stopping polling aborts the in-flight request and ignores a late result', async () => {
  const clock = fakeTimers()
  let signal
  let resolveRequest
  const updates = []
  const controller = createTaskPollingController({
    getTask: async (_id, config) => {
      signal = config.signal
      return new Promise((resolve) => { resolveRequest = resolve })
    },
    onUpdate: (task) => updates.push(task),
    onTerminal: (task) => updates.push(task),
    setTimer: clock.setTimer,
    clearTimer: clock.clearTimer,
  })
  controller.start('task-abort')
  await flush()
  assert.ok(signal instanceof AbortSignal)
  controller.stop()
  assert.equal(signal.aborted, true)
  resolveRequest({ data: { code: 200, data: { taskID: 'task-abort', status: 'succeeded' } } })
  await flush()
  assert.deepEqual(updates, [])
  assert.equal(clock.timers.size, 0)
})

test('scope change isolates storage keys and late task responses cannot update the new scope', async () => {
  const keyA = taskStorageKeyForScope({ containerId: 'container-A' })
  const keyB = taskStorageKeyForScope({ containerId: 'container-B' })
  assert.notEqual(keyA, keyB)
  assert.notEqual(taskSessionScopeKey({ containerId: 'container-A', taskId: 'task-A' }), taskSessionScopeKey({ containerId: 'container-A', taskId: 'task-B' }))
  assert.equal(taskStorageKeyForScope({ storageKey: 'custom', containerName: 'web' }), 'custom:container-name%3Aweb')

  const clock = fakeTimers()
  const scope = createTaskScopeGuard(keyA)
  let resolveRequest
  const updates = []
  const controller = createTaskPollingController({
    getTask: async () => new Promise((resolve) => { resolveRequest = resolve }),
    isCurrentScope: () => scope.isCurrent(keyA),
    onUpdate: (task) => updates.push(task),
    onTerminal: (task) => updates.push(task),
    setTimer: clock.setTimer,
    clearTimer: clock.clearTimer,
  })
  controller.start('task-A')
  await flush()
  scope.current = keyB
  resolveRequest({ data: { code: 200, data: { taskID: 'task-A', status: 'running' } } })
  await flush()
  controller.stop()
  assert.deepEqual(updates, [])
  assert.equal(clock.timers.size, 0)
})

test('container recovery asks separately for queued/running and picks the newest active task', async () => {
  const calls = []
  const signal = new AbortController().signal
  const task = await findActiveTaskByContainer({
    containerId: 'container-1',
    signal,
    listTasks: async (params, config) => {
      calls.push({ params, config })
      const items = params.status === 'queued'
        ? [{ taskID: 'queued-new', status: 'queued', createdAt: '2026-10-22T10:00:00Z' }]
        : [{ taskID: 'running-old', status: 'running', createdAt: '2026-10-22T09:00:00Z' }]
      return { data: { code: 200, data: { items } } }
    },
  })
  assert.deepEqual(calls.map((call) => call.params.status).sort(), ['queued', 'running'])
  assert.ok(calls.every((call) => call.params.containerID === 'container-1' && call.config.signal === signal))
  assert.equal(task.taskID, 'queued-new')
})

test('container recovery rejects malformed list records instead of treating them as no active task', async () => {
  await assert.rejects(findActiveTaskByContainer({
    containerId: 'container-1',
    listTasks: async (params) => ({ data: { code: 200, data: { items: params.status === 'queued' ? [{ taskID: 'invalid', status: 'completed' }] : [] } } }),
  }), (error) => error.errorCode === 'INVALID_TASK_RESPONSE')
})

test('malformed or unknown task statuses are visible as query errors, not empty success', async () => {
  for (const payload of [null, { taskID: 'bad-status', status: 'completed' }]) {
    const clock = fakeTimers()
    const errors = []
    const updates = []
    const controller = createTaskPollingController({
      getTask: async () => ({ data: { code: 200, data: payload } }),
      onConnectionError: (error) => errors.push(error),
      onUpdate: (task) => updates.push(task),
      intervalMs: 1,
      setTimer: clock.setTimer,
      clearTimer: clock.clearTimer,
    })
    controller.start('bad-task')
    await flush()
    assert.equal(errors[0].errorCode, 'INVALID_TASK_RESPONSE')
    assert.deepEqual(updates, [])
    assert.equal(clock.timers.size, 1)
    controller.stop()
  }
})

test('refresh recovery locates active task by container without an initial task ID', async () => {
  const clock = fakeTimers()
  const lookedUp = []
  const observed = []
  let poll = 0
  const controller = createTaskPollingController({
    findTask: async () => {
      lookedUp.push(true)
      poll += 1
      return poll === 1 ? null : { taskID: 'recovered-task', status: 'running' }
    },
    getTask: async (id) => ({ data: { code: 200, data: { taskID: id, status: 'succeeded' } } }),
    onUpdate: (task) => observed.push(task),
    onTerminal: (task) => observed.push(task),
    intervalMs: 1,
    setTimer: clock.setTimer,
    clearTimer: clock.clearTimer,
  })
  controller.start()
  await flush()
  assert.equal(lookedUp.length, 1)
  await clock.fire()
  assert.equal(lookedUp.length, 2)
  assert.equal(controller.getTaskId(), 'recovered-task')
  await clock.fire()
  assert.deepEqual(observed.map((item) => item.status), ['running', 'succeeded', 'succeeded'])
  assert.equal(clock.timers.size, 0)
})

test('terminal handling invalidates related query caches once and clears persisted task ID', async () => {
  const values = new Map([['current-task', 'task-cache']])
  globalThis.localStorage = {
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, value),
    removeItem: (key) => values.delete(key),
  }
  const queryClient = new QueryClient()
  queryClient.setQueryData(['containers'], ['before'])
  const originalInvalidate = queryClient.invalidateQueries.bind(queryClient)
  let invalidations = 0
  queryClient.invalidateQueries = (...args) => { invalidations += 1; return originalInvalidate(...args) }
  const callback = []
  const terminalHandler = createTaskTerminalHandler({
    queryClient,
    queryKeysRef: { current: [['containers'], ['task-list']] },
    handledRef: { current: new Set() },
    storageKey: 'current-task',
    onTerminal: (task) => callback.push(task),
  })
  const task = { taskID: 'task-cache', status: 'succeeded' }
  terminalHandler(task)
  terminalHandler(task)
  await flush()
  assert.equal(invalidations, 2)
  assert.deepEqual(callback, [task])
  assert.equal(values.has('current-task'), false)
  assert.equal(queryClient.getQueryState(['containers']).isInvalidated, true)
  queryClient.clear()
})

test('backup discovery without a request identity finds only active tasks', async () => {
  const task = await findRecentTaskByType({
    type: 'backup-export',
    listTasks: async () => ({ data: { code: 200, data: { items: [
      { taskID: 'completed', type: 'backup-export', status: 'succeeded', createdAt: '2026-10-22T12:00:02Z' },
      { taskID: 'active', type: 'backup-export', status: 'running', createdAt: '2026-10-22T12:00:01Z' },
    ] } } }),
  })
  assert.equal(task.taskID, 'active')
})