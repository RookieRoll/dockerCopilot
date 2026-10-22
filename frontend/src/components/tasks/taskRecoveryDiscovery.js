// The effect owner supplies a latest-value reader so a delayed page-wide task
// query can safely map against containers fetched after the query began.
export function startTaskRecoveryDiscovery(fetchRecords, getContainers, applyRecords) {
  let active = true
  Promise.resolve().then(fetchRecords).then((records) => {
    if (active) applyRecords(records, getContainers())
  }).catch((error) => {
    if (active) console.warn('Unable to discover active container update tasks:', error)
  })
  return () => { active = false }
}
