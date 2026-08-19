// Command ledgerd serves a ledger over HTTP.
//
// With no database configured it runs entirely in memory, which makes it
// usable for a demo or a test run without any setup. Point it at PostgreSQL
// with -dsn (or LEDGER_DSN) for a ledger that outlives the process.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/brnom/ledger/adapter/driven/memstore"
	"github.com/brnom/ledger/adapter/driven/pgstore"
	"github.com/brnom/ledger/adapter/driving/httpapi"
	"github.com/brnom/ledger/app"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "ledgerd: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr     = flag.String("addr", envOr("LEDGER_ADDR", ":8080"), "address to listen on")
		dsn      = flag.String("dsn", os.Getenv("LEDGER_DSN"), "PostgreSQL DSN; empty runs in memory")
		ledgerID = flag.String("ledger", envOr("LEDGER_ID", "main"), "ledger to serve")
		migrate  = flag.Bool("migrate", true, "apply pending migrations on start")
		backdate = flag.Duration("backdate-limit", app.DefaultBackdateLimit, "how far back an entry may be dated")
		future   = flag.Duration("future-limit", app.DefaultFutureLimit, "how far ahead an entry may be dated")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Signals cancel the root context, which unwinds startup and shutdown
	// through the same path.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := openStore(ctx, *dsn, *migrate, log)
	if err != nil {
		return err
	}
	defer store.Close()

	ledger, err := app.Open(store, *ledgerID,
		app.WithBackdateLimit(*backdate),
		app.WithFutureLimit(*future),
	)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           httpapi.New(ledger, log),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", slog.String("addr", *addr), slog.String("ledger", *ledgerID))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	// Drain in flight requests before closing the store underneath them.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func openStore(ctx context.Context, dsn string, migrate bool, log *slog.Logger) (app.Store, error) {
	if dsn == "" {
		log.Warn("no -dsn given. The ledger runs in memory. Nothing is saved.")
		return memstore.New(), nil
	}
	store, err := pgstore.Open(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if migrate {
		if err := store.Migrate(ctx); err != nil {
			store.Close()
			return nil, err
		}
		log.Info("migrations up to date")
	}
	return store, nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
