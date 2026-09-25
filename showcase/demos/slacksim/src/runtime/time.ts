// Deterministic time formatting for captures. A message ts is an epoch-seconds
// string ("1699999999.000000"). We format in a FIXED timezone (UTC) so the same
// scenario renders identical clock text on any machine — a real Slack client
// shows local time, but a demo needs byte-stable output.

function toDate(ts: string): Date {
  return new Date(Math.floor(Number(ts) * 1000))
}

const timeFmt = new Intl.DateTimeFormat('en-US', {
  timeZone: 'UTC',
  hour: 'numeric',
  minute: '2-digit',
})

/** "4:06 PM" */
export function formatTime(ts: string): string {
  return timeFmt.format(toDate(ts))
}

const weekdayFmt = new Intl.DateTimeFormat('en-US', { timeZone: 'UTC', weekday: 'long' })
const monthFmt = new Intl.DateTimeFormat('en-US', { timeZone: 'UTC', month: 'long' })

function ordinal(n: number): string {
  const s = ['th', 'st', 'nd', 'rd']
  const v = n % 100
  return n + (s[(v - 20) % 10] ?? s[v] ?? s[0])
}

/** "Thursday, August 28th" */
export function formatDateDivider(ts: string): string {
  const d = toDate(ts)
  return `${weekdayFmt.format(d)}, ${monthFmt.format(d)} ${ordinal(d.getUTCDate())}`
}

/** The UTC calendar day key for grouping messages under date dividers. */
export function dayKey(ts: string): string {
  const d = toDate(ts)
  return `${d.getUTCFullYear()}-${d.getUTCMonth()}-${d.getUTCDate()}`
}

/** "Last reply 18 days ago" style relative text, deterministic against a base. */
export function relativeReply(count: number): string {
  return count === 1 ? '1 reply' : `${count} replies`
}
