import test from 'node:test'
import assert from 'node:assert/strict'
import { taskBatchSummary, taskDiagnosticEntries, taskIdentifiers, taskStatusLabel, taskSubmissionOutcome } from './taskStatusModel.js'

test('task diagnostics preserve failure, recovery, warnings, and identifier fields', () => {
  const task = {
    taskID: 'task-uuid-1', requestID: 'req-123', status: 'failed',
    detailMsg: '新容器启动检查未通过', error: 'health check failed',
    recovery: { status: 'restored', detail: '旧容器已恢复运行' },
    warnings: ['新容器保留供排查'],
  }
  assert.deepEqual(taskDiagnosticEntries(task), [
    { label: '详细说明', value: '新容器启动检查未通过', tone: 'danger' },
    { label: '错误', value: 'health check failed', tone: 'danger' },
    { label: '恢复结果', value: 'restored · 旧容器已恢复运行', tone: 'normal' },
    { label: '警告', value: ['新容器保留供排查'], tone: 'danger' },
  ])
  assert.deepEqual(taskIdentifiers(task), { taskId: 'task-uuid-1', requestId: 'req-123' })
})

test('interrupted recovery state remains a manual-attention diagnostic', () => {
  assert.deepEqual(taskDiagnosticEntries({
    status: 'interrupted',
    detailMsg: '服务重启时操作仍未完成',
    recovery: { status: 'unknown', detail: '请核对容器状态' },
  }), [
    { label: '详细说明', value: '服务重启时操作仍未完成', tone: 'danger' },
    { label: '恢复结果', value: 'unknown · 请核对容器状态', tone: 'danger' },
  ])
})

test('status labels are explicit and task/request identifiers accept legacy casing', () => {
  assert.equal(taskStatusLabel('queued'), '排队中')
  assert.equal(taskStatusLabel('running'), '执行中')
  assert.equal(taskStatusLabel('succeeded'), '已完成')
  assert.equal(taskStatusLabel('failed'), '失败')
  assert.equal(taskStatusLabel('interrupted'), '已中断')
  assert.equal(taskStatusLabel('new-state'), '状态未知')
  assert.deepEqual(taskIdentifiers({ taskId: 'legacy-task', requestId: 'legacy-request' }), { taskId: 'legacy-task', requestId: 'legacy-request' })
})

test('server errors and timeouts keep submission outcome uncertain', () => {
  assert.equal(taskSubmissionOutcome(400).title, '提交未成功')
  assert.equal(taskSubmissionOutcome(409).title, '提交未成功')
  assert.equal(taskSubmissionOutcome(503).title, '提交结果待确认')
  assert.equal(taskSubmissionOutcome(408).title, '提交结果待确认')
  assert.equal(taskSubmissionOutcome(undefined).title, '提交结果待确认')
})

test('batch summary uses durable counts and falls back to visible item states', () => {
  assert.deepEqual(taskBatchSummary({
    items: [{ status: 'succeeded' }, { status: 'failed' }, { status: 'skipped' }, { status: 'queued' }],
  }), { success: 1, failed: 1, skipped: 1, total: 4 })
  assert.deepEqual(taskBatchSummary({
    successCount: 2, failedCount: 1, skippedCount: 0, items: [{ status: 'queued' }],
  }), { success: 2, failed: 1, skipped: 0, total: 1 })
  assert.equal(taskBatchSummary({ items: [] }), null)
})
