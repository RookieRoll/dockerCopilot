import test from 'node:test'
import assert from 'node:assert/strict'
import { formatContainerPort, getConfiguredPortUrl, getContainerPortTitle, getContainerPortUrl, isHostNetworkMode, isPublishedTcpPort, normalizeConfiguredPorts, normalizeContainerPorts, stopContainerPortEvent } from './containerPorts.js'

test('normalizes missing port arrays', () => {
  assert.deepEqual(normalizeContainerPorts(undefined), [])
  assert.deepEqual(normalizeContainerPorts(null), [])
  const ports = [{ hostPort: 8080, containerPort: 80, protocol: 'tcp' }]
  assert.deepEqual(normalizeContainerPorts(ports), ports)
})

test('deduplicates the same logical mapping regardless of host IP', () => {
  const ports = normalizeContainerPorts([
    { hostIp: '0.0.0.0', hostPort: 12712, containerPort: 12712, protocol: 'tcp' },
    { hostIp: '::', hostPort: 12712, containerPort: 12712, protocol: 'TCP' },
    { hostIp: '0.0.0.0', hostPort: 18080, containerPort: 8080, protocol: 'tcp' },
  ])
  assert.deepEqual(ports, [
    { hostIp: '0.0.0.0', hostPort: 12712, containerPort: 12712, protocol: 'tcp' },
    { hostIp: '0.0.0.0', hostPort: 18080, containerPort: 8080, protocol: 'tcp' },
  ])
})


test('normalizes configured host network ports and builds TCP URLs', () => {
  const ports = normalizeConfiguredPorts([
    { port: 18080, protocol: 'TCP', label: ' API ' },
    { port: 18080, protocol: 'tcp', label: 'duplicate' },
    { port: 5353, protocol: 'udp' },
    { port: 0, protocol: 'tcp' },
  ])
  assert.deepEqual(ports, [
    { port: 5353, protocol: 'udp', label: '' },
    { port: 18080, protocol: 'tcp', label: 'API' },
  ])
  assert.strictEqual(getConfiguredPortUrl(ports[1], 'http://192.0.2.10:12712/manager'), 'http://192.0.2.10:18080/')
  assert.strictEqual(getConfiguredPortUrl(ports[0], 'http://192.0.2.10:12712/manager'), null)
})

test('formats published and unpublished ports', () => {
  assert.strictEqual(formatContainerPort({ hostPort: 8080, containerPort: 80, protocol: 'tcp' }), '8080:80/tcp')
  assert.strictEqual(formatContainerPort({ hostPort: 0, containerPort: 80, protocol: 'tcp' }), '80/tcp')
  assert.strictEqual(formatContainerPort({ hostPort: 5353, containerPort: 53, protocol: 'UDP' }), '5353:53/udp')
})

test('only published TCP ports are web links', () => {
  assert.strictEqual(isPublishedTcpPort({ hostPort: 8080, containerPort: 80, protocol: 'tcp' }), true)
  assert.strictEqual(isPublishedTcpPort({ hostPort: 8080, containerPort: 80, protocol: 'TCP' }), true)
  assert.strictEqual(isPublishedTcpPort({ hostPort: 0, containerPort: 80, protocol: 'tcp' }), false)
  assert.strictEqual(isPublishedTcpPort({ hostPort: 5353, containerPort: 53, protocol: 'udp' }), false)
  assert.strictEqual(isPublishedTcpPort({ hostPort: 70000, containerPort: 80, protocol: 'tcp' }), false)
  assert.strictEqual(isPublishedTcpPort({ hostPort: 8080, containerPort: 80, protocol: 'tcp' }, 'host'), false)
})

test('stops container card event propagation for port controls', () => {
  let stopCount = 0
  stopContainerPortEvent({
    stopPropagation() {
      stopCount += 1
    },
  })
  assert.strictEqual(stopCount, 1)
})
test('builds a new HTTP tab URL from the current management URL', () => {
  const port = { hostPort: 8080, containerPort: 80, protocol: 'tcp' }
  assert.strictEqual(getContainerPortUrl(port, 'http://192.0.2.10:12712/manager?tab=containers#ports'), 'http://192.0.2.10:8080/')
})

test('preserves HTTPS and handles IPv6 hosts', () => {
  const port = { hostPort: 8443, containerPort: 443, protocol: 'tcp' }
  assert.strictEqual(getContainerPortUrl(port, 'https://[2001:db8::10]:12712/manager'), 'https://[2001:db8::10]:8443/')
})

test('recognizes host network mode and does not build host network URLs', () => {
  assert.strictEqual(isHostNetworkMode('host'), true)
  assert.strictEqual(isHostNetworkMode('HOST'), true)
  assert.strictEqual(isHostNetworkMode('bridge'), false)
  assert.strictEqual(getContainerPortUrl({ hostPort: 8080, containerPort: 80, protocol: 'tcp' }, 'http://example.test/manager', 'host'), null)
})

test('does not build URLs for unpublished or UDP ports', () => {
  assert.strictEqual(getContainerPortUrl({ hostPort: 0, containerPort: 80, protocol: 'tcp' }, 'http://example.test/manager'), null)
  assert.strictEqual(getContainerPortUrl({ hostPort: 5353, containerPort: 53, protocol: 'udp' }, 'http://example.test/manager'), null)
  assert.strictEqual(getContainerPortTitle({ hostPort: 0, containerPort: 80, protocol: 'tcp' }), '该端口未发布到宿主机，不能通过浏览器打开')
  assert.strictEqual(getContainerPortTitle({ hostPort: 5353, containerPort: 53, protocol: 'udp' }), 'UDP 端口不能通过浏览器打开')
})
