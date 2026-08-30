export const USER_CAPABILITIES = Object.freeze([
  Object.freeze({
    key: 'archive_chat',
    label: 'Ask the archive',
    setupLabel: 'Ask the archive',
    description: 'Ask grounded questions across documents they can read.',
  }),
  Object.freeze({
    key: 'mailboxes',
    label: 'Manage mailboxes',
    setupLabel: 'Manage own mailboxes',
    description: 'Connect and manage their own mail intake.',
  }),
  Object.freeze({
    key: 'share_links',
    label: 'Create share links',
    setupLabel: 'Create share links',
    description: 'Share documents using revocable links.',
  }),
  Object.freeze({
    key: 'share_views',
    label: 'Share saved views',
    setupLabel: 'Share saved views',
    description: "Publish saved views to every user's dashboard.",
  }),
])

export function hasCapability(user, capability) {
  return user?.role === 'admin' || (user?.capabilities || []).includes(capability)
}
