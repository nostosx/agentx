// Package artifact defines storage for named binary outputs of agents and tools.
// Implementations live in agentx-contrib; applications own authorization and
// choose the scope from trusted identity, rather than from model arguments.
package artifact

import (
	"context"
	"errors"
)

// ErrNotFound reports an absent artifact in the requested scope.
var ErrNotFound = errors.New("artifact: not found")

// Key identifies an artifact. Scope and ID are exact, case-sensitive strings.
// Implementations must not interpret them as filesystem paths.
type Key struct {
	Scope string `json:"scope"`
	ID    string `json:"id"`
}

// Artifact holds a binary output and application-owned metadata.
// It is not a provider file ID or an expiring download URL.
type Artifact struct {
	Key
	Name      string            `json:"name,omitempty"`
	MediaType string            `json:"media_type"`
	Data      []byte            `json:"data"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// Store persists artifacts atomically per key. Put replaces an existing value;
// applications needing immutable revisions must choose distinct IDs. Get
// returns ErrNotFound for missing keys. Delete is idempotent. Implementations
// must be concurrent-safe and must not retain or return mutable aliases.
// Calls do not provide a transaction across several artifacts.
type Store interface {
	Put(ctx context.Context, value *Artifact) error
	Get(ctx context.Context, key Key) (*Artifact, error)
	Delete(ctx context.Context, key Key) error
}

// Validate checks a key without imposing backend path conventions.
func (k Key) Validate() error {
	if k.Scope == "" || k.ID == "" {
		return errors.New("artifact: scope and ID are required")
	}
	return nil
}

// Validate checks the required artifact fields. Empty files are allowed.
func (a *Artifact) Validate() error {
	if a == nil {
		return errors.New("artifact: nil artifact")
	}
	if err := a.Key.Validate(); err != nil {
		return err
	}
	if a.MediaType == "" {
		return errors.New("artifact: media type is required")
	}
	return nil
}
