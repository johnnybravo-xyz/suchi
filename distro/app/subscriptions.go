// SPDX-License-Identifier: AGPL-3.0-or-later
package app

import (
	"context"
	"github.com/johnnybravo-xyz/suchi/core/settings"
)

// Disconnect also stops inference when this account powers the active model.
type subscriptionSettingsProvider struct {
	settings.SubscriptionProvider
	disconnect func(context.Context) error
}

func (p subscriptionSettingsProvider) Disconnect(ctx context.Context) error { return p.disconnect(ctx) }
