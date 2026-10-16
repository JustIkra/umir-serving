// Package pgtest builds a temporary PostgreSQL database with the Serving schema
// and the two real roles of production (C8): the owner that migrates and the
// runtime role that is not the owner and has NOBYPASSRLS. Tests that touch the
// database use it; nothing outside tests imports it.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/JustIkra/umir-serving/internal/adapters/pgstore"
	"github.com/JustIkra/umir-serving/internal/adapters/postgres"
	"github.com/JustIkra/umir-serving/tests/itest"
)

// Env is one temporary database with migrated Serving schema.
type Env struct {
	AdminURL    string
	OwnerURL    string
	RuntimeURL  string
	OwnerRole   string
	RuntimeRole string
	password    string

	mu    sync.Mutex
	roles []string
}

func randHex(tb testing.TB) string {
	tb.Helper()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		tb.Fatalf("random: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// New creates the database, the roles and applies the migrations as the owner.
func New(tb testing.TB) *Env {
	tb.Helper()
	env := NewUnmigrated(tb)
	owner, err := postgres.Open(tb.Context(), postgres.Config{URL: env.OwnerURL, ApplicationName: "serving-test-migrate", MaxConns: 2})
	if err != nil {
		tb.Fatalf("open owner connection: %v", err)
	}
	defer owner.Close()
	if err := pgstore.Migrate(tb.Context(), owner); err != nil {
		tb.Fatalf("migrate: %v", err)
	}
	// The runtime role reads the generation and the schema version and never
	// writes them (reference doc.go); the default privileges gave it more.
	env.Admin(tb, fmt.Sprintf("REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON storage_meta, schema_migrations FROM %s", pgx.Identifier{env.RuntimeRole}.Sanitize()))
	return env
}

// NewUnmigrated creates the database and roles without applying migrations.
func NewUnmigrated(tb testing.TB) *Env {
	tb.Helper()
	ctx := tb.Context()
	e := &Env{password: randHex(tb) + randHex(tb)}
	base := "t_serving_" + randHex(tb)
	e.OwnerRole = base + "_owner"
	e.RuntimeRole = base + "_rt"
	cluster := itest.DatabaseURL(tb)
	conn, err := pgx.Connect(ctx, cluster)
	if err != nil {
		tb.Fatalf("connect to the cluster: %v", err)
	}
	defer closeConn(conn)
	e.roles = []string{e.OwnerRole, e.RuntimeRole}
	tb.Cleanup(func() { e.dropRoles(tb, cluster) })
	for _, role := range e.roles {
		e.createRole(tb, conn, role, "NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS")
	}
	e.AdminURL = itest.NewDatabase(tb)
	admin, err := pgx.Connect(ctx, e.AdminURL)
	if err != nil {
		tb.Fatalf("connect to the temporary database: %v", err)
	}
	defer closeConn(admin)
	owner, rt := pgx.Identifier{e.OwnerRole}.Sanitize(), pgx.Identifier{e.RuntimeRole}.Sanitize()
	dbName := pgx.Identifier{databaseName(tb, e.AdminURL)}.Sanitize()
	for _, stmt := range []string{
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s, %s", dbName, owner, rt),
		"REVOKE CREATE ON SCHEMA public FROM PUBLIC",
		fmt.Sprintf("GRANT USAGE, CREATE ON SCHEMA public TO %s", owner),
		fmt.Sprintf("GRANT USAGE ON SCHEMA public TO %s", rt),
		fmt.Sprintf("ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO %s", owner, rt),
		fmt.Sprintf("ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO %s", owner, rt),
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			tb.Fatalf("%s: %v", stmt, err)
		}
	}
	e.OwnerURL = withUser(tb, e.AdminURL, e.OwnerRole, e.password)
	e.RuntimeURL = withUser(tb, e.AdminURL, e.RuntimeRole, e.password)
	return e
}

// OpenRuntime opens a pool as the runtime role and closes it with the test.
func (e *Env) OpenRuntime(tb testing.TB, maxConns int32) *postgres.DB {
	tb.Helper()
	db, err := postgres.Open(tb.Context(), postgres.Config{URL: e.RuntimeURL, ApplicationName: "serving-test", MaxConns: maxConns})
	if err != nil {
		tb.Fatalf("open runtime connection: %v", err)
	}
	tb.Cleanup(db.Close)
	return db
}

// OpenOwner opens a pool as the owner role and closes it with the test.
func (e *Env) OpenOwner(tb testing.TB) *postgres.DB {
	tb.Helper()
	db, err := postgres.Open(tb.Context(), postgres.Config{URL: e.OwnerURL, ApplicationName: "serving-test-owner", MaxConns: 2})
	if err != nil {
		tb.Fatalf("open owner connection: %v", err)
	}
	tb.Cleanup(db.Close)
	return db
}

// Admin runs statements as the superuser of the temporary database.
func (e *Env) Admin(tb testing.TB, stmt string, args ...any) {
	tb.Helper()
	conn, err := pgx.Connect(tb.Context(), e.AdminURL)
	if err != nil {
		tb.Fatalf("connect as admin: %v", err)
	}
	defer closeConn(conn)
	if _, err := conn.Exec(tb.Context(), stmt, args...); err != nil {
		tb.Fatalf("admin statement: %v", err)
	}
}

func (e *Env) createRole(tb testing.TB, conn *pgx.Conn, role, attributes string) {
	tb.Helper()
	// The password is random hex produced above, so inlining it is safe.
	stmt := fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s' %s", pgx.Identifier{role}.Sanitize(), e.password, attributes)
	if _, err := conn.Exec(tb.Context(), stmt); err != nil {
		tb.Fatalf("create role %s: %v", role, err)
	}
}

func (e *Env) dropRoles(tb testing.TB, cluster string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, cluster)
	if err != nil {
		tb.Errorf("connect to drop roles: %v", err)
		return
	}
	defer closeConn(conn)
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, role := range e.roles {
		quoted := pgx.Identifier{role}.Sanitize()
		if _, err := conn.Exec(ctx, "DROP OWNED BY "+quoted); err != nil {
			tb.Errorf("drop owned by %s: %v", role, err)
		}
		if _, err := conn.Exec(ctx, "DROP ROLE IF EXISTS "+quoted); err != nil {
			tb.Errorf("drop role %s: %v", role, err)
		}
	}
}

func closeConn(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.Close(ctx); err != nil {
		return
	}
}

func withUser(tb testing.TB, raw, user, password string) string {
	tb.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		tb.Fatalf("parse database URL: %v", err)
	}
	u.User = url.UserPassword(user, password)
	return u.String()
}

func databaseName(tb testing.TB, raw string) string {
	tb.Helper()
	u, err := url.Parse(raw)
	if err != nil || len(u.Path) < 2 {
		tb.Fatalf("parse database URL: %v", err)
	}
	return u.Path[1:]
}
