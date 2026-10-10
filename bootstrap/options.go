package bootstrap

import (
	"time"

	"github.com/JustIkra/umir-serving/contract"
)

type Options struct {
	Now              func() time.Time
	NewID            func() string
	OrdersWarehouse  contract.OrdersWarehouse
	CatalogWarehouse contract.CatalogWarehouse
	MenuWarehouse    contract.MenuWarehouse
}
