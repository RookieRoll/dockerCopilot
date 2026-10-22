import test from 'node:test'
import assert from 'node:assert/strict'
import { isTaskPanelTerminal, taskPanelControls } from './taskPanelModel.js'

test('terminal panels can be dismissed while active task tracking remains visible', () => {
  assert.deepEqual(taskPanelControls({ status: 'queued' }), { canCollapse: true, canDismiss: false })
  assert.deepEqual(taskPanelControls({ status: 'running' }), { canCollapse: true, canDismiss: false })
  for (const status of ['succeeded', 'failed', 'interrupted']) {
    assert.equal(isTaskPanelTerminal({ status }), true)
    assert.deepEqual(taskPanelControls({ status }), { canCollapse: true, canDismiss: true })
  }
})

test('a definite submission rejection can be closed without hiding a tracking task', () => {
  assert.deepEqual(taskPanelControls(null, { status: 409 }), { canCollapse: true, canDismiss: true })
  assert.deepEqual(taskPanelControls({ status: 'running' }, { status: 503 }), { canCollapse: true, canDismiss: false })
})
