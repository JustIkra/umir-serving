// Package pgstore keeps Serving proposals, confirmation attempts and
// restaurant registrations in PostgreSQL. It builds on the copied reference
// adapter internal/adapters/postgres (R15) and adds only the Serving schema and
// the Store.
package pgstore

import (
	"context"
	"embed"
	"fmt"

	"github.com/JustIkra/umir-serving/internal/adapters/postgres"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// ServiceName names the migration lock and the schema of this service.
const ServiceName = "serving"

// MigrationSpec describes the embedded migrations.
func MigrationSpec() postgres.Spec {
	return postgres.Spec{Service: ServiceName, FS: migrationFS, Dir: "migrations"}
}

// Migrate applies the migrations. Only the migrate job calls it, under the
// owner role; serve never does (C8).
func Migrate(ctx context.Context, db *postgres.DB) error {
	if err := postgres.Migrate(ctx, db, MigrationSpec()); err != nil {
		return fmt.Errorf("migrate serving schema: %w", err)
	}
	return nil
}

// VerifySchema is the runtime check that the database is at this build's version.
func VerifySchema(ctx context.Context, db *postgres.DB) error {
	return postgres.VerifySchema(ctx, db, MigrationSpec())
}
