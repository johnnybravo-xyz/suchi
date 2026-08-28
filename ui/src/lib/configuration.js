export const SETUP_STEPS = [
  { name: 'archive', label: 'Filing tree' },
  { name: 'users', label: 'People' },
  { name: 'sources', label: 'Watched folder' },
  { name: 'mail', label: 'Email intake' },
  { name: 'llm', label: 'Classification' },
  { name: 'automations', label: 'Automations' },
  { name: 'preferences', label: 'OCR and backups' },
]

export const ARCHIVE_SETTINGS_GROUPS = [
  {
    name: 'structure',
    label: 'Structure and access',
    description: 'How documents are organized and administered.',
    items: [
      {
        name: 'archive', label: 'Filing tree', icon: 'docs',
        description: 'Choose or replace the structure used to file documents.',
        href: '#/settings?tab=archive&section=archive',
      },
      {
        name: 'users', label: 'People and metadata', icon: 'shield',
        description: 'Manage users, groups, custom fields, and taxonomy.',
        href: '#/settings?tab=archive&section=users',
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
    name: 'maintenance',
    label: 'Maintenance',
    description: 'OCR coverage and archive recovery settings.',
    items: [
      {
        name: 'preferences', label: 'OCR and backups', icon: 'settings',
        description: 'Set OCR languages and the automatic backup schedule.',
        href: '#/settings?tab=archive&section=preferences',
      },
    ],
  },
]

export const ARCHIVE_SETTINGS_ITEMS = ARCHIVE_SETTINGS_GROUPS.flatMap((group) => group.items)
