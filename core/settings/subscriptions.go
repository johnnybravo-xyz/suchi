// SPDX-License-Identifier: AGPL-3.0-or-later
package settings

import "context"

// SubscriptionProvider is the browser settings seam. Credentials and transport
// details stay in the compiled integration, outside this contract.
type SubscriptionProvider interface {
	Start(context.Context, int64) (SubscriptionLogin, error)
	Poll(context.Context, int64) (SubscriptionPoll, error)
	Cancel(context.Context, int64) error
	Disconnect(context.Context) error
	Connected(context.Context) bool
	Models(context.Context) ([]SubscriptionModel, error)
}

type SubscriptionStatus struct {
	Provider  string `json:"subscription_provider"`
	Model     string `json:"subscription_model"`
	Connected bool   `json:"subscription_connected"`
}

type SubscriptionLogin struct {
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	Interval        int    `json:"interval"`
}

type SubscriptionPoll struct {
	Pending   bool `json:"pending"`
	Connected bool `json:"connected"`
}

type SubscriptionModel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func SubscriptionModelKey(provider string) string { return "llm.subscriptions." + provider + ".model" }
