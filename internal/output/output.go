// Package output is the filesystem writer for gojira. It owns the
// per-issue directory layout, idempotency rules (skip-if-exists vs.
// refetch), and atomic write semantics. It knows nothing about Jira,
// ADF, Markdown content, or any other project-internal concern.
//
// # API design choice
//
// Write accepts plain parameters rather than a *config.Config so that
// callers (primarily internal/crawl) can pass only the two fields they
// need (OutputDir and Refetch) without importing the full config type
// into every test. The config package is still the canonical source of
// those values; the caller extracts them.
//
// # Atomic write strategy
//
// Each file is written to a temporary file in the same directory as the
// destination (e.g. index.md.tmp.<pid>), then renamed into place with
// os.Rename. Because source and destination are on the same filesystem,
// the rename is atomic on POSIX systems, so a crash mid-write cannot
// leave a half-written file at the canonical path.
//
// # references/ directory
//
// The references/ subdirectory is always created (0755) even when
// outboundMD is empty. This makes the directory structure predictable
// for downstream tools. The outbound.md file itself is only written
// when outboundMD is non-empty.
//
// # Key validation
//
// Jira issue keys match [A-Z][A-Z0-9]+-[0-9]+, which is
// filesystem-safe. This package rejects empty keys and keys that
// contain a path separator (/ or \) to prevent path-traversal attacks.
// It does not enforce the full Jira key regex — that is the caller's
// responsibility.
package output

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/neumachen/errext"
)

// ErrAlreadyExists is returned by Write when the destination
// index.md already exists on disk and refetch is false. Callers can
// test for this with errors.Is.
var ErrAlreadyExists = errors.New("output: issue already exists on disk")

// Write creates the per-issue directory tree under outputDir and writes
// the rendered Markdown files for a single Jira issue.
//
// Parameters:
//   - outputDir: root output directory (e.g. "/tmp/out").
//   - key: Jira issue key (e.g. "PLATENG-1147"). Must be non-empty and
//     must not contain a path separator.
//   - indexMD: content for <outputDir>/<key>/index.md.
//   - outboundMD: content for <outputDir>/<key>/references/outbound.md.
//     If empty, the file is not created (but the references/ directory
//     is still created).
//   - refetch: when false and index.md already exists, Write returns
//     ErrAlreadyExists without touching any file. When true, existing
//     files are overwritten.
//
// Directory permissions: 0755. File permissions: 0644.
// Writes are atomic: a temp file is written then renamed into place.
func Write(outputDir, key, indexMD, outboundMD string, refetch bool) error {
	if err := validateKey(key); err != nil {
		return err
	}

	issueDir := IssueDir(outputDir, key)
	refsDir := filepath.Join(issueDir, "references")
	indexPath := filepath.Join(issueDir, "index.md")

	// Skip-if-exists check: stat before creating directories so we
	// don't create empty directories for issues we are going to skip.
	if !refetch {
		if _, err := os.Stat(indexPath); err == nil {
			// File exists and refetch is off — return sentinel.
			return ErrAlreadyExists
		}
	}

	// Create directory tree.
	if err := os.MkdirAll(refsDir, 0755); err != nil {
		return errext.Errorf("output: create directories for %s: %w", key, err)
	}

	// Write index.md atomically.
	if err := atomicWrite(indexPath, indexMD, 0644); err != nil {
		return errext.Errorf("output: write index.md for %s: %w", key, err)
	}

	// Write references/outbound.md atomically, only when non-empty.
	if outboundMD != "" {
		outboundPath := filepath.Join(refsDir, "outbound.md")
		if err := atomicWrite(outboundPath, outboundMD, 0644); err != nil {
			return errext.Errorf("output: write references/outbound.md for %s: %w", key, err)
		}
	}

	return nil
}

// IssueDir returns the canonical per-issue directory path:
// filepath.Join(outputDir, key). Render and crawl packages use this
// to compute relative paths between issues without re-implementing the
// layout rule.
func IssueDir(outputDir, key string) string {
	return filepath.Join(outputDir, key)
}

// atomicWrite writes content to path using a temp-file-then-rename
// strategy. The temp file is created in the same directory as path so
// that os.Rename is guaranteed to be an atomic same-filesystem rename
// on POSIX systems.
func atomicWrite(path, content string, perm os.FileMode) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)

	// Create the temp file in the destination directory.
	tmp, err := os.CreateTemp(dir, base+".tmp.")
	if err != nil {
		return errext.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()

	// Clean up the temp file on any failure path.
	success := false
	defer func() {
		if !success {
			_ = os.Remove(tmpName)
		}
	}()

	// Write content.
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return errext.Errorf("write temp file: %w", err)
	}

	// Sync and close before rename so the data is durable.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return errext.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return errext.Errorf("close temp file: %w", err)
	}

	// Set permissions before rename so the final file has the right mode.
	if err := os.Chmod(tmpName, perm); err != nil {
		return errext.Errorf("chmod temp file: %w", err)
	}

	// Atomic rename into place.
	if err := os.Rename(tmpName, path); err != nil {
		return errext.Errorf("rename temp file: %w", err)
	}

	success = true
	return nil
}

// validateKey rejects keys that would cause path-traversal or other
// filesystem hazards. It does not enforce the full Jira key regex
// ([A-Z][A-Z0-9]+-[0-9]+) — that is the caller's responsibility.
func validateKey(key string) error {
	if key == "" {
		return errext.Errorf("output: issue key must not be empty")
	}
	// Reject any key that contains a path separator. This catches both
	// Unix "/" and Windows "\" to prevent directory traversal.
	if strings.ContainsAny(key, `/\`) {
		return errext.Errorf("output: issue key %q contains a path separator", key)
	}
	return nil
}
