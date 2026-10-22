import test from 'node:test'
import assert from 'node:assert/strict'
import { compareVersions, parseVersion, shouldUpdate } from './versionUtils.js'

test('version parser accepts backend version grammar and ignores build metadata for ordering', () => {
  assert.ok(parseVersion('v2.2.2-rc.1+build.7'))
  assert.equal(compareVersions('2.2.2+build.1', 'v2.2.2+build.9'), 0)
  assert.equal(compareVersions('2.2.2-rc.1', '2.2.2'), -1)
  assert.equal(compareVersions('2.2.2-rc.10', '2.2.2-rc.2'), 1)
  assert.equal(compareVersions('2.2.2-1', '2.2.2-alpha'), -1)
  assert.equal(shouldUpdate('v2.2.2', '2.2.3+release'), true)
  assert.equal(shouldUpdate('2.2.2', '2.2.2+build'), false)
})

test('version parser rejects values outside the backend version grammar', () => {
  assert.equal(parseVersion(''), null)
  assert.equal(parseVersion('2.2'), null)
  assert.equal(parseVersion('2.2.2-'), null)
  assert.equal(parseVersion('2.2.2+build!'), null)
  assert.equal(compareVersions('2.2.2', 'not-a-version'), null)
  assert.equal(shouldUpdate('unknown', '3.0.0'), false)
})
