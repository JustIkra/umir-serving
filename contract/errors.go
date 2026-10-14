package contract

import "errors"

type OperationError struct {
	Retryable   bool
	Preparation *WarehousePreparation
	Code        string
	Message     string
	Outcome     string
	Cause       error
}

func (e *OperationError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return e.Code
}

func (e *OperationError) Unwrap() error { return e.Cause }

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
