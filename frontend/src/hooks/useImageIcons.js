import { useQuery } from '@tanstack/react-query'
import { imageAPI } from '../api/client.js'

export const IMAGE_ICONS_QUERY_KEY = ['customIcons']
export const IMAGE_ICONS_STORAGE_KEY = 'docker_copilot_image_logos'
export const IMAGE_ICON_MAX_BYTES = 10 * 1024 * 1024
export const IMAGE_ICON_SUPPORTED_FORMATS = ['png', 'jpg', 'jpeg', 'webp', 'gif']
let imageIconsRevision = 0

function isSuccessCode(code) {
  return code === 0 || code === 200
}

export function parseImageIconsResponse(response) {
  const envelope = response?.data
  if (!isSuccessCode(envelope?.code)) {
    throw new Error(envelope?.msg || '获取图标映射失败')
  }

  let icons = envelope.data
  // Some older handlers serialized the map as a JSON string (including '{}').
  if (typeof icons === 'string') {
    try {
      icons = JSON.parse(icons)
    } catch {
      throw new Error('图标映射响应格式无效')
    }
  }
  if (icons == null) return {}
  if (typeof icons !== 'object' || Array.isArray(icons)) {
    throw new Error('图标映射响应格式无效')
  }
  if (Object.values(icons).some((url) => typeof url !== 'string')) {
    throw new Error('图标映射值必须是字符串 URL')
  }
  return icons
}

export function readCachedImageIcons() {
  try {
    const cached = localStorage.getItem(IMAGE_ICONS_STORAGE_KEY)
    if (!cached) return undefined
    const parsed = JSON.parse(cached)
    if (parsed && typeof parsed === 'object' && !Array.isArray(parsed) &&
      Object.values(parsed).every((url) => typeof url === 'string')) return parsed
  } catch (error) {
    console.warn('读取本地图标缓存失败:', error)
  }
  return undefined
}

function persistImageIcons(icons) {
  try {
    localStorage.setItem(IMAGE_ICONS_STORAGE_KEY, JSON.stringify(icons))
  } catch (error) {
    console.warn('保存本地图标缓存失败:', error)
  }
}

export function useImageIcons() {
  return useQuery({
    queryKey: IMAGE_ICONS_QUERY_KEY,
    queryFn: ({ signal }) => fetchImageIcons({ signal }),
    initialData: readCachedImageIcons,
    refetchOnMount: true,
  })
}

export async function fetchImageIcons({ signal } = {}) {
  const revision = imageIconsRevision
  const icons = parseImageIconsResponse(await imageAPI.getIcons({ signal }))
  if (signal?.aborted || revision !== imageIconsRevision) {
    throw new DOMException('图标映射请求已取消或过期', 'AbortError')
  }
  persistImageIcons(icons)
  return icons
}

export async function uploadImageIcon({ file, imageName, containerName, queryClient }) {
  imageIconsRevision += 1
  await queryClient.cancelQueries({ queryKey: IMAGE_ICONS_QUERY_KEY, exact: true })
  const response = await imageAPI.uploadIcon(file, imageName, containerName)
  const envelope = response?.data
  if (!isSuccessCode(envelope?.code)) {
    throw new Error(envelope?.msg || '上传图标失败')
  }
  const filename = typeof envelope.data === 'string' ? envelope.data : ''
  if (!filename) {
    throw new Error('上传成功响应缺少图标文件名')
  }

  // Invalidate reads that began while the upload was in flight, too.
  imageIconsRevision += 1
  const url = `/src/config/image/${filename}`
  queryClient.setQueryData(IMAGE_ICONS_QUERY_KEY, (previous) => {
    const current = previous && typeof previous === 'object' && !Array.isArray(previous) ? previous : {}
    const updated = { ...current, [imageName]: url }
    persistImageIcons(updated)
    return updated
  })
  // Keep other mounted consumers synchronized; a refetch failure retains the
  // successful filename-based cache update and surfaces the query error.
  queryClient.invalidateQueries({ queryKey: IMAGE_ICONS_QUERY_KEY }).catch(() => {})
  return { filename, url }
}
