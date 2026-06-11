package grpc

import (
	"context"
	"fmt"
	"net"
	"os"

	"google.golang.org/grpc"

	gojira "github.com/neumachen/gojira"
	gojirav1 "github.com/neumachen/gojira/gen/gojira/v1"
)

// Serve boots the gojira gRPC server on cfg.ServerAddress and blocks
// until ctx is cancelled (clean shutdown) or the underlying
// grpc.Server.Serve call fails.
//
// cfg is assumed already validated by the caller's config cascade.
// cfg.ServerAddress is always populated by that cascade (it defaults
// to 127.0.0.1:50051); an empty value here means the config wiring
// failed to deliver it — a programmer error, not a user/runtime
// condition — so Serve panics rather than returning an error.
//
// Lifecycle:
//   - A clean signal-driven shutdown (ctx cancelled) returns nil after
//     GracefulStop drains in-flight RPCs.
//   - A non-nil error from grpc.Server.Serve is returned as-is.
//
// Diagnostics: a "listening on <bound-address>" line is emitted on
// os.Stderr when the listener is up, and a "stopped" line when Serve
// returns from a clean shutdown. The bound-address line uses the
// listener's actual local address (lis.Addr().String()), so an
// ephemeral port (":0") still reports the assigned port — process
// supervisors and tests rely on this stable line.
func Serve(ctx context.Context, cfg gojira.Config) error {
	if cfg.ServerAddress == "" {
		panic("internal/grpc: Serve called with empty cfg.ServerAddress; config cascade did not populate it")
	}

	lis, err := net.Listen("tcp", cfg.ServerAddress)
	if err != nil {
		return fmt.Errorf("grpc: listen on %s: %w", cfg.ServerAddress, err)
	}

	srv := grpc.NewServer()
	gojirav1.RegisterGojiraServer(srv, NewServer(cfg))

	// Report the *actually-bound* address, which differs from the
	// requested cfg.ServerAddress when the caller asks for an
	// ephemeral port (e.g. "127.0.0.1:0"). Tests and process
	// supervisors rely on this line being stable.
	fmt.Fprintf(os.Stderr, "gojira gRPC server listening on %s\n", lis.Addr().String())

	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- srv.Serve(lis) }()

	select {
	case <-ctx.Done():
		// Graceful stop drains in-flight RPCs; Serve then returns nil.
		srv.GracefulStop()
		<-serveErrCh
		fmt.Fprintln(os.Stderr, "gojira gRPC server stopped")
		return nil

	case err := <-serveErrCh:
		return err
	}
}
