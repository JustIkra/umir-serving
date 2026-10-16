// Package storecontract is the one behavior suite that every proposal and
// attempt store must pass: the in-process store used by tests and fixtures and
// the PostgreSQL store. Run it with a factory of the store under test.
package storecontract

import (
	"context"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/internal/application"
)

// Store is what OrdersService needs from one restaurant's storage.
type Store interface {
	application.ProposalStore
	application.ConfirmationAttemptStore
}

// Options are the limits a test asks the fixture to enforce.
type Options struct {
	MaxActiveProposals int
}

// Fixture is a store of restaurant A and another one of restaurant B on the
// same storage, with a way to move the clock the storage uses.
type Fixture struct {
	Store   Store
	Other   Store
	Advance func(time.Duration)
	// Sweep runs one retention pass over the whole storage.
	Sweep func(ctx context.Context) (application.SweepResult, error)
	// PickDue is the reconciler selection over the whole storage.
	PickDue func(ctx context.Context, limit int, shift time.Duration) ([]application.DueAttempt, error)
	// OrgID and RestaurantID of Store, as the picker reports them.
	OrgID        string
	RestaurantID string
}

// Factory builds a fresh fixture on empty storage.
type Factory func(t *testing.T, options Options) Fixture
