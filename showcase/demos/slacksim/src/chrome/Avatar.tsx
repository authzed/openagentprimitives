import type { SimUser } from '../store/types'

// Slack avatars are rounded squares (radius ~8px at 36px). Bots/apps get the
// same shape; the APP/AGENT distinction is a text badge on the name line, not
// the avatar. Falls back to initials on a colored tile when there is no image.
export function Avatar({ user, size = 36 }: { user: SimUser | undefined; size?: number }) {
  const radius = Math.round(size * 0.22)
  if (user?.avatar.url) {
    return (
      <img
        className="sk-avatar"
        src={user.avatar.url}
        alt={user.name}
        style={{ width: size, height: size, borderRadius: radius }}
      />
    )
  }
  const initials = user?.avatar.initials ?? '?'
  const color = user?.avatar.color ?? '#616061'
  return (
    <div
      className="sk-avatar sk-avatar--initials"
      style={{
        width: size,
        height: size,
        borderRadius: radius,
        background: color,
        fontSize: Math.round(size * 0.42),
      }}
      aria-label={user?.name}
    >
      {initials}
    </div>
  )
}
