package application

import (
	"context"
	"time"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/domain"
)

type ProposalStore interface {
	Create(context.Context, domain.OrderProposal) error
	Get(context.Context, string) (domain.ProposalRecord, bool, error)
}

type ConfirmationAttemptStore interface {
	BeginAttempt(context.Context, contract.ConfirmationAttempt) (contract.ConfirmationAttempt, bool, error)
	GetAttempt(context.Context, string) (contract.ConfirmationAttempt, bool, error)
	ReserveAttempt(context.Context, string) error
	RememberWarehouseResult(context.Context, string, contract.CommitResult, *contract.OrderProposal) error
	CompleteAttempt(context.Context, string, contract.ConfirmResult, *contract.AttemptError, *domain.OrderProposal) error

	// AcquireLease takes the right to talk to the Warehouse about one proposal.
	// It reports false when another holder has an unexpired lease. A proposal
	// that does not exist has nothing to serialize and gets a lease with Seq 0.
	AcquireLease(ctx context.Context, proposalID, holder string, ttl time.Duration) (Lease, bool, error)
	// ReleaseLease ends a lease early. A lease that was taken over is left alone.
	ReleaseLease(context.Context, Lease) error
	// RecordCheck notes one more passive check of a pending attempt: the count
	// grows, the next check is due after delay, and stuck marks the attempt
	// (once) as stuck.
	RecordCheck(ctx context.Context, commandID string, delay time.Duration, stuck bool) error
	// MarkUnresolved stops the automatic checks of an attempt (C25).
	MarkUnresolved(ctx context.Context, commandID string) error
}

// Lease is a fenced right on one proposal. Seq grows with every new holder; a
// write that carries a Seq older than the stored one is refused (ErrLeaseLost).
type Lease struct {
	ProposalID string
	Holder     string
	Seq        int64
}

type leaseKey struct{}

// WithLease puts the lease token into the context that mutating store calls
// receive.
func WithLease(ctx context.Context, lease Lease) context.Context {
	return context.WithValue(ctx, leaseKey{}, lease)
}

// LeaseFromContext returns the lease token of ctx, if any.
func LeaseFromContext(ctx context.Context) (Lease, bool) {
	lease, ok := ctx.Value(leaseKey{}).(Lease)
	return lease, ok
}

// DueAttempt names a pending attempt whose passive check is due.
type DueAttempt struct {
	OrgID        string
	RestaurantID string
	CommandID    string
}

// AttemptPicker selects the work of the reconciler across restaurants. It
// moves the next check of each picked attempt forward by shift, so replicas do
// not pick the same rows together.
type AttemptPicker interface {
	PickDue(ctx context.Context, limit int, shift time.Duration) ([]DueAttempt, error)
}

// SweepResult counts what a retention pass deleted.
type SweepResult struct {
	Proposals        int
	Attempts         int
	ClosedRestaurant int
}

// Sweeper deletes rows past their retention (R9).
type Sweeper interface {
	Sweep(ctx context.Context, limit int) (SweepResult, error)
}
