import test from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import vm from 'node:vm'

const workerSource = await readFile(new URL('./sw.js', import.meta.url), 'utf8')

function createWorker({ scope = 'https://app.example/docker/', fetchImpl = async () => new Response('network') } = {}) {
  const handlers = new Map()
  const cacheData = new Map()
  const cacheCalls = { match: 0, put: 0 }
  const caches = {
    async open(name) {
      if (!cacheData.has(name)) cacheData.set(name, new Map())
      const cache = cacheData.get(name)
      return {
        async addAll(urls) {
          for (const url of urls) cache.set(url, new Response('cached asset'))
        },
        async put(request, response) {
          cacheCalls.put += 1
          cache.set(typeof request === 'string' ? request : request.url, response)
        },
      }
    },
    async match(request) {
      cacheCalls.match += 1
      const key = typeof request === 'string' ? request : request.url
      for (const cache of cacheData.values()) {
        if (cache.has(key)) return cache.get(key).clone()
      }
      return undefined
    },
    async keys() { return [...cacheData.keys()] },
    async delete(name) { return cacheData.delete(name) },
  }
  const self = {
    registration: { scope },
    clients: { claim: async () => {} },
    addEventListener(type, handler) { handlers.set(type, handler) },
    skipWaiting() {},
  }
  vm.runInNewContext(workerSource, { self, caches, fetch: fetchImpl, URL, Response, Promise })

  return {
    cacheCalls,
    cacheData,
    async dispatch(request) {
      let responsePromise
      handlers.get('fetch')({
        request,
        respondWith(promise) { responsePromise = promise },
      })
      return responsePromise ? responsePromise : fetchImpl(request)
    },
    async activate() {
      let waitPromise
      handlers.get('activate')({ waitUntil(promise) { waitPromise = promise } })
      await waitPromise
    },
  }
}

test('API GETs bypass cache for base-path deployments and never turn network failure into HTML', async () => {
  let calls = 0
  const worker = createWorker({ fetchImpl: async () => new Response(`api-${++calls}`) })
  const first = await worker.dispatch(new Request('https://app.example/api/containers'))
  const second = await worker.dispatch(new Request('https://app.example/api/containers'))
  const basePathApi = await worker.dispatch(new Request('https://app.example/docker/api/progress/task-1'))
  assert.equal(await first.text(), 'api-1')
  assert.equal(await second.text(), 'api-2')
  assert.equal(await basePathApi.text(), 'api-3')
  assert.equal(worker.cacheCalls.match, 0)
  assert.equal(worker.cacheCalls.put, 0)

  const failing = createWorker({ fetchImpl: async () => { throw new Error('network down') } })
  await assert.rejects(
    failing.dispatch(new Request('https://app.example/docker/api/progress/task-1')),
    /network down/,
  )
})

test('Authorization GETs bypass cache and preserve network errors', async () => {
  let calls = 0
  const worker = createWorker({ fetchImpl: async () => new Response(`private-${++calls}`) })
  const request = new Request('https://app.example/docker/private.json', {
    headers: { Authorization: 'Bearer secret' },
  })
  assert.equal(await (await worker.dispatch(request)).text(), 'private-1')
  assert.equal(await (await worker.dispatch(request)).text(), 'private-2')
  assert.equal(worker.cacheCalls.match, 0)
  assert.equal(worker.cacheCalls.put, 0)

  const failing = createWorker({ fetchImpl: async () => { throw new Error('private network down') } })
  await assert.rejects(
    failing.dispatch(new Request('https://app.example/docker/private.json', {
      headers: { Authorization: 'Bearer secret' },
    })),
    /private network down/,
  )
})

test('activation only removes prior Docker Copilot caches', async () => {
  const worker = createWorker()
  worker.cacheData.set('docker-copilot-v2', new Map())
  worker.cacheData.set('another-app-cache', new Map())
  worker.cacheData.set('docker-copilot-v3', new Map())
  await worker.activate()
  assert.deepEqual([...worker.cacheData.keys()].sort(), ['another-app-cache', 'docker-copilot-v3'])
})
