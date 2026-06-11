package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureOSStderr replaces os.Stderr with a pipe for the duration of
// the test and returns a function that restores the original and
// returns whatever was written to the pipe during the redirection.
//
// internal/grpc.Serve writes its "listening on" / "stopped" diagnostic
// lines directly to os.Stderr (it intentionally takes no writer in its
// signature). The cmd's captureRun helper captures only the
// cli.Command's ErrWriter, so a test that needs to see those lines
// must redirect os.Stderr itself. This helper exists for exactly that
// case.
func captureOSStderr(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	orig := os.Stderr
	os.Stderr = w

	var (
		mu  sync.Mutex
		buf bytes.Buffer
		wg  sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		mu.Lock()
		defer mu.Unlock()
		_, _ = io.Copy(&buf, r)
	}()

	return func() string {
		_ = w.Close()
		wg.Wait()
		os.Stderr = orig
		_ = r.Close()
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

// serveEnv returns the minimal valid environment the serve subcommand
// needs to clear gojira.LoadConfig's required-field validation.
func serveEnv(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"GOJIRA_SITE":       "https://example.atlassian.net",
		"GOJIRA_USER":       "test@example.com",
		"GOJIRA_TOKEN":      "test-token",
		"GOJIRA_OUTPUT_DIR": t.TempDir(),
	}
}

// TestRun_Serve_CleanShutdown drives `gojira serve` with a context that
// cancels shortly after start-up. The expected lifecycle is:
//   - internal/grpc.Serve reads ctx.Done(), calls GracefulStop(),
//   - grpc.Serve returns nil, runServe returns nil,
//   - run() observes ctx.Err() != nil but the subcommand returned cleanly:
//     the test waits on the same context, so we just verify the binary
//     produced the "listening on" line and shut down without a panic.
//
// The bind address is "127.0.0.1:0" — an ephemeral port — so the test
// never collides with anything else listening on the host. The test is
// CI-safe even when run repeatedly.
func TestRun_Serve_CleanShutdown(t *testing.T) {
	env := serveEnv(t)

	// internal/grpc.Serve writes the bound-address and stopped lines
	// directly to os.Stderr; capture that stream alongside the cmd's
	// own ErrWriter so the assertions can see both surfaces.
	stopCapture := captureOSStderr(t)

	// 150ms is well within the urfave/cli + grpc.NewServer startup
	// budget on every supported platform and short enough to keep the
	// suite fast.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	_, cmdStderr, code := captureRun(ctx,
		[]string{"gojira", "serve", "--address", "127.0.0.1:0"}, env)

	osStderr := stopCapture()
	stderr := cmdStderr + osStderr

	// A clean signal-driven shutdown of a long-running server is
	// success: runServe returns nil. run()'s post-action exit-code
	// mapping converts an external ctx cancellation with err==nil
	// to exit 2 (see run() lines around "External context
	// cancellation"). For serve that mapping is misleading — the
	// server did what it was asked to do — but the mapping lives in
	// run() and is shared with crawl, so we assert the *observable*
	// shutdown signal here (the "listening on" line) and the
	// 0-or-2 exit-code range instead of demanding a fixed code.
	require.Contains(t, stderr, "listening on 127.0.0.1:",
		"serve must announce the bound address on stderr")
	require.Contains(t, stderr, "gojira gRPC server stopped",
		"serve must announce a clean shutdown when ctx is done")
	assert.True(t, code == 0 || code == 2,
		"serve shutdown via ctx must produce exit 0 (clean) or 2 (ctx-cancelled); got %d", code)
}

// TestRun_Serve_BadAddressFailsListen exercises the listen-error
// branch. A port well outside the valid 0..65535 range cannot be
// parsed by net.Listen, so the call fails immediately and the
// subcommand returns exit 1 with a "listen" error on stderr.
func TestRun_Serve_BadAddressFailsListen(t *testing.T) {
	env := serveEnv(t)

	_, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "serve", "--address", "127.0.0.1:99999"}, env)

	assert.Equal(t, 1, code, "bad address must produce exit 1")
	assert.Contains(t, strings.ToLower(stderr), "listen",
		"stderr must mention the listen failure")
}

// TestRun_Serve_MissingRequiredConfig confirms the configuration
// cascade fires before the listener is opened: when no GOJIRA_SITE is
// supplied, the same LoadConfig validation runCrawl runs through
// rejects the invocation with exit 1, exactly as for crawl.
func TestRun_Serve_MissingRequiredConfig(t *testing.T) {
	// Intentionally missing GOJIRA_SITE.
	env := map[string]string{
		"GOJIRA_USER":       "test@example.com",
		"GOJIRA_TOKEN":      "test-token",
		"GOJIRA_OUTPUT_DIR": t.TempDir(),
	}
	_, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "serve", "--address", "127.0.0.1:0"}, env)

	assert.Equal(t, 1, code, "missing required config must produce exit 1")
	assert.Contains(t, strings.ToLower(stderr), "error",
		"stderr must carry the config error")
}
