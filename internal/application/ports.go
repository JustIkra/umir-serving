package application

import (
	"context"

	"github.com/JustIkra/umir-serving/internal/domain"
)

type ProposalStore interface {
	Create(context.Context, domain.OrderProposal) error
	Get(context.Context, string) (domain.ProposalRecord, bool, error)
	MarkPending(context.Context, string, string) error
	ResolvePending(context.Context, string, string) error
	Replace(context.Context, string, string, domain.OrderProposal) (domain.OrderProposal, error)
}
