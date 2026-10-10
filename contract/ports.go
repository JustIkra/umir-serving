package contract

import "context"

type OrdersWarehouse interface {
	EvaluateOrder(context.Context, EvaluateOrderRequest) (OrderEvaluation, error)
	CommitAcceptedOrder(context.Context, CommitOrderRequest) (CommitResult, error)
	FindRecordedCommand(context.Context, string) (CommandRecord, bool, error)
	FindOrderByProposal(context.Context, string) (Order, bool, error)
}

type OrderAttemptReader interface {
	FindOrderAttempt(context.Context, string, string) (OrderAttempt, error)
}

type CatalogWarehouse interface {
	GetProducts(context.Context) (Products, error)
	GetDishCatalog(context.Context) (DishCatalog, error)
	GetDish(context.Context, string) (DishDetails, error)
	RegisterProduct(context.Context, RegisterProductRequest) (Product, error)
	DefineDish(context.Context, DefineDishRequest) (Dish, error)
}

type MenuWarehouse interface {
	EvaluateMenu(context.Context) (MenuEvaluation, error)
}
