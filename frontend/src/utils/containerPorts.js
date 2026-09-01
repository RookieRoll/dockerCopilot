const MAX_PORT = 65535

function toPortNumber(value) {
  const port = Number(value)
  return Number.isInteger(port) && port >= 0 && port <= MAX_PORT ? port : 0
}

function normalizeProtocol(value) {
  return typeof value === 'string' && value.trim() ? value.trim().toLowerCase() : 'unknown'
}

export function normalizeContainerPorts(ports) {
  if (!Array.isArray(ports)) {
    return []
  }

  const unique = new Map()
  for (const port of ports) {
    const normalizedPort = {
      ...port,
      hostPort: toPortNumber(port?.hostPort),
      containerPort: toPortNumber(port?.containerPort),
      protocol: normalizeProtocol(port?.protocol),
    }
    const key = `${normalizedPort.hostPort}:${normalizedPort.containerPort}/${normalizedPort.protocol}`
    if (!unique.has(key)) {
      unique.set(key, normalizedPort)
    }
  }

  return [...unique.values()].sort((left, right) => {
    if (left.hostPort !== right.hostPort) return left.hostPort - right.hostPort
    if (left.containerPort !== right.containerPort) return left.containerPort - right.containerPort
    return left.protocol.localeCompare(right.protocol)
  })
}

export function isHostNetworkMode(networkMode) {
  return typeof networkMode === 'string' && networkMode.trim().toLowerCase() === 'host'
}

export function normalizeConfiguredPorts(ports) {
  if (!Array.isArray(ports)) {
    return []
  }

  const unique = new Map()
  for (const port of ports) {
    const normalizedPort = {
      port: toPortNumber(port?.port),
      protocol: normalizeProtocol(port?.protocol),
      label: typeof port?.label === 'string' ? port.label.trim() : '',
    }
    if (normalizedPort.port === 0 || !['tcp', 'udp'].includes(normalizedPort.protocol)) {
      continue
    }
    const key = `${normalizedPort.port}/${normalizedPort.protocol}`
    if (!unique.has(key)) {
      unique.set(key, normalizedPort)
    }
  }

  return [...unique.values()].sort((left, right) => {
    if (left.port !== right.port) return left.port - right.port
    return left.protocol.localeCompare(right.protocol)
  })
}

export function getConfiguredPortUrl(port, currentUrl) {
  return getContainerPortUrl({
    hostPort: port?.port,
    containerPort: port?.port,
    protocol: port?.protocol,
  }, currentUrl)
}

export function stopContainerPortEvent(event) {
  event.stopPropagation()
}

export function isPublishedTcpPort(port, networkMode) {
  return !isHostNetworkMode(networkMode) && toPortNumber(port?.hostPort) > 0 && normalizeProtocol(port?.protocol) === 'tcp'
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

export function getContainerPortTitle(port, networkMode) {
  if (isHostNetworkMode(networkMode)) {
    return `host \u7f51\u7edc\u6a21\u5f0f\uff1a${formatContainerPort(port)}\uff0cDocker \u672a\u63d0\u4f9b\u5bbf\u4e3b\u673a\u7aef\u53e3\u6620\u5c04\uff0c\u65e0\u6cd5\u81ea\u52a8\u6253\u5f00`
  }

  if (isPublishedTcpPort(port, networkMode)) {
    return `\u6253\u5f00 ${formatContainerPort(port)}`
  }

  if (normalizeProtocol(port?.protocol) === 'udp') {
    return 'UDP \u7aef\u53e3\u4e0d\u80fd\u901a\u8fc7\u6d4f\u89c8\u5668\u6253\u5f00'
  }

  return '\u8be5\u7aef\u53e3\u672a\u53d1\u5e03\u5230\u5bbf\u4e3b\u673a\uff0c\u4e0d\u80fd\u901a\u8fc7\u6d4f\u89c8\u5668\u6253\u5f00'
}

export function getContainerPortUrl(port, currentUrl, networkMode) {
  if (!isPublishedTcpPort(port, networkMode)) {
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
