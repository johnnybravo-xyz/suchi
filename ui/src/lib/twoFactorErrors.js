// SPDX-License-Identifier: AGPL-3.0-or-later
import { localeState, t } from './i18n.svelte.js'

// waitText turns Retry-After seconds into a localized "in 2 minutes".
function waitText(seconds) {
  const format = new Intl.RelativeTimeFormat(localeState.locale, { numeric: 'always' })
  if (seconds < 90) return format.format(Math.max(1, seconds), 'second')
  if (seconds < 90 * 60) return format.format(Math.ceil(seconds / 60), 'minute')
  return format.format(Math.ceil(seconds / 3600), 'hour')
}

// twoFactorErrorMessage maps the server's two-factor error codes to wording a
// person can act on, or returns null so the caller keeps its own message.
export function twoFactorErrorMessage(error) {
  switch (error?.code) {
    case 'too_many_attempts':
      return error.retryAfter > 0
        ? t('twoFactorErrors.tooManyAttemptsWait', { wait: waitText(error.retryAfter) })
        : t('twoFactorErrors.tooManyAttempts')
    case 'two_factor_locked':
      return t('twoFactorErrors.locked')
    case 'two_factor_required':
      return t('twoFactorErrors.codeRequired')
    default:
      return null
  }
}
