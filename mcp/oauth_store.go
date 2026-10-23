package mcp

import (
	"context"
	"sync"
)

// TokenStore keeps OAuth tokens, by the canonical URL of the MCP server,
// and dynamic client registrations, by the issuer of the authorization
// server that issued them: registrations must not be used with another
// authorization server. Tokens and client secrets are credentials; stores
// must keep them confidential.
type TokenStore interface {
	// Token returns the token for server, or nil if there is none.
	Token(ctx context.Context, server string) (*Token, error)
	// SetToken saves the token for server; nil deletes it.
	SetToken(ctx context.Context, server string, tok *Token) error
	// Client returns the registration with issuer, or nil.
	Client(ctx context.Context, issuer string) (*ClientRegistration, error)
	// SetClient saves the registration with issuer; nil deletes it.
	SetClient(ctx context.Context, issuer string, reg *ClientRegistration) error
}

// storeData is what the stores keep.
type storeData struct {
	Tokens  map[string]*Token              `json:"tokens,omitempty"`
	Clients map[string]*ClientRegistration `json:"clients,omitempty"`
}

func (d *storeData) setToken(server string, tok *Token) {
	if tok == nil {
		delete(d.Tokens, server)
		return
	}
	if d.Tokens == nil {
		d.Tokens = map[string]*Token{}
	}
	c := *tok
	d.Tokens[server] = &c
}

func (d *storeData) setClient(issuer string, reg *ClientRegistration) {
	if reg == nil {
		delete(d.Clients, issuer)
		return
	}
	if d.Clients == nil {
		d.Clients = map[string]*ClientRegistration{}
	}
	c := *reg
	d.Clients[issuer] = &c
}

func cloneToken(t *Token) *Token {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

func cloneClient(r *ClientRegistration) *ClientRegistration {
	if r == nil {
		return nil
	}
	c := *r
	return &c
}

// MemoryTokenStore keeps tokens in memory for the life of the process.
type MemoryTokenStore struct {
	mu   sync.Mutex
	data storeData
}

// NewMemoryTokenStore returns an empty in-memory store.
func NewMemoryTokenStore() *MemoryTokenStore { return &MemoryTokenStore{} }

// Token implements TokenStore.
func (s *MemoryTokenStore) Token(_ context.Context, server string) (*Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneToken(s.data.Tokens[server]), nil
}

// SetToken implements TokenStore.
func (s *MemoryTokenStore) SetToken(_ context.Context, server string, tok *Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.setToken(server, tok)
	return nil
}

// Client implements TokenStore.
func (s *MemoryTokenStore) Client(_ context.Context, issuer string) (*ClientRegistration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneClient(s.data.Clients[issuer]), nil
}

// SetClient implements TokenStore.
func (s *MemoryTokenStore) SetClient(_ context.Context, issuer string, reg *ClientRegistration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.setClient(issuer, reg)
	return nil
}
