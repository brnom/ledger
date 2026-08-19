package pgstore_test

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/brnom/ledger/adapter/driven/pgstore"
	"github.com/brnom/ledger/adapter/driven/storagetest"
	"github.com/brnom/ledger/app"
	"github.com/brnom/ledger/domain"
)

// DSNEnv names the environment variable that points these tests at a
// PostgreSQL instance. Without it they skip, so `go test ./...` stays useful
// on a machine with no database.
const DSNEnv = "LEDGER_TEST_POSTGRES_DSN"

var (
	once sync.Once
	pool *pgxpool.Pool
	fail error
)

// sharedPool connects and migrates once for the whole package. Tests isolate
// themselves by ledger id rather than by schema, which is also how a real
// deployment separates books.
func sharedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv(DSNEnv)
	if dsn == "" {
		t.Skipf("set %s to run the PostgreSQL tests", DSNEnv)
	}
	once.Do(func() {
		ctx := context.Background()
		opened, err := pgxpool.New(ctx, dsn)
		if err != nil {
			fail = err
			return
		}
		if err := opened.Ping(ctx); err != nil {
			fail = err
			return
		}
		if err := pgstore.New(opened).Migrate(ctx); err != nil {
			fail = err
			return
		}
		pool = opened
	})
	if fail != nil {
		t.Fatalf("connecting to PostgreSQL: %v", fail)
	}
	return pool
}

func TestConformance(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) app.Store {
		return pgstore.New(sharedPool(t))
	})
}

// TestMigrateIsIdempotent covers the ordinary case of a service restarting:
// migrations run on every start and must be a no-op once applied.
func TestMigrateIsIdempotent(t *testing.T) {
	store := pgstore.New(sharedPool(t))
	for i := 0; i < 3; i++ {
		if err := store.Migrate(context.Background()); err != nil {
			t.Fatalf("Migrate run %d: %v", i+1, err)
		}
	}
}

// TestPayloadSurvivesRoundTrip is the reason event payloads are stored in a
// json column rather than jsonb. jsonb normalizes the document: it reorders
// keys and rewrites numbers. The event hash covers the exact bytes. Payloads
// stored as jsonb would therefore break the chain silently on read-back.
func TestPayloadSurvivesRoundTrip(t *testing.T) {
	store := pgstore.New(sharedPool(t))
	ctx := context.Background()
	ledger, err := app.Open(store, "payload-roundtrip-"+domain.NewID().String())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	brl := domain.MustCurrency("BRL")
	for _, name := range []domain.AccountName{"equity:opening-balances", "liabilities:users:1"} {
		if _, _, err := ledger.OpenAccount(ctx, app.OpenAccountCommand{
			Name: name, Currency: brl, Normal: domain.Credit,
			AllowNegative: name == "equity:opening-balances",
			// Keys that would be reordered by jsonb, and a value with characters JSON
			// has to escape.
			Metadata: map[string]string{
				"zeta": "last", "alpha": "first", "mid": `quote" tab	 ação`,
			},
		}); err != nil {
			t.Fatalf("OpenAccount(%q): %v", name, err)
		}
	}
	if _, err := ledger.Commit(ctx, app.CommitCommand{
		Reference: "ação-123",
		Metadata:  map[string]string{"zzz": "1", "aaa": "2"},
		Postings: []domain.Posting{
			domain.Dr("equity:opening-balances", domain.FromMinor(brl, 12345)),
			domain.Cr("liabilities:users:1", domain.FromMinor(brl, 12345)),
		},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	events, err := ledger.Events(ctx, 1, 100)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("read %d events, want 3", len(events))
	}
	for _, event := range events {
		if err := event.Verify(); err != nil {
			t.Errorf("event %d: %v", event.Seq, err)
		}
		// Re-encoding the decoded payload must reproduce the stored bytes.
		if _, err := domain.Project(event); err != nil {
			t.Errorf("Project(%d): %v", event.Seq, err)
		}
	}
	if _, err := ledger.Verify(ctx); err != nil {
		t.Errorf("Verify: %v", err)
	}
}
