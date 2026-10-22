import test from 'node:test'
import assert from 'node:assert/strict'
import { QueryClient } from '@tanstack/react-query'
import { imageAPI } from '../api/client.js'
import {
  fetchImageIcons,
  IMAGE_ICONS_QUERY_KEY,
  IMAGE_ICONS_STORAGE_KEY,
  parseImageIconsResponse,
  uploadImageIcon,
} from './useImageIcons.js'
import { resolveImageLogo } from '../utils/imageLogoMatcher.js'

function setLocalStorage(initial = {}) {
  const values = new Map(Object.entries(initial))
  globalThis.localStorage = {
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, value),
    removeItem: (key) => values.delete(key),
  }
  return values
}

test('parses legacy icon mapping payloads and accepts both successful API codes', () => {
  assert.deepEqual(parseImageIconsResponse({ data: { code: 200, data: { nginx: '/old.png' } } }), { nginx: '/old.png' })
  assert.deepEqual(parseImageIconsResponse({ data: { code: 0, data: '{"redis":"/redis.png"}' } }), { redis: '/redis.png' })
  assert.deepEqual(parseImageIconsResponse({ data: { code: 200, data: '{}' } }), {})
  assert.throws(() => parseImageIconsResponse({ data: { code: 200, data: { nginx: { path: '/bad.png' } } } }), /字符串 URL/)
})

test('failed icon fetch is visible to React Query and leaves persisted success cache unchanged', async () => {
  const storage = setLocalStorage({ [IMAGE_ICONS_STORAGE_KEY]: '{"nginx":"/cached.png"}' })
  const previous = imageAPI.getIcons
  imageAPI.getIcons = async () => ({ data: { code: 50001, msg: 'mapping unavailable', data: null } })
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  try {
    await assert.rejects(queryClient.fetchQuery({ queryKey: IMAGE_ICONS_QUERY_KEY, queryFn: fetchImageIcons }), /mapping unavailable/)
    assert.equal(queryClient.getQueryState(IMAGE_ICONS_QUERY_KEY).status, 'error')
    assert.equal(storage.get(IMAGE_ICONS_STORAGE_KEY), '{"nginx":"/cached.png"}')
  } finally {
    imageAPI.getIcons = previous
    queryClient.clear()
  }
})

test('successful upload uses legacy filename response and updates the shared mapping cache', async () => {
  const storage = setLocalStorage()
  const previousUpload = imageAPI.uploadIcon
  imageAPI.uploadIcon = async (file, imageName, containerName) => {
    assert.equal(file.name, 'custom.png')
    assert.equal(imageName, 'registry/app:latest')
    assert.equal(containerName, 'app')
    return { data: { code: 200, msg: 'success', data: 'icon-123.png' } }
  }
  const queryClient = new QueryClient()
  queryClient.setQueryData(IMAGE_ICONS_QUERY_KEY, { redis: '/redis.png' })
  try {
    const result = await uploadImageIcon({
      file: { name: 'custom.png' },
      imageName: 'registry/app:latest',
      containerName: 'app',
      queryClient,
    })
    const expected = '/src/config/image/icon-123.png'
    assert.deepEqual(result, { filename: 'icon-123.png', url: expected })
    assert.deepEqual(queryClient.getQueryData(IMAGE_ICONS_QUERY_KEY), {
      redis: '/redis.png',
      'registry/app:latest': expected,
    })
    assert.deepEqual(JSON.parse(storage.get(IMAGE_ICONS_STORAGE_KEY)), queryClient.getQueryData(IMAGE_ICONS_QUERY_KEY))
  } finally {
    imageAPI.uploadIcon = previousUpload
    queryClient.clear()
  }
})

test('failed upload response does not alter shared or persisted mapping cache', async () => {
  const original = { nginx: '/old.png' }
  const storage = setLocalStorage({ [IMAGE_ICONS_STORAGE_KEY]: JSON.stringify(original) })
  const previousUpload = imageAPI.uploadIcon
  imageAPI.uploadIcon = async () => ({ data: { code: 50001, msg: 'rejected', data: null } })
  const queryClient = new QueryClient()
  queryClient.setQueryData(IMAGE_ICONS_QUERY_KEY, original)
  try {
    await assert.rejects(uploadImageIcon({ file: {}, imageName: 'nginx', queryClient }), /rejected/)
    assert.deepEqual(queryClient.getQueryData(IMAGE_ICONS_QUERY_KEY), original)
    assert.equal(storage.get(IMAGE_ICONS_STORAGE_KEY), JSON.stringify(original))
  } finally {
    imageAPI.uploadIcon = previousUpload
    queryClient.clear()
  }
})

test('late pre-upload mapping response cannot overwrite the uploaded logo cache', async () => {
  const storage = setLocalStorage()
  let finishFetch
  const previousGet = imageAPI.getIcons
  const previousUpload = imageAPI.uploadIcon
  imageAPI.getIcons = () => new Promise((resolve) => { finishFetch = resolve })
  imageAPI.uploadIcon = async () => ({ data: { code: 0, msg: 'ok', data: 'new.png' } })
  const queryClient = new QueryClient()
  const pendingFetch = queryClient.fetchQuery({
    queryKey: IMAGE_ICONS_QUERY_KEY,
    queryFn: ({ signal }) => fetchImageIcons({ signal }),
    staleTime: Infinity,
  }).catch(() => undefined)
  try {
    const upload = await uploadImageIcon({ file: {}, imageName: 'nginx', queryClient })
    finishFetch({ data: { code: 200, data: { nginx: '/old.png' } } })
    await pendingFetch
    assert.deepEqual(queryClient.getQueryData(IMAGE_ICONS_QUERY_KEY), { nginx: upload.url })
    assert.deepEqual(JSON.parse(storage.get(IMAGE_ICONS_STORAGE_KEY)), { nginx: upload.url })
  } finally {
    imageAPI.getIcons = previousGet
    imageAPI.uploadIcon = previousUpload
    queryClient.clear()
  }
})

test('mapping request started during upload cannot overwrite the committed filename mapping', async () => {
  const storage = setLocalStorage()
  const previousGet = imageAPI.getIcons
  const previousUpload = imageAPI.uploadIcon
  let finishFetch
  let finishUpload
  let announceFetch
  let announceUpload
  const fetchStarted = new Promise(resolve => { announceFetch = resolve })
  const uploadStarted = new Promise(resolve => { announceUpload = resolve })
  imageAPI.getIcons = () => new Promise(resolve => { finishFetch = resolve; announceFetch() })
  imageAPI.uploadIcon = () => new Promise(resolve => { finishUpload = resolve; announceUpload() })
  const queryClient = new QueryClient()
  try {
    const uploadPromise = uploadImageIcon({ file: {}, imageName: 'nginx', queryClient })
    await uploadStarted
    const staleFetch = queryClient.fetchQuery({
      queryKey: IMAGE_ICONS_QUERY_KEY,
      queryFn: ({ signal }) => fetchImageIcons({ signal }),
      staleTime: Infinity,
    }).catch(() => undefined)
    await fetchStarted
    finishUpload({ data: { code: 200, msg: 'ok', data: 'committed.png' } })
    const uploaded = await uploadPromise
    finishFetch({ data: { code: 200, data: { nginx: '/stale.png' } } })
    await staleFetch
    assert.deepEqual(queryClient.getQueryData(IMAGE_ICONS_QUERY_KEY), { nginx: uploaded.url })
    assert.deepEqual(JSON.parse(storage.get(IMAGE_ICONS_STORAGE_KEY)), { nginx: uploaded.url })
  } finally {
    imageAPI.getIcons = previousGet
    imageAPI.uploadIcon = previousUpload
    queryClient.clear()
  }
})

test('a custom upload overrides a built-in match, including registry ports and tags', () => {
  assert.equal(resolveImageLogo('library/nginx:latest', { 'library/nginx': '/custom.png' }, { nginx: '/builtin.png' }), '/custom.png')
  assert.equal(resolveImageLogo('registry.example:5000/team/app:v2', { 'registry.example:5000/team/app': '/custom-app.png' }), '/custom-app.png')
  assert.equal(resolveImageLogo('library/nginx:latest', {}, { nginx: '/builtin.png' }), '/builtin.png')
  assert.equal(resolveImageLogo('my-nginx-proxy:latest', {}, { nginx: '/builtin.png' }), '/builtin.png')
  assert.equal(resolveImageLogo('constructor', {}, {}), null)
})
