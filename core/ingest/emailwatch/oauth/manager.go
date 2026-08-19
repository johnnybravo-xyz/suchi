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
	activeID string
	scopes   []string
	clients  map[string]*Client
}

func NewManager(activeID string, scopes []string) (*Manager, error) {
	m := &Manager{scopes: append([]string(nil), scopes...), clients: make(map[string]*Client)}
	if err := m.SetActive(activeID); err != nil {
		return nil, err
	}
	return m, nil
}

func UsableClientID(id string) bool {
	id = strings.TrimSpace(id)
	return id != "" && id != DefaultClientID
}

// Validate constructs the same MSAL client SetActive would cache without
// changing the active registration.
func (m *Manager) Validate(clientID string) error {
	if !UsableClientID(clientID) {
		return ErrUnavailable
	}
	_, err := New(Options{ClientID: strings.TrimSpace(clientID), Scopes: m.scopes})
	return err
}

func (m *Manager) SetActive(clientID string) error {
	clientID = strings.TrimSpace(clientID)
	if !UsableClientID(clientID) {
		m.mu.Lock()
		m.activeID = ""
		m.mu.Unlock()
		return nil
	}
	client, err := New(Options{ClientID: clientID, Scopes: m.scopes})
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.clients[clientID] = client
	m.activeID = clientID
	m.mu.Unlock()
	return nil
}

func (m *Manager) Active() (*Client, string, bool) {
	if m == nil {
		return nil, "", false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.activeID == "" {
		return nil, "", false
	}
	return m.clients[m.activeID], m.activeID, true
}

func (m *Manager) ActiveID() string {
	_, id, _ := m.Active()
	return id
}

func (m *Manager) Ready() bool {
	_, _, ready := m.Active()
	return ready
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
