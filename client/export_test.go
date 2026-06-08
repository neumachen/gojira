// export_test.go exposes unexported client internals for use in tests
// within the client_test package. This file is compiled only during
// testing (it lives in the client package, not client_test).
package client

import (
	"context"
	"net/http"
	"time"
)

// WithSleepFnForTest returns an Option that replaces the internal sleep
// function. Used by tests to eliminate real waits without build tags.
func WithSleepFnForTest(fn func(context.Context, time.Duration) error) Option {
	return withSleepFn(fn)
}

// DoPutForTest drives newPutJSON through doWithRetry so the external
// _test package can exercise the PUT transport path and the new 400/409
// status mappings without having to wait for the real write methods
// (CreateIssue/UpdateIssue/etc.) to land in later Phase 2 tasks. It is
// intentionally tiny: a single Client method visible only at test time.
func (c *Client) DoPutForTest(ctx context.Context, rawURL string, body []byte) ([]byte, error) {
	return c.doWithRetry(ctx, func() (*http.Request, error) {
		return c.newPutJSON(ctx, rawURL, body)
	})
}
