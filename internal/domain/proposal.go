package domain

import "time"

type Position struct {
	LineNo   int
	DishID   string
	Portions int64
}

type AcceptedPosition struct {
	LineNo   int
	DishID   string
	Portions int64
}

type Deficit struct {
	ProductID         string
	RequiredQuantity  int64
	AvailableQuantity int64
}

type Change struct {
	LineNo            int
	DishID            string
	RequestedPortions int64
	AcceptedPortions  int64
	Deficits          []Deficit
}

type ProposalContext struct {
	OrgID           string
	TenantID        string
	Epoch           string
	TimeZone        string
	SettingsVersion string
	EvaluatedAt     time.Time
	StateVersion    int64
}

type OrderProposal struct {
	Context            ProposalContext
	ProposalID         string
	EvaluatedAt        time.Time
	StateVersion       int64
	RequestedPositions []Position
	AcceptedPositions  []AcceptedPosition
	Changes            []Change
}

type ProposalRecord struct {
	Proposal         OrderProposal
	PendingCommandID string
	SupersededBy     string
}

func (p OrderProposal) Clone() OrderProposal {
	p.RequestedPositions = append([]Position(nil), p.RequestedPositions...)
	p.AcceptedPositions = append([]AcceptedPosition(nil), p.AcceptedPositions...)
	p.Changes = append([]Change(nil), p.Changes...)
	for i := range p.Changes {
		p.Changes[i].Deficits = append([]Deficit(nil), p.Changes[i].Deficits...)
	}
	return p
}
