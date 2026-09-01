const MAX_PORT = 65535

function toPortNumber(value) {
  const port = Number(value)
  return Number.isInteger(port) && port >= 0 && port <= MAX_PORT ? port : 0
}

function normalizeProtocol(value) {
  return typeof value === 'string' && value.trim() ? value.trim().toLowerCase() : 'unknown'
}

export function normalizeContainerPorts(ports) {
  return Array.isArray(ports) ? ports : []
}

export function stopContainerPortEvent(event) {
  event.stopPropagation()
}

export function isPublishedTcpPort(port) {
  return toPortNumber(port?.hostPort) > 0 && normalizeProtocol(port?.protocol) === 'tcp'
}

export function formatContainerPort(port) {
  const hostPort = toPortNumber(port?.hostPort)
  const containerPort = toPortNumber(port?.containerPort)
  const protocol = normalizeProtocol(port?.protocol)

  if (hostPort > 0) {
    return `${hostPort}:${containerPort}/${protocol}`
  }

  return `${containerPort}/${protocol}`
}

export function getContainerPortTitle(port) {
  if (isPublishedTcpPort(port)) {
    return `打开 ${formatContainerPort(port)}`
  }

  if (normalizeProtocol(port?.protocol) === 'udp') {
    return 'UDP 端口不能通过浏览器打开'
  }

  return '该端口未发布到宿主机，不能通过浏览器打开'
}

export function getContainerPortUrl(port, currentUrl) {
  if (!isPublishedTcpPort(port)) {
    return null
  }

  const sourceUrl = currentUrl || (typeof window !== 'undefined' ? window.location.href : '')
  if (!sourceUrl) {
    return null
  }

  try {
    const url = new URL(sourceUrl)
    url.port = String(toPortNumber(port.hostPort))
    url.pathname = '/'
    url.search = ''
    url.hash = ''
    return url.toString()
  } catch {
    return null
  }
}
