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
	CommitPending   CommitResultKind = "Pending"
)

type OrderAttemptState string

const (
	OrderAttemptUnseen    OrderAttemptState = "Unseen"
	OrderAttemptPending   OrderAttemptState = "Pending"
	OrderAttemptCompleted OrderAttemptState = "Completed"
	OrderAttemptRejected  OrderAttemptState = "Rejected"
)

type OrderAttempt struct {
	State  OrderAttemptState
	Result *CommitResult
	Error  *AttemptError
}

type AttemptError struct {
	Code    string
	Message string
	Outcome string
}

type ConfirmationAttempt struct {
	CommandID         string
	ProposalID        string
	Fingerprint       string
	AcceptedPositions []AcceptedPosition
	State             OrderAttemptState
	Pending           bool
	Result            *ConfirmResult
	WarehouseResult   *CommitResult
	Replacement       *OrderProposal
	Error             *AttemptError
}

func (a ConfirmationAttempt) Clone() ConfirmationAttempt {
	a.AcceptedPositions = append([]AcceptedPosition(nil), a.AcceptedPositions...)
	if a.Result != nil {
		result := *a.Result
		if result.Order != nil {
			order := result.Order.Clone()
			result.Order = &order
		}
		if result.Proposal != nil {
			proposal := result.Proposal.Clone()
			result.Proposal = &proposal
		}
		a.Result = &result
	}
	if a.WarehouseResult != nil {
		result := a.WarehouseResult.Clone()
		a.WarehouseResult = &result
	}
	if a.Replacement != nil {
		proposal := a.Replacement.Clone()
		a.Replacement = &proposal
	}
	if a.Error != nil {
		value := *a.Error
		a.Error = &value
	}
	return a
}

func (result CommitResult) Clone() CommitResult {
	if result.Order != nil {
		order := result.Order.Clone()
		result.Order = &order
	}
	if result.OrderEvaluation != nil {
		evaluation := *result.OrderEvaluation
		evaluation.RequestedPositions = append([]Position(nil), evaluation.RequestedPositions...)
		evaluation.AcceptedPositions = append([]AcceptedPosition(nil), evaluation.AcceptedPositions...)
		evaluation.Changes = append([]Change(nil), evaluation.Changes...)
		for index := range evaluation.Changes {
			evaluation.Changes[index].Deficits = append([]Deficit(nil), evaluation.Changes[index].Deficits...)
		}
		result.OrderEvaluation = &evaluation
	}
	return result
}

func (order Order) Clone() Order {
	order.Positions = append([]AcceptedPosition(nil), order.Positions...)
	order.Allocations = append([]Allocation(nil), order.Allocations...)
	return order
}

func (proposal OrderProposal) Clone() OrderProposal {
	proposal.RequestedPositions = append([]Position(nil), proposal.RequestedPositions...)
	proposal.AcceptedPositions = append([]AcceptedPosition(nil), proposal.AcceptedPositions...)
	proposal.Changes = append([]Change(nil), proposal.Changes...)
	for index := range proposal.Changes {
		proposal.Changes[index].Deficits = append([]Deficit(nil), proposal.Changes[index].Deficits...)
	}
	return proposal
}

func (result ConfirmResult) Clone() ConfirmResult {
	if result.Order != nil {
		order := result.Order.Clone()
		result.Order = &order
	}
	if result.Proposal != nil {
		proposal := result.Proposal.Clone()
		result.Proposal = &proposal
	}
	return result
}

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
