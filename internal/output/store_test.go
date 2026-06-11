package output_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

		if err := s.Write(ctx, "PROJ-1", "# index", "## outbound"); err != nil {
			t.Fatalf("unexpected error on first Write: %v", err)
		}
		got, ok := s.written["PROJ-1"]
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

		if err := s.Write(ctx, "PROJ-2", "# index", ""); err != nil {
			t.Fatalf("unexpected error on first Write: %v", err)
		}
		err := s.Write(ctx, "PROJ-2", "# index again", "")
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

		if err := s.Write(ctx, "PROJ-3", "# index", ""); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got := s.written["PROJ-3"]
		if got.outboundMD != "" {
			t.Errorf("outboundMD: got %q, want empty string", got.outboundMD)
		}
	})
}

// TestFSStore verifies FSStore, the filesystem-backed Store implementation.
func TestFSStore(t *testing.T) {
	// Compile-time assertion: FSStore must implement output.Store.
	var _ output.Store = (*output.FSStore)(nil)

	ctx := context.Background()

	tests := []struct {
		name       string
		key        string
		indexMD    string
		outboundMD string
		refetch    bool
		seedFirst  bool // write once with refetch=false before the test Write
		wantErr    error
	}{
		{
			name:       "writes index and outbound",
			key:        "PROJ-10",
			indexMD:    "# PROJ-10",
			outboundMD: "## outbound",
			refetch:    false,
			seedFirst:  false,
			wantErr:    nil,
		},
		{
			name:       "writes index only when outboundMD empty",
			key:        "PROJ-11",
			indexMD:    "# PROJ-11",
			outboundMD: "",
			refetch:    false,
			seedFirst:  false,
			wantErr:    nil,
		},
		{
			name:       "returns ErrAlreadyExists when index exists and refetch false",
			key:        "PROJ-12",
			indexMD:    "# PROJ-12 new",
			outboundMD: "",
			refetch:    false,
			seedFirst:  true,
			wantErr:    output.ErrAlreadyExists,
		},
		{
			name:       "refetch=true overwrites existing files",
			key:        "PROJ-13",
			indexMD:    "# PROJ-13 updated",
			outboundMD: "## updated outbound",
			refetch:    true,
			seedFirst:  true,
			wantErr:    nil,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()

			if tc.seedFirst {
				seed := output.NewFSStore(dir, false)
				if err := seed.Write(ctx, tc.key, "# original", "## original outbound"); err != nil {
					t.Fatalf("seed Write failed: %v", err)
				}
			}

			s := output.NewFSStore(dir, tc.refetch)
			err := s.Write(ctx, tc.key, tc.indexMD, tc.outboundMD)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Write error: got %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected Write error: %v", err)
			}

			// Verify index.md on disk.
			indexPath := filepath.Join(dir, tc.key, "index.md")
			gotIndex, err := os.ReadFile(indexPath)
			if err != nil {
				t.Fatalf("read index.md: %v", err)
			}
			if string(gotIndex) != tc.indexMD {
				t.Errorf("index.md: got %q, want %q", string(gotIndex), tc.indexMD)
			}

			// Verify references/outbound.md on disk.
			outboundPath := filepath.Join(dir, tc.key, "references", "outbound.md")
			if tc.outboundMD != "" {
				gotOutbound, err := os.ReadFile(outboundPath)
				if err != nil {
					t.Fatalf("read references/outbound.md: %v", err)
				}
				if string(gotOutbound) != tc.outboundMD {
					t.Errorf("references/outbound.md: got %q, want %q", string(gotOutbound), tc.outboundMD)
				}
			} else {
				if _, err := os.Stat(outboundPath); err == nil {
					t.Error("references/outbound.md should not exist when outboundMD is empty")
				}
			}
		})
	}
}
