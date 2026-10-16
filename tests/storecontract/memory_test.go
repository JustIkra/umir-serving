package storecontract_test

import (
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/internal/adapters/memory"
	"github.com/JustIkra/umir-serving/tests/storecontract"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func TestMemoryStore(t *testing.T) {
	storecontract.Run(t, func(t *testing.T, options storecontract.Options) storecontract.Fixture {
		clock := &fakeClock{now: time.Now().UTC()}
		a := memory.NewProposalStore(memory.WithClock(clock.Now), memory.WithMaxActiveProposals(options.MaxActiveProposals))
		b := memory.NewProposalStore(memory.WithClock(clock.Now), memory.WithMaxActiveProposals(options.MaxActiveProposals))
		return storecontract.Fixture{Store: a, Other: b, Advance: clock.Advance}
	})
}
