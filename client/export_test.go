// export_test.go exposes unexported client internals for use in tests
// within the client_test package. This file is compiled only during
// testing (it lives in the client package, not client_test).
package client

import (
	"context"
	"time"
)

// WithSleepFnForTest returns an Option that replaces the internal sleep
// function. Used by tests to eliminate real waits without build tags.
func WithSleepFnForTest(fn func(context.Context, time.Duration) error) Option {
	return withSleepFn(fn)
}
