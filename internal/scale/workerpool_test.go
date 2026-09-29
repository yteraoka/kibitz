package scale

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/option"
)

func fakeRunAPI(t *testing.T, body string) *CloudRunWorkerPool {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/workerPools/kibitz-worker") {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	pool, err := NewCloudRunWorkerPool(context.Background(), "p", "asia-northeast1", "kibitz-worker",
		option.WithEndpoint(srv.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

// The update time is the API's own, in the form it sends: RFC 3339 with
// fractional seconds.
func TestWorkerPoolLastChanged(t *testing.T) {
	pool := fakeRunAPI(t, `{"updateTime":"2026-09-29T15:04:44.123456Z","scaling":{"manualInstanceCount":1}}`)
	got, err := pool.LastChanged(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 9, 29, 15, 4, 44, 123456000, time.UTC); !got.Equal(want) {
		t.Errorf("LastChanged = %s, want %s", got, want)
	}
}

// A time that cannot be read is an error, which the scaler answers by leaving
// the worker where it is -- not a zero time, which would read as long ago.
func TestWorkerPoolLastChangedUnreadable(t *testing.T) {
	pool := fakeRunAPI(t, `{"scaling":{"manualInstanceCount":1}}`)
	if _, err := pool.LastChanged(context.Background()); err == nil {
		t.Error("a missing update time was not an error")
	}
}
