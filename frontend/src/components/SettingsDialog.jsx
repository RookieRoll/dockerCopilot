import React, { useEffect, useRef, useState } from 'react'
import api from '../api/client.js'

export function SettingsDialog({ onClose }) {
  const dialog = useRef(null)
  const [settings, setSettings] = useState(null)
  const [address, setAddress] = useState('')
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    const controller = new AbortController()
    dialog.current.showModal()
    api.get('/api/settings/proxy', { signal: controller.signal })
      .then(({ data }) => { setSettings(data); setAddress(data.endpoint) })
      .catch(() => { if (!controller.signal.aborted) setError('无法读取设置，请关闭后重试。') })
    return () => controller.abort()
  }, [])

  async function save(event) {
    event.preventDefault()
    setSaving(true)
    setError('')
    try {
      await api.put('/api/settings/proxy', {
        enabled: settings.enabled,
        address: address.trim() === settings.endpoint ? undefined : address.trim(),
      })
      onClose()
    } catch (err) {
      setError(typeof err.response?.data === 'string' ? err.response.data : '保存失败，请重试。')
      setSaving(false)
    }
  }

  return (
    <dialog ref={dialog} onCancel={event => { event.preventDefault(); if (!saving) onClose() }}
      aria-labelledby="settings-title"
      className="w-[calc(100%-2rem)] max-w-md rounded-xl p-6 shadow-xl backdrop:bg-black/50 dark:bg-gray-800 dark:text-white">
      <form onSubmit={save} className="space-y-5">
        <h2 id="settings-title" className="text-xl font-semibold">设置</h2>
        {settings && <>
          <label className="block text-sm">代理地址
            <input type="text" value={address} disabled={saving} autoComplete="off"
              placeholder="http://192.168.1.10:7890"
              onChange={e => setAddress(e.target.value)}
              className="mt-2 w-full rounded-md border bg-transparent px-3 py-2" />
          </label>
          <p className="text-sm text-gray-600 dark:text-gray-300">
            检查更新和 Go 下载使用此代理。留空时直连镜像仓库。
          </p>
          <label className="block text-sm">镜像下载方式
            <select value={settings.enabled ? 'go' : 'docker'} disabled={saving}
              onChange={e => setSettings({ ...settings, enabled: e.target.value === 'go' })}
              className="mt-2 w-full rounded-md border bg-transparent px-3 py-2 dark:bg-gray-800">
              <option value="docker">Docker 引擎拉取（默认）</option>
              <option value="go">Go 下载并导入 Docker</option>
            </select>
          </label>
          <p className="text-sm text-gray-600 dark:text-gray-300">
            {settings.enabled ? '按检查到的摘要下载并校验镜像，再重建容器。' : '由 NAS 的 Docker 引擎拉取，使用引擎自身的网络配置。'}
          </p>
        </>}
        {!settings && !error && <p role="status">正在读取设置…</p>}
        {error && <p role="alert" className="text-sm text-red-600">{error}</p>}
        <div className="flex justify-end gap-3">
          <button type="button" onClick={onClose} disabled={saving} className="rounded-md border px-4 py-2">取消</button>
          <button disabled={saving || !settings} className="rounded-md bg-primary-600 px-4 py-2 text-white disabled:opacity-50">
            {saving ? '保存中…' : '保存'}
          </button>
        </div>
      </form>
    </dialog>
  )
}
