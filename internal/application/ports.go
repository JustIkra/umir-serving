package application

import (
	"context"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/domain"
)

type ProposalStore interface {
	Create(context.Context, domain.OrderProposal) error
	Get(context.Context, string) (domain.ProposalRecord, bool, error)
	MarkPending(context.Context, string, string) error
	ResolvePending(context.Context, string, string) error
	Replace(context.Context, string, string, domain.OrderProposal) (domain.OrderProposal, error)
}

type ConfirmationAttemptStore interface {
	BeginAttempt(context.Context, contract.ConfirmationAttempt) (contract.ConfirmationAttempt, bool, error)
	GetAttempt(context.Context, string) (contract.ConfirmationAttempt, bool, error)
	ReserveAttempt(context.Context, string) error
	RememberWarehouseResult(context.Context, string, contract.CommitResult, *contract.OrderProposal) error
	CompleteAttempt(context.Context, string, contract.ConfirmResult, *contract.AttemptError, *domain.OrderProposal) error
}
