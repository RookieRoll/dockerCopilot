// The effect owner supplies a latest-value reader so a delayed page-wide task
// query can safely map against containers fetched after the query began.
// Failures remain visible to the owner and retry forever with capped backoff;
// a transient API outage must not permanently disable refresh recovery.
export function startTaskRecoveryDiscovery(fetchRecords, getContainers, applyRecords, {
  onError = () => {},
  initialDelayMs = 1000,
  maxDelayMs = 30000,
  schedule = (callback, delay) => setTimeout(callback, delay),
  cancelSchedule = (timer) => clearTimeout(timer),
} = {}) {
  let active = true
  let retryAttempt = 0
  let timer = null
  let controller = null

  const discover = async () => {
    if (!active) return
    controller = new AbortController()
    try {
      const records = await fetchRecords({ signal: controller.signal })
      if (!active) return
      retryAttempt = 0
      onError(null)
      applyRecords(records, getContainers())
    } catch (error) {
      if (!active || controller.signal.aborted) return
      onError(error)
      const delay = Math.min(initialDelayMs * (2 ** retryAttempt), maxDelayMs)
      retryAttempt += 1
      timer = schedule(discover, delay)
    }
  }

  Promise.resolve().then(discover)
  return () => {
    active = false
    controller?.abort()
    if (timer !== null) cancelSchedule(timer)
  }
}
