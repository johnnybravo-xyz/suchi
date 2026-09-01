package oauth

import (
	"errors"
	"strings"
	"sync"
)

var ErrUnavailable = errors.New("oauth/microsoft: no usable public client configured")

// Manager keeps the active registration for new sign-ins while retaining
// clients used by existing mailbox credentials. Public clients have no secret,
// so caching one immutable Client per application id is sufficient.
type Manager struct {
	mu       sync.RWMutex
	active   *Client
	activeID string
	scopes   []string
	clients  map[string]*Client
}

func NewManager(activeID string, scopes []string) (*Manager, error) {
	activeID = strings.TrimSpace(activeID)
	m := &Manager{
		activeID: activeID,
		scopes:   append([]string(nil), scopes...),
		clients:  make(map[string]*Client),
	}
	if !UsableClientID(activeID) {
		m.activeID = ""
		return m, nil
	}
	client, err := New(Options{ClientID: activeID, Scopes: m.scopes})
	if err != nil {
		return nil, err
	}
	m.active = client
	m.clients[activeID] = client
	return m, nil
}

func UsableClientID(id string) bool {
	id = strings.TrimSpace(id)
	return id != "" && id != UnconfiguredClientID
}

func (m *Manager) Active() (*Client, string, bool) {
	if m == nil || m.active == nil {
		return nil, "", false
	}
	return m.active, m.activeID, true
}

func (m *Manager) Ready() bool {
	return m != nil && m.active != nil
}

func (m *Manager) ClientFor(clientID string) (*Client, error) {
	if m == nil || !UsableClientID(clientID) {
		return nil, ErrUnavailable
	}
	clientID = strings.TrimSpace(clientID)
	m.mu.RLock()
	client := m.clients[clientID]
	m.mu.RUnlock()
	if client != nil {
		return client, nil
	}
	created, err := New(Options{ClientID: clientID, Scopes: m.scopes})
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if existing := m.clients[clientID]; existing != nil {
		created = existing
	} else {
		m.clients[clientID] = created
	}
	m.mu.Unlock()
	return created, nil
}
