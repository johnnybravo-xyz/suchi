// SPDX-License-Identifier: AGPL-3.0-or-later
package llmclassifier

import (
	"context"
	"fmt"
	suchicrypto "github.com/johnnybravo-xyz/suchi/core/crypto"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/settings"
)

const OpenAIChatGPT = "openai_chatgpt"

type subscriptionProvider interface {
	settings.SubscriptionProvider
	endpoint() string
	complete(context.Context, *runtime, []byte) ([]byte, error)
}

// Subscriptions owns the private registry of shipped account adapters.
type Subscriptions struct {
	providers map[string]subscriptionProvider
}

func NewSubscriptions(database *db.DB, key *suchicrypto.AEADKey) *Subscriptions {
	return &Subscriptions{providers: map[string]subscriptionProvider{OpenAIChatGPT: NewChatGPTLogin(database, key)}}
}

func (s *Subscriptions) provider(id string) (subscriptionProvider, error) {
	if s != nil {
		if p, ok := s.providers[id]; ok {
			return p, nil
		}
	}
	return nil, fmt.Errorf("unknown subscription provider %q", id)
}

func (s *Subscriptions) Provider(id string) (settings.SubscriptionProvider, bool) {
	p, err := s.provider(id)
	return p, err == nil
}

func (l *ChatGPTLogin) endpoint() string { return ChatGPTEndpoint }
