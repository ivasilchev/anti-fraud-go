package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/devastion/anti-fraud-go/internal/store"
	"github.com/devastion/anti-fraud-go/internal/store/sqlite"
	"github.com/devastion/anti-fraud-go/internal/store/storetest"
)

func TestStore(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		s, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "events.db"))
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}
