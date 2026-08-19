package memstore_test

import (
	"testing"

	"github.com/brnom/ledger/adapter/driven/memstore"
	"github.com/brnom/ledger/adapter/driven/storagetest"
	"github.com/brnom/ledger/app"
)

func TestConformance(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) app.Store {
		store := memstore.New()
		t.Cleanup(func() { store.Close() })
		return store
	})
}
