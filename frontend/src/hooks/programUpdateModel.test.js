import test from 'node:test'
import assert from 'node:assert/strict'
import { programUpdateState, newProgramUpdateRequestId } from './programUpdateModel.js'

test('program requests have canonical UUID identity on plain HTTP without randomUUID', () => {
  const id = newProgramUpdateRequestId({ getRandomValues: (bytes) => bytes.fill(255) })
  assert.equal(id, 'ffffffff-ffff-4fff-bfff-ffffffffffff')
})

test('program update only completes from a terminal success record', () => {
  assert.equal(programUpdateState(), 'idle')
  assert.equal(programUpdateState({ isSubmitting: true }), 'submitting')
  assert.equal(programUpdateState({ task: { status: 'queued' }, taskId: 'task-1' }), 'active')
  assert.equal(programUpdateState({ task: { status: 'running' }, taskId: 'task-1' }), 'active')
  assert.equal(programUpdateState({ task: { status: 'succeeded' }, taskId: 'task-1' }), 'succeeded')
  assert.equal(programUpdateState({ task: { status: 'failed' }, taskId: 'task-1' }), 'failed')
  assert.equal(programUpdateState({ task: { status: 'interrupted' }, taskId: 'task-1' }), 'interrupted')
  assert.equal(programUpdateState({ submitError: { status: 400 } }), 'rejected')
})

test('restart disconnect and missing task response stay uncertain and block replay', () => {
  assert.equal(programUpdateState({ taskId: 'task-restart' }), 'uncertain')
  assert.equal(programUpdateState({ taskId: 'task-restart', connectionError: new Error('offline') }), 'uncertain')
  assert.equal(programUpdateState({ taskId: 'task-restart', task: { status: 'unknown' } }), 'uncertain')
  assert.equal(programUpdateState({ submitError: { status: 503 } }), 'uncertain')
  assert.equal(programUpdateState({ submitError: { status: 408 } }), 'uncertain')
})
