package contract

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

type Product struct {
	ProductID string
	Name      string
	BaseUnit  string
}

type Ingredient struct {
	ProductID string
	Quantity  int64
	BaseUnit  string
}

type Dish struct {
	DishID string
	Name   string
	Recipe []Ingredient
}

type DishDetails struct {
	EvaluatedAt  time.Time
	StateVersion int64
	Dish         Dish
}

type ShelfLife struct {
	Unit  string
	Value int64
}

type DeliveryLine struct {
	LineNo         int
	ProductID      string
	ManufacturedAt time.Time
	ShelfLife      ShelfLife
	ReceivedAt     time.Time
	Quantity       int64
}

type ReceiveDeliveryRequest struct {
	CommandID  string
	DeliveryID string
	Number     string
	CreatedAt  time.Time
	Lines      []DeliveryLine
}

type DeliveryReceipt struct {
	DeliveryID string
	BatchIDs   []string
}

type Delivery struct {
	DeliveryID string
	Number     string
	CreatedAt  time.Time
	Lines      []DeliveryLine
	BatchIDs   []string
}

type Batch struct {
	BatchID           string
	DeliveryID        string
	LineNo            int
	ProductID         string
	ManufacturedAt    time.Time
	ShelfLife         ShelfLife
	ReceivedAt        time.Time
	ExpiresAt         time.Time
	ReceivedQuantity  int64
	RemainingQuantity int64
}

type Allocation struct {
	ProductID string
	BatchID   string
	Quantity  int64
}

type MenuAllocation struct {
	ProductID   string
	ProductName string
	BatchID     string
	Quantity    int64
	BaseUnit    string
	ExpiresAt   time.Time
}

type Order struct {
	OrderID     string
	ProposalID  string
	Positions   []AcceptedPosition
	Allocations []Allocation
	PlacedAt    time.Time
}

type OrderEvaluation struct {
	EvaluatedAt        time.Time
	StateVersion       int64
	RequestedPositions []Position
	AcceptedPositions  []AcceptedPosition
	Changes            []Change
}

type MenuEvaluation struct {
	EvaluatedAt  time.Time
	StateVersion int64
	Dishes       []DishEvaluation
}

type DishEvaluation struct {
	DishID      string
	DishName    string
	IsAvailable bool
	Deficits    []Deficit
	Allocations []MenuAllocation
}

type OrderProposal struct {
	ProposalID         string
	EvaluatedAt        time.Time
	StateVersion       int64
	RequestedPositions []Position
	AcceptedPositions  []AcceptedPosition
	Changes            []Change
}

type EvaluateOrderRequest struct {
	Positions []Position
}

type CommitOrderRequest struct {
	CommandID         string
	ProposalID        string
	AcceptedPositions []AcceptedPosition
}

type CommandRecord struct {
	CommandID          string
	CommandType        string
	CommandFingerprint string
	Result             CommandResult
}

type CommandResult struct {
	Kind            CommandResultKind
	Product         *Product
	Dish            *Dish
	DeliveryReceipt *DeliveryReceipt
	Order           *Order
	ExpiryResult    *ExpiryResult
}

type CommandResultKind string

const (
	ProductRegistered CommandResultKind = "ProductRegistered"
	DishDefined       CommandResultKind = "DishDefined"
	DeliveryReceived  CommandResultKind = "DeliveryReceived"
	OrderPlaced       CommandResultKind = "OrderPlaced"
	BatchExpired      CommandResultKind = "BatchExpired"
)

type ExpiryResult struct {
	BatchID            string
	ProductID          string
	WrittenOffQuantity int64
	Reason             string
	ExpiresAt          time.Time
	ProcessedAt        time.Time
}

type Menu struct {
	EvaluatedAt  time.Time
	StateVersion int64
	Dishes       []MenuDish
}

type MenuDish struct {
	DishID string
	Name   string
}

type StopList struct {
	EvaluatedAt  time.Time
	StateVersion int64
	Dishes       []StopListDish
}

type StopListDish struct {
	DishID   string
	Name     string
	Deficits []Deficit
}

type Recommendations struct {
	EvaluatedAt  time.Time
	StateVersion int64
	Items        []Recommendation
}

type Recommendation struct {
	DishID            string
	DishName          string
	PriorityExpiresAt time.Time
	Reasons           []RecommendationReason
}

type RecommendationReason struct {
	ProductID          string
	ProductName        string
	BatchID            string
	QuantityPerPortion int64
	BaseUnit           string
	ExpiresAt          time.Time
}

type InventoryView struct {
	EvaluatedAt  time.Time
	StateVersion int64
	Products     []ProductInventory
}

type ProductInventory struct {
	ProductID string
	Name      string
	BaseUnit  string
	Quantity  int64
	Batches   []BatchInventory
}

type BatchInventory struct {
	BatchID           string
	ManufacturedAt    time.Time
	ReceivedAt        time.Time
	ExpiresAt         time.Time
	RemainingQuantity int64
	IsExpired         bool
}

type ExpiringBatch struct {
	BatchID           string
	ProductID         string
	ProductName       string
	BaseUnit          string
	RemainingQuantity int64
	ManufacturedAt    time.Time
	ReceivedAt        time.Time
	ExpiresAt         time.Time
}

type History struct {
	Entries []HistoryEntry
}

type HistoryEntry struct {
	EventID            string
	StreamID           string
	Sequence           int64
	SchemaVersion      int64
	CommandID          string
	CommandFingerprint string
	RecordedAt         time.Time
	Type               string
	Payload            EventPayload
}

type EventPayload struct {
	Product         *Product
	Dish            *Dish
	Delivery        *Delivery
	Batches         []Batch
	DeliveryReceipt *DeliveryReceipt
	Order           *Order
	ExpiryResult    *ExpiryResult
}

type RegisterProductRequest struct {
	CommandID string
	ProductID string
	Name      string
	BaseUnit  string
}

type DefineDishRequest struct {
	CommandID string
	DishID    string
	Name      string
	Recipe    []Ingredient
}

type PrepareOrderRequest struct {
	Positions []Position
}

type ConfirmOrderRequest struct {
	CommandID  string
	ProposalID string
}

type ExpireBatchRequest struct {
	CommandID string
	BatchID   string
}

type Products struct {
	EvaluatedAt  time.Time
	StateVersion int64
	Items        []Product
}

type DishCatalog struct {
	EvaluatedAt  time.Time
	StateVersion int64
	Items        []Dish
}
