import React, { useEffect, useState } from 'react'
import { Network, Save } from 'lucide-react'
import api from '../api/client.js'

export function SettingsPage() {
  const [saved, setSaved] = useState(false)
  const [settings, setSettings] = useState(null)
  const [address, setAddress] = useState('')
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    const controller = new AbortController()
    api.get('/api/settings/proxy', { signal: controller.signal })
      .then(({ data }) => { setSettings(data); setAddress(data.endpoint) })
      .catch(() => { if (!controller.signal.aborted) setError('无法读取设置，请重新进入设置页。') })
    return () => controller.abort()
  }, [])

  async function save(event) {
    event.preventDefault()
    setSaved(false)
    setSaving(true)
    setError('')
    try {
      const { data } = await api.put('/api/settings/proxy', {
        address: address.trim() === settings.endpoint ? undefined : address.trim(),
      })
      setSettings(data)
      setAddress(data.endpoint)
      setSaved(true)
    } catch (err) {
      setError(typeof err.response?.data === 'string' ? err.response.data : '保存失败，请重试。')
    } finally {
      setSaving(false)
    }
  }

  return (
    <section aria-labelledby="settings-title" className="mx-auto w-full max-w-7xl px-2 sm:px-6 py-4 text-gray-900 dark:text-gray-100">
      <form onSubmit={save} onChange={() => setSaved(false)}>
        <header className="flex flex-col items-start gap-4 sm:flex-row sm:items-center sm:justify-between">
          <div>
            <h1 id="settings-title" className="text-2xl font-bold text-gray-900 dark:text-white">设置</h1>
            <p className="mt-1 text-gray-600 dark:text-gray-400">配置镜像更新检查使用的代理</p>
          </div>
          <button type="submit" disabled={saving || !settings}
            className="inline-flex items-center gap-2 rounded-lg bg-primary-600 px-4 py-2 text-sm font-medium text-white hover:bg-primary-700 disabled:opacity-50">
            <Save className="h-4 w-4" aria-hidden="true" />{saving ? '保存中…' : '保存设置'}
          </button>
        </header>
        <div className="mt-8 max-w-2xl">
        {settings &&
          <section aria-labelledby="proxy-heading" className="card flex flex-col rounded-2xl p-4">
            <div className="mb-4 flex items-center gap-3">
              <span className="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-primary-600 text-white">
                <Network className="h-5 w-5" aria-hidden="true" />
              </span>
              <h2 id="proxy-heading" className="text-lg font-semibold">
                <label htmlFor="proxy-address">代理地址</label>
              </h2>
            </div>
            <p id="proxy-help" className="mb-3 text-sm leading-6 text-gray-600 dark:text-gray-400">
              DockerCopilot 检查镜像更新时使用此代理。留空时直连镜像仓库。
            </p>
            <input id="proxy-address" aria-describedby="proxy-help" type="text"
              value={address} disabled={saving} autoComplete="off" placeholder="http://192.168.1.10:7890"
              onChange={e => setAddress(e.target.value)}
              className="input mt-auto text-sm" />
          </section>
        }
        </div>
        {!settings && !error && <p role="status">正在读取设置…</p>}
        {error && <p role="alert" className="mt-4 rounded-lg border border-red-200 bg-red-50 p-3 text-sm text-red-800 dark:border-red-900 dark:bg-red-900/20 dark:text-red-200">{error}</p>}
        {saved && <p role="status" className="mt-4 rounded-lg border border-green-200 bg-green-50 p-3 text-sm text-green-800 dark:border-green-900 dark:bg-green-900/20 dark:text-green-200">设置已保存</p>}
      </form>
    </section>
  )
}
