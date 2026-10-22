const TERMINAL_STATUSES = new Set(['succeeded', 'failed', 'interrupted'])

export function isTaskPanelTerminal(task, submitError) {
  if (task) return TERMINAL_STATUSES.has(task.status)
  return Boolean(submitError)
}

export function taskPanelControls(task, submitError) {
  return {
    canCollapse: Boolean(task || submitError),
    canDismiss: isTaskPanelTerminal(task, submitError),
  }
}
