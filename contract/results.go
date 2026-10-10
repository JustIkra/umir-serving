package contract

type CommitResult struct {
	Kind            CommitResultKind
	Order           *Order
	OrderEvaluation *OrderEvaluation
}

type CommitResultKind string

const (
	OrderCommitted  CommitResultKind = "OrderCommitted"
	RecheckRequired CommitResultKind = "RecheckRequired"
)

type ConfirmResult struct {
	Kind         ConfirmResultKind
	Order        *Order
	Proposal     *OrderProposal
	SupersededBy string
}

type ConfirmResultKind string

const (
	OrderConfirmed       ConfirmResultKind = "OrderConfirmed"
	RequiresConfirmation ConfirmResultKind = "RequiresConfirmation"
	EmptyOrder           ConfirmResultKind = "EmptyOrder"
	ProposalSuperseded   ConfirmResultKind = "ProposalSuperseded"
	ConfirmationPending  ConfirmResultKind = "ConfirmationPending"
)
