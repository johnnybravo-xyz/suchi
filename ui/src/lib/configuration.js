// SPDX-License-Identifier: AGPL-3.0-or-later

export const TAGS_SETTINGS_HASH = '#/settings?tab=archive&section=metadata&metadata=tags'
export const CUSTOM_FIELDS_SETTINGS_HASH = '#/settings?tab=archive&section=metadata&metadata=custom_fields'

export const FILING_TREE_SETTINGS_ITEM = {
  name: 'filing-tree', label: 'Filing tree', icon: 'docs',
  description: 'Choose, compose, review, import, or export the structure used to file documents.',
  href: '#/settings?tab=archive&section=filing-tree',
}

export const ARCHIVE_SETTINGS_GROUPS = [
  {
    name: 'structure',
    label: 'People and access',
    description: 'Who can use the archive and how document metadata is managed.',
    items: [
      {
        name: 'users', label: 'People', icon: 'shield',
        description: 'Manage users, groups, roles, and archive access.',
        href: '#/settings?tab=archive&section=users',
      },
      {
        name: 'metadata', label: 'Metadata', icon: 'settings',
        description: 'Manage document labels, correspondents, and custom fields.',
        href: '#/settings?tab=archive&section=metadata',
      },
    ],
  },
  {
    name: 'intake',
    label: 'Document intake',
    description: 'Where new documents enter Suchi.',
    items: [
      {
        name: 'sources', label: 'Watched folder', icon: 'upload',
        description: 'Configure filesystem intake and document ownership.',
        href: '#/settings?tab=archive&section=sources',
      },
      {
        name: 'mail', label: 'Email intake', icon: 'mail',
        description: 'Connect mailboxes and choose which messages become documents.',
        href: '#/settings?tab=archive&section=mail',
      },
    ],
  },
  {
    name: 'processing',
    label: 'Processing',
    description: 'How incoming documents are understood and filed.',
    items: [
      {
        name: 'llm', label: 'Classification', icon: 'zap',
        description: 'Configure archive learning and an optional document model.',
        href: '#/settings?tab=archive&section=llm',
      },
      {
        name: 'automations', label: 'Automations', icon: 'zap',
        description: 'Build rules that tag, title, and file incoming documents.',
        href: '#/settings?tab=archive&section=automations',
      },
    ],
  },
  {
    name: 'advanced',
    label: 'Advanced',
    description: 'Local filesystem output, OCR, and archive recovery settings.',
    items: [
      {
        name: 'folder-layouts', label: 'Folder layouts', icon: 'docs',
        description: 'Control how documents appear in the local rendered filesystem.',
        href: '#/settings?tab=archive&section=folder-layouts',
      },
      {
        name: 'preferences', label: 'OCR and backups', icon: 'settings',
        description: 'Set OCR languages and the automatic backup schedule.',
        href: '#/settings?tab=archive&section=preferences',
      },
    ],
  },
]

export const ARCHIVE_SETTINGS_ITEMS = [FILING_TREE_SETTINGS_ITEM, ...ARCHIVE_SETTINGS_GROUPS.flatMap((group) => group.items)]
