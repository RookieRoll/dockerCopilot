const VERSION_PATTERN = /^v?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$/

export function parseVersion(version) {
  if (typeof version !== 'string') return null
  const match = VERSION_PATTERN.exec(version)
  if (!match) return null
  return {
    major: BigInt(match[1]),
    minor: BigInt(match[2]),
    patch: BigInt(match[3]),
    prerelease: match[4] ? match[4].split('.') : [],
  }
}

export function compareVersions(leftVersion, rightVersion) {
  const left = parseVersion(leftVersion)
  const right = parseVersion(rightVersion)
  if (!left || !right) return null
  for (const component of ['major', 'minor', 'patch']) {
    if (left[component] > right[component]) return 1
    if (left[component] < right[component]) return -1
  }
  if (!left.prerelease.length || !right.prerelease.length) {
    if (!left.prerelease.length && right.prerelease.length) return 1
    if (left.prerelease.length && !right.prerelease.length) return -1
    return 0
  }
  const length = Math.max(left.prerelease.length, right.prerelease.length)
  for (let index = 0; index < length; index += 1) {
    const a = left.prerelease[index]
    const b = right.prerelease[index]
    if (a === undefined) return -1
    if (b === undefined) return 1
    if (a === b) continue
    const aNumeric = /^\d+$/.test(a)
    const bNumeric = /^\d+$/.test(b)
    if (aNumeric && bNumeric) {
      if (BigInt(a) === BigInt(b)) continue
      return BigInt(a) > BigInt(b) ? 1 : -1
    }
    if (aNumeric !== bNumeric) return aNumeric ? -1 : 1
    return a > b ? 1 : -1
  }
  return 0
}

export function shouldUpdate(currentVersion, latestVersion) {
  const comparison = compareVersions(latestVersion, currentVersion)
  return comparison !== null && comparison > 0
}
