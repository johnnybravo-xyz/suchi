// SPDX-License-Identifier: AGPL-3.0-or-later
import { t } from './i18n.svelte.js'


export const TAGS_SETTINGS_HASH = '#/settings?tab=archive&section=metadata&metadata=tags'
export const CUSTOM_FIELDS_SETTINGS_HASH = '#/settings?tab=archive&section=metadata&metadata=custom_fields'

export const FILING_TREE_SETTINGS_ITEM = {
  name: 'filing-tree', get label() { return t('configuration.filingTree.label') }, icon: 'docs',
  get description() { return t('configuration.filingTree.description') },
  href: '#/settings?tab=archive&section=filing-tree',
}

export const ARCHIVE_SETTINGS_GROUPS = [
  {
    name: 'structure',
    get label() { return t('configuration.structure.label') },
    get description() { return t('configuration.structure.description') },
    items: [
      {
        name: 'users', get label() { return t('configuration.users.label') }, icon: 'shield',
        get description() { return t('configuration.users.description') },
        href: '#/settings?tab=archive&section=users',
      },
      {
        name: 'metadata', get label() { return t('configuration.metadata.label') }, icon: 'tag',
        get description() { return t('configuration.metadata.description') },
        href: '#/settings?tab=archive&section=metadata',
      },
    ],
  },
  {
    name: 'intake',
    get label() { return t('configuration.intake.label') },
    get description() { return t('configuration.intake.description') },
    items: [
      {
        name: 'sources', get label() { return t('configuration.sources.label') }, icon: 'upload',
        get description() { return t('configuration.sources.description') },
        href: '#/settings?tab=archive&section=sources',
      },
      {
        name: 'mail', get label() { return t('configuration.mail.label') }, icon: 'mail',
        get description() { return t('configuration.mail.description') },
        href: '#/settings?tab=archive&section=mail',
      },
    ],
  },
  {
    name: 'processing',
    get label() { return t('configuration.processing.label') },
    get description() { return t('configuration.processing.description') },
    items: [
      {
        name: 'llm', get label() { return t('configuration.classification.label') }, icon: 'zap',
        get description() { return t('configuration.classification.description') },
        href: '#/settings?tab=archive&section=llm',
      },
      {
        name: 'automations', get label() { return t('configuration.automations.label') }, icon: 'zap',
        get description() { return t('configuration.automations.description') },
        href: '#/settings?tab=archive&section=automations',
      },
    ],
  },
  {
    name: 'advanced',
    get label() { return t('configuration.advanced.label') },
    get description() { return t('configuration.advanced.description') },
    items: [
      {
        name: 'folder-layouts', get label() { return t('configuration.folderLayouts.label') }, icon: 'docs',
        get description() { return t('configuration.folderLayouts.description') },
        href: '#/settings?tab=archive&section=folder-layouts',
      },
      {
        name: 'scan-processing', get label() { return t('configuration.scanProcessing.label') }, icon: 'upload',
        get description() { return t('configuration.scanProcessing.description') },
        href: '#/settings?tab=archive&section=scan-processing',
      },
      {
        name: 'preferences', get label() { return t('configuration.preferences.label') }, icon: 'settings',
        get description() { return t('configuration.preferences.description') },
        href: '#/settings?tab=archive&section=preferences',
      },
    ],
  },
]

export const ARCHIVE_SETTINGS_ITEMS = [FILING_TREE_SETTINGS_ITEM, ...ARCHIVE_SETTINGS_GROUPS.flatMap((group) => group.items)]
