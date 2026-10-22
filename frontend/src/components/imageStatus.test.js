import test from 'node:test'
import assert from 'node:assert/strict'
import { imageUpdatePresentation } from './imageStatus.js'

test('image check presentation distinguishes unknown, stale failure, update, and current', () => {
  assert.deepEqual(imageUpdatePresentation(undefined), {
    kind: 'unknown', label: '远端更新检查未知', detail: '',
  })
  assert.deepEqual(imageUpdatePresentation({ known: false, stale: true, error: 'TLS certificate rejected' }), {
    kind: 'unknown', label: '远端更新检查未知', detail: 'TLS certificate rejected',
  })
  assert.deepEqual(imageUpdatePresentation({ known: true, needUpdate: true, stale: true, error: 'timeout' }), {
    kind: 'stale', label: '更新检查已陈旧', detail: 'timeout',
  })
  assert.deepEqual(imageUpdatePresentation({ known: true, needUpdate: true, stale: false }), {
    kind: 'update', label: '远端发现新镜像', detail: '',
  })
  assert.deepEqual(imageUpdatePresentation({ known: true, needUpdate: false, stale: false }), {
    kind: 'current', label: '检查正常，未发现更新', detail: '',
  })
})
