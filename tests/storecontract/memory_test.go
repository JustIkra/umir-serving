package storecontract_test

import (
	"sync"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/internal/adapters/memory"
	"github.com/JustIkra/umir-serving/tests/storecontract"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

const (
	orgA        = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	restaurantA = "a1a1a1a1-a1a1-4a1a-8a1a-a1a1a1a1a1a1"
	restaurantB = "b2b2b2b2-b2b2-4b2b-8b2b-b2b2b2b2b2b2"
)

func TestMemoryStore(t *testing.T) {
	storecontract.Run(t, func(t *testing.T, options storecontract.Options) storecontract.Fixture {
		clock := &fakeClock{now: time.Now().UTC()}
		storage := memory.NewStorage(memory.WithClock(clock.Now), memory.WithMaxActiveProposals(options.MaxActiveProposals))
		return storecontract.Fixture{
			Store: storage.Store(orgA, restaurantA), Other: storage.Store(orgA, restaurantB), Advance: clock.Advance, Now: clock.Now,
			Sweep: storage.Sweep, PickDue: storage.PickDue, OrgID: orgA, RestaurantID: restaurantA,
		}
	})
}
