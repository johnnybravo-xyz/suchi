// SPDX-License-Identifier: AGPL-3.0-or-later
import { t } from './i18n.svelte.js'

const CAPABILITY_FEATURES = Object.freeze({
  archive_chat: 'llm_connections',
  mailboxes: 'email_ingestion',
  share_links: 'share_links',
})


export const USER_CAPABILITIES = Object.freeze([
  Object.freeze({
    key: 'archive_chat',
    get label() { return t('capabilities.archiveChat.label') },
    get description() { return t('capabilities.archiveChat.description') },
  }),
  Object.freeze({
    key: 'archive_intelligence',
    get label() { return t('capabilities.archiveIntelligence.label') },
    get description() { return t('capabilities.archiveIntelligence.description') },
  }),
  Object.freeze({
    key: 'mailboxes',
    get label() { return t('capabilities.mailboxes.label') },
    get description() { return t('capabilities.mailboxes.description') },
  }),
  Object.freeze({
    key: 'share_links',
    get label() { return t('capabilities.shareLinks.label') },
    get description() { return t('capabilities.shareLinks.description') },
  }),
  Object.freeze({
    key: 'share_views',
    get label() { return t('capabilities.shareViews.label') },
    get description() { return t('capabilities.shareViews.description') },
  }),
])

export function hasFeature(user, feature) {
  return user?.features?.[feature] === true
}

export function availableCapabilities(user) {
  return USER_CAPABILITIES.filter(({ key }) => {
    const feature = CAPABILITY_FEATURES[key]
    return !feature || hasFeature(user, feature)
  })
}

export function hasCapability(user, capability) {
  const feature = CAPABILITY_FEATURES[capability]
  if (feature && !hasFeature(user, feature)) return false
  return user?.role === 'admin' || (user?.capabilities || []).includes(capability)
}
