package bootstrap

import (
	"log/slog"
	"time"

	"github.com/JustIkra/umir-serving/contract"
)

type Options struct {
	Now                      func() time.Time
	NewID                    func() string
	OrdersWarehouse          contract.OrdersWarehouse
	OrderAttemptReader       contract.OrderAttemptReader
	CatalogWarehouse         contract.CatalogWarehouse
	MenuWarehouse            contract.MenuWarehouse
	RecommendationsWarehouse contract.RecommendationWarehouse
	Logger                   *slog.Logger
}
