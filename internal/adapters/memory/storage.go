package memory

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/application"
)

// Storage is the in-process counterpart of the PostgreSQL backend: one
// ProposalStore per restaurant plus the passes that cross restaurants. Tests
// and fixtures use it; the production binary does not link it.
type Storage struct {
	mu        sync.Mutex
	now       func() time.Time
	maxActive int
	stores    map[string]*ProposalStore
	// closedAt marks restaurants that were closed; their rows are removed after
	// the retention of a closed restaurant.
	closedAt map[string]time.Time
}

var (
	_ application.AttemptPicker = (*Storage)(nil)
	_ application.Sweeper       = (*Storage)(nil)
)

// NewStorage builds an empty storage. Options apply to every restaurant store.
func NewStorage(options ...Option) *Storage {
	probe := NewProposalStore(options...)
	return &Storage{now: probe.now, maxActive: probe.maxActive, stores: make(map[string]*ProposalStore), closedAt: make(map[string]time.Time)}
}

type storeKey struct{ org, restaurant string }

func (k storeKey) String() string { return k.org + "/" + k.restaurant }

// Store returns the store of one restaurant, creating it on first use.
func (s *Storage) Store(orgID, restaurantID string) *ProposalStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := storeKey{orgID, restaurantID}.String()
	if store, ok := s.stores[key]; ok {
		return store
	}
	store := NewProposalStore(WithClock(s.now), WithMaxActiveProposals(s.maxActive))
	store.org, store.restaurant = orgID, restaurantID
	s.stores[key] = store
	return store
}

func (s *Storage) snapshot() []*ProposalStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	stores := make([]*ProposalStore, 0, len(s.stores))
	for _, store := range s.stores {
		stores = append(stores, store)
	}
	return stores
}

// PickDue implements application.AttemptPicker.
func (s *Storage) PickDue(ctx context.Context, limit int, shift time.Duration) ([]application.DueAttempt, error) {
	if limit <= 0 || shift <= 0 {
		return nil, contract.ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var due []dueAttempt
	now := s.now()
	for _, store := range s.snapshot() {
		if s.isClosed(store) {
			continue
		}
		due = append(due, store.pickDue(now, shift)...)
	}
	sort.SliceStable(due, func(i, j int) bool { return due[i].dueAt.Before(due[j].dueAt) })
	if len(due) > limit {
		// Rows beyond the limit stay due: give their next check back.
		for _, extra := range due[limit:] {
			s.Store(extra.OrgID, extra.RestaurantID).restoreCheck(extra.CommandID, extra.dueAt)
		}
		due = due[:limit]
	}
	out := make([]application.DueAttempt, len(due))
	for i, d := range due {
		out[i] = d.DueAttempt
	}
	return out, nil
}

func (s *Storage) isClosed(store *ProposalStore) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, closed := s.closedAt[storeKey{store.org, store.restaurant}.String()]
	return closed
}

// MarkClosed records that a restaurant was closed, for retention and picking.
func (s *Storage) MarkClosed(orgID, restaurantID string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closedAt[storeKey{orgID, restaurantID}.String()] = at
}

// Sweep implements application.Sweeper.
func (s *Storage) Sweep(ctx context.Context, limit int) (application.SweepResult, error) {
	if limit <= 0 {
		return application.SweepResult{}, contract.ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return application.SweepResult{}, err
	}
	var total application.SweepResult
	now := s.now()
	for _, store := range s.snapshot() {
		s.mu.Lock()
		closed, isClosed := s.closedAt[storeKey{store.org, store.restaurant}.String()]
		s.mu.Unlock()
		result := store.sweep(now, limit, isClosed && now.Sub(closed) > closedRestaurantRetention)
		total.Proposals += result.Proposals
		total.Attempts += result.Attempts
		total.ClosedRestaurant += result.ClosedRestaurant
	}
	return total, nil
}
