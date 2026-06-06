package fetch_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/neumachen/gojira/client"
	"github.com/neumachen/gojira/internal/config"
	"github.com/neumachen/gojira/internal/fetch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// compile-time assertion: *fetch.ClientFetcher satisfies fetch.Fetcher.
var _ fetch.Fetcher = (*fetch.ClientFetcher)(nil)

// noSleep eliminates real waits in retry paths so tests stay fast.
func noSleep(_ context.Context, _ time.Duration) error { return nil }

// newFetcher builds a ClientFetcher pointed at srv with tiny backoffs
// and no real sleeping.
func newFetcher(t *testing.T, srv *httptest.Server) *fetch.ClientFetcher {
	t.Helper()
	cfg := config.Config{
		Site:  srv.URL,
		User:  "user@example.com",
		Token: "api-token",
	}
	f, err := fetch.NewFromConfig(cfg,
		client.WithHTTPClient(srv.Client()),
		client.WithRateLimitBackoff(time.Millisecond, 5*time.Millisecond),
		client.WithNetworkBackoff(time.Millisecond, 5*time.Millisecond),
		client.WithMaxRetries(0), // no 429 retries — keeps tests instant
	)
	require.NoError(t, err, "NewFromConfig")
	return f
}

// TestInterfaceSatisfied is a compile-time guard; the var _ line above
// is the real check, but this test documents the intent explicitly.
func TestInterfaceSatisfied(t *testing.T) {
	// If *ClientFetcher does not implement Fetcher the package will not compile.
	var _ fetch.Fetcher = (*fetch.ClientFetcher)(nil)
}

// TestFetch_Success verifies that a 200 response returns the raw body bytes.
func TestFetch_Success(t *testing.T) {
	const wantBody = `{"key":"PROJ-1","fields":{}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, wantBody)
	}))
	defer srv.Close()

	f := newFetcher(t, srv)
	got, err := f.Fetch(context.Background(), "PROJ-1")
	require.NoError(t, err)
	assert.Equal(t, wantBody, string(got), "body")
}

// TestFetch_Unauthorized verifies that a 401 response propagates
// client.ErrUnauthorized so the crawl can abort.
func TestFetch_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	f := newFetcher(t, srv)
	_, err := f.Fetch(context.Background(), "PROJ-1")
	require.Error(t, err)
	assert.ErrorIs(t, err, client.ErrUnauthorized)
}

// TestFetch_Forbidden verifies that a 403 response propagates
// client.ErrForbidden so the crawl can render a permission-denied stub.
func TestFetch_Forbidden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	f := newFetcher(t, srv)
	_, err := f.Fetch(context.Background(), "PROJ-2")
	require.Error(t, err)
	assert.ErrorIs(t, err, client.ErrForbidden)
}

// TestFetch_NotFound verifies that a 404 response propagates
// client.ErrNotFound so the crawl can render a not-found stub.
func TestFetch_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	f := newFetcher(t, srv)
	_, err := f.Fetch(context.Background(), "PROJ-3")
	require.Error(t, err)
	assert.ErrorIs(t, err, client.ErrNotFound)
}

// TestFetch_ContextCancellation verifies that cancelling the context before
// the server responds causes Fetch to return an error containing
// context.Canceled.
func TestFetch_ContextCancellation(t *testing.T) {
	// The server blocks until the test is done, simulating a slow response.
	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-unblock:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer func() {
		close(unblock)
		srv.Close()
	}()

	cfg := config.Config{
		Site:  srv.URL,
		User:  "user@example.com",
		Token: "api-token",
	}
	f, err := fetch.NewFromConfig(cfg, client.WithHTTPClient(srv.Client()))
	require.NoError(t, err, "NewFromConfig")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately before the request completes

	_, fetchErr := f.Fetch(ctx, "PROJ-4")
	require.Error(t, fetchErr, "expected error after context cancellation")
	assert.ErrorIs(t, fetchErr, context.Canceled)
}

// TestNewFromConfig_InvalidSite verifies that NewFromConfig returns an error
// (without panicking) when the config contains an empty/invalid Site.
func TestNewFromConfig_InvalidSite(t *testing.T) {
	cfg := config.Config{
		Site:  "", // invalid: empty
		User:  "user@example.com",
		Token: "api-token",
	}
	f, err := fetch.NewFromConfig(cfg)
	require.Error(t, err, "expected error for empty Site")
	assert.Nil(t, f, "expected nil *ClientFetcher on error")
}
