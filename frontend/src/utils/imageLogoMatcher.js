function stripImageTag(imageName) {
  const value = String(imageName || '')
  const colon = value.lastIndexOf(':')
  return colon > value.lastIndexOf('/') ? value.slice(0, colon) : value
}

function lookupLogo(logos, key) {
  return Object.hasOwn(logos, key) && typeof logos[key] === 'string' ? logos[key] : null
}

function findLogo(imageName, logos, includeSubstring = false) {
  const exact = lookupLogo(logos, imageName)
  if (exact) return exact
  const baseName = stripImageTag(imageName)
  const base = lookupLogo(logos, baseName)
  if (base) return base
  const simpleName = baseName.split('/').pop()
  const simple = lookupLogo(logos, simpleName)
  if (simple) return simple

  for (const [key, url] of Object.entries(logos)) {
    if (!key || typeof url !== 'string') continue
    const logoBaseName = stripImageTag(key)
    if (baseName === logoBaseName || baseName.startsWith(`${logoBaseName}/`)) return url
  }
  if (includeSubstring) {
    const baseName = stripImageTag(imageName)
    const simpleName = baseName.split('/').pop()
    for (const [key, url] of Object.entries(logos)) {
      if (key && typeof url === 'string' && (baseName.includes(key) || simpleName.includes(key))) return url
    }
  }
  return null
}

// A user-uploaded logo intentionally overrides a built-in match for that image.
export function resolveImageLogo(imageName, customLogos = {}, builtInLogos = {}) {
  return findLogo(imageName, customLogos) || findLogo(imageName, builtInLogos, true)
}
