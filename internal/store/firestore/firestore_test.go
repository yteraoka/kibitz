package firestore_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/yteraoka/kibitz/internal/store"
	kfirestore "github.com/yteraoka/kibitz/internal/store/firestore"
	"github.com/yteraoka/kibitz/internal/store/storetest"
)

// These tests need the Firestore emulator, which CI starts and `make
// up-firestore` starts locally:
//
//	FIRESTORE_EMULATOR_HOST=localhost:8086 go test ./internal/store/firestore/...
const projectID = "kibitz-test"

func requireEmulator(t *testing.T) {
	t.Helper()
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("FIRESTORE_EMULATOR_HOST is not set; skipping the Firestore test")
	}
}

func TestConformance(t *testing.T) {
	requireEmulator(t)

	storetest.Run(t, func(t *testing.T) store.Store {
		t.Helper()

		ctx := context.Background()
		s, err := kfirestore.New(ctx, kfirestore.Config{
			ProjectID: projectID,
			// A collection per run keeps tests independent inside one
			// emulator, which has no way to drop a database.
			Collection: fmt.Sprintf("kibitz-test-%d", time.Now().UnixNano()),
		})
		if err != nil {
			t.Fatalf("connecting to the emulator: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

func TestConfigValidation(t *testing.T) {
	if _, err := kfirestore.New(context.Background(), kfirestore.Config{}); err == nil {
		t.Error("New accepted a config with no project id")
	}
}
