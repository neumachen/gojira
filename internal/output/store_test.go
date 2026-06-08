package output_test

import (
	"context"
	"errors"
	"testing"

	"github.com/neumachen/gojira/internal/output"
)

// fakeStore is a minimal in-memory implementation of output.Store used
// to verify that the interface shape is correct and that ErrAlreadyExists
// can be returned and detected via errors.Is.
type fakeStore struct {
	written map[string]fakeEntry
}

type fakeEntry struct {
	indexMD    string
	outboundMD string
}

func (f *fakeStore) Write(_ context.Context, key, indexMD, outboundMD string) error {
	if _, exists := f.written[key]; exists {
		return output.ErrAlreadyExists
	}
	if f.written == nil {
		f.written = make(map[string]fakeEntry)
	}
	f.written[key] = fakeEntry{indexMD: indexMD, outboundMD: outboundMD}
	return nil
}

// TestStoreInterfaceShape verifies that fakeStore satisfies output.Store
// at compile time and that the Write method behaves as documented.
func TestStoreInterfaceShape(t *testing.T) {
	// Compile-time assertion: fakeStore must implement output.Store.
	var _ output.Store = (*fakeStore)(nil)

	t.Run("write stores entry", func(t *testing.T) {
		s := &fakeStore{}
		ctx := context.Background()

		if err := s.Write(ctx, "PLATENG-1", "# index", "## outbound"); err != nil {
			t.Fatalf("unexpected error on first Write: %v", err)
		}
		got, ok := s.written["PLATENG-1"]
		if !ok {
			t.Fatal("entry not stored after Write")
		}
		if got.indexMD != "# index" {
			t.Errorf("indexMD: got %q, want %q", got.indexMD, "# index")
		}
		if got.outboundMD != "## outbound" {
			t.Errorf("outboundMD: got %q, want %q", got.outboundMD, "## outbound")
		}
	})

	t.Run("second write returns ErrAlreadyExists", func(t *testing.T) {
		s := &fakeStore{}
		ctx := context.Background()

		if err := s.Write(ctx, "PLATENG-2", "# index", ""); err != nil {
			t.Fatalf("unexpected error on first Write: %v", err)
		}
		err := s.Write(ctx, "PLATENG-2", "# index again", "")
		if err == nil {
			t.Fatal("expected ErrAlreadyExists on second Write, got nil")
		}
		if !errors.Is(err, output.ErrAlreadyExists) {
			t.Errorf("expected errors.Is(err, ErrAlreadyExists) to be true; got %v", err)
		}
	})

	t.Run("empty outboundMD is accepted", func(t *testing.T) {
		s := &fakeStore{}
		ctx := context.Background()

		if err := s.Write(ctx, "PLATENG-3", "# index", ""); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got := s.written["PLATENG-3"]
		if got.outboundMD != "" {
			t.Errorf("outboundMD: got %q, want empty string", got.outboundMD)
		}
	})
}
