import React from 'react'
import { Github, Mail, Heart, MessageSquare, RefreshCw, Server } from 'lucide-react'
import logoImg from '../assets/DockerCopilot-logo.png'
import { TaskStatusPanel } from './tasks/TaskStatusPanel.jsx'

export function About({ versionState = {} }) {
  const { backendVersion, remoteVersion, buildDate, programTask, programUpdateState, versionCheckError, checkForUpdates } = versionState
  const updatePending = ['submitting', 'active', 'uncertain'].includes(programUpdateState)

  return (
    <div className="max-w-7xl mx-auto">

      <div className="px-2 sm:px-6 py-4 pt-4 sm:pt-4 space-y-6">
        {/* 项目展示卡片 */}
        <div className="card p-8 flex flex-col items-center text-center relative overflow-hidden">
          <div className="relative mb-4">
            <div className="absolute inset-0 bg-primary-400/20 blur-xl rounded-full"></div>
            <img
              src={logoImg}
              alt="Docker Copilot"
              className="relative w-20 h-20 rounded-2xl shadow-lg"
            />
          </div>

          <h1 className="text-3xl font-bold text-gray-900 dark:text-white mb-2">Docker Copilot</h1>
          <p className="text-gray-600 dark:text-gray-400 max-w-lg mx-auto mb-6">
            一个简洁、优雅且强大的 Docker 容器管理工具，旨在为您提供流畅的容器运维体验。
          </p>

          <div className="flex flex-wrap items-center justify-center gap-3">
            <a
              href="https://github.com/RookieRoll/dockerCopilot"
              target="_blank"
              rel="noopener noreferrer"
              className="flex items-center gap-2 px-4 py-2 bg-gray-900 text-white rounded-lg hover:bg-gray-800 transition-colors shadow-sm"
            >
              <Github className="h-4 w-4" />
              <span>GitHub</span>
            </a>
            <a
              href="mailto:onlylty@lty.wiki"
              className="flex items-center gap-2 px-4 py-2 bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-300 rounded-lg hover:bg-blue-200 dark:hover:bg-blue-900/50 transition-colors"
            >
              <Mail className="h-4 w-4" />
              <span>联系作者</span>
            </a>
          </div>
        </div>

        <section className="card p-6" aria-labelledby="about-version-heading">
          <div className="mb-4 flex items-center gap-2">
            <Server className="h-5 w-5 text-primary-600 dark:text-primary-400" aria-hidden="true" />
            <h2 id="about-version-heading" className="text-lg font-bold text-gray-900 dark:text-white">版本与程序升级</h2>
          </div>
          <dl className="grid grid-cols-1 gap-3 text-sm sm:grid-cols-3">
            <div>
              <dt className="text-gray-500 dark:text-gray-400">当前运行版本</dt>
              <dd className="mt-1 font-semibold text-gray-900 dark:text-white">{backendVersion || '查询中'}</dd>
            </div>
            <div>
              <dt className="text-gray-500 dark:text-gray-400">远端版本</dt>
              <dd className="mt-1 font-semibold text-gray-900 dark:text-white">{remoteVersion || '未知'}</dd>
            </div>
            <div>
              <dt className="text-gray-500 dark:text-gray-400">构建时间</dt>
              <dd className="mt-1 font-semibold text-gray-900 dark:text-white">{buildDate || '未知'}</dd>
            </div>
          </dl>
          {versionCheckError && (
            <div className="mt-4 flex flex-wrap items-center justify-between gap-3 rounded-lg border border-amber-300 bg-amber-50 p-3 text-sm text-amber-900 dark:border-amber-800 dark:bg-amber-950/40 dark:text-amber-200" role="alert">
              <span>版本检查失败：{versionCheckError}</span>
              <button type="button" onClick={checkForUpdates} className="shrink-0 font-medium underline underline-offset-2">重新检查</button>
            </div>
          )}
          {updatePending && (
            <p className="mt-4 flex items-center gap-2 text-sm text-amber-800 dark:text-amber-300" role="status">
              <RefreshCw className="h-4 w-4 animate-spin" aria-hidden="true" />
              更新任务尚未确认完成。服务重启期间连接中断只表示结果待确认；页面会继续查询，不会重复提交更新。
            </p>
          )}
          <TaskStatusPanel
            task={programTask?.task}
            taskId={programTask?.taskId}
            connectionError={programTask?.connectionError}
            unauthorized={programTask?.unauthorized}
            submitError={programTask?.submitError}
            isTracking={programTask?.isTracking}
          />
          <details className="mt-4 rounded-lg border border-amber-200 bg-amber-50 p-4 text-sm text-amber-950 dark:border-amber-900 dark:bg-amber-950/30 dark:text-amber-100">
            <summary className="cursor-pointer font-semibold">安装异常？查看人工恢复说明</summary>
            <p className="mt-2 leading-relaxed">
              如果任务显示失败/中断，或长时间处于“待确认”，先查看任务错误和服务日志，不要重复提交升级。记录任务 ID，停止管理容器，备份当前程序、交接标记/结果文件和旧程序副本；确认旧程序可用后才将 `.dockerCopilot-previous-&lt;handoff_id&gt;` 恢复为 `dockerCopilot` 并重启服务。不要删除任务数据目录或覆盖唯一备份。
            </p>
            <p className="mt-2 text-xs text-amber-900/80 dark:text-amber-200/80">
              <a className="underline underline-offset-2" href="https://github.com/RookieRoll/dockerCopilot/blob/main/docs/program-update.md#人工恢复" target="_blank" rel="noopener noreferrer">打开完整人工恢复步骤</a>
            </p>
          </details>
        </section>

        <div className="grid grid-cols-1 lg:grid-cols-2 gap-6">
          {/* 致谢 */}
          <div className="card p-6 flex flex-col h-full">
            <div className="flex items-center gap-2 mb-4">
              <Heart className="h-5 w-5 text-red-500" />
              <h3 className="text-lg font-bold text-gray-900 dark:text-white">致谢 / Thanks</h3>
            </div>
            <p className="text-gray-600 dark:text-gray-400 leading-relaxed flex-1">
              非常感谢大家自项目开始以来的使用、建议、鼓励和支持。特别感谢绿联对本项目的支持。没有大家的反馈，Docker Copilot 不会是今天的样子。它是属于我们共同的作品。
            </p>
          </div>

          {/* 反馈 */}
          <div className="card p-6 flex flex-col h-full">
            <div className="flex items-center gap-2 mb-4">
              <MessageSquare className="h-5 w-5 text-green-500" />
              <h3 className="text-lg font-bold text-gray-900 dark:text-white">反馈与建议</h3>
            </div>
            <p className="text-gray-600 dark:text-gray-400 leading-relaxed mb-4">
              在项目使用中遇到 Bug 或有新的功能想法？欢迎提交 Issue 或直接联系我。您的每一个反馈都至关重要。
            </p>
            <a
              href="https://github.com/RookieRoll/dockerCopilot/issues"
              target="_blank"
              rel="noopener noreferrer"
              className="inline-flex items-center text-primary-600 dark:text-primary-400 hover:underline font-medium"
            >
              前往 GitHub Issues &rarr;
            </a>
          </div>
        </div>
      </div>
    </div>
  )
}
