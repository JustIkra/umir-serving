package contract

import "errors"

var (
	ErrInvalidArgument         = errors.New("invalid argument")
	ErrInventoryUnavailable    = errors.New("inventory unavailable")
	ErrCommandIdentityConflict = errors.New("command identity conflict")
	ErrConcurrentModification  = errors.New("concurrent modification")
	ErrProposalNotFound        = errors.New("proposal not found")
	ErrProposalSuperseded      = errors.New("proposal superseded")
	ErrConfirmationPending     = errors.New("confirmation pending")
	ErrDishNotFound            = errors.New("dish not found")
	ErrProductNotFound         = errors.New("product not found")
)
