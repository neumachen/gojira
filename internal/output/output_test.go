package output_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/neumachen/gojira/internal/output"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// helper reads a file and returns its content as a string.
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err, "readFile(%q)", path)
	return string(b)
}

// helper asserts a file does NOT exist.
func assertNotExists(t *testing.T, path string) {
	t.Helper()
	_, err := os.Stat(path)
	require.Error(t, err, "expected %q to not exist, but it does", path)
}

// helper asserts a path has the expected permission bits (masked to
// the lower 9 bits so sticky/setuid bits don't interfere).
func assertPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err, "stat(%q)", path)
	got := info.Mode().Perm()
	require.Equal(t, want, got, "perm(%q)", path)
}

// TestWrite_NewIssue asserts that writing a brand-new issue creates
// both files at the correct canonical paths with the correct content,
// and that directory permissions are 0755 and file permissions are 0644.
func TestWrite_NewIssue(t *testing.T) {
	dir := t.TempDir()
	const key = "PROJ-1147"
	const indexContent = "# PROJ-1147 — My Issue\n"
	const outboundContent = "## Outbound references\n"

	require.NoError(t, output.Write(dir, key, indexContent, outboundContent, false), "Write")

	issueDir := output.IssueDir(dir, key)
	refsDir := filepath.Join(issueDir, "references")
	indexPath := filepath.Join(issueDir, "index.md")
	outboundPath := filepath.Join(refsDir, "outbound.md")

	// Content checks.
	assert.Equal(t, indexContent, readFile(t, indexPath), "index.md content")
	assert.Equal(t, outboundContent, readFile(t, outboundPath), "outbound.md content")

	// Permission checks.
	assertPerm(t, issueDir, 0755)
	assertPerm(t, refsDir, 0755)
	assertPerm(t, indexPath, 0644)
	assertPerm(t, outboundPath, 0644)
}

// TestWrite_EmptyOutbound asserts that when outboundMD is empty,
// references/outbound.md is NOT created, but the references/ directory
// itself IS created (predictable structure for downstream tools).
func TestWrite_EmptyOutbound(t *testing.T) {
	dir := t.TempDir()
	const key = "PROJ-42"

	require.NoError(t, output.Write(dir, key, "# content\n", "", false), "Write")

	issueDir := output.IssueDir(dir, key)
	refsDir := filepath.Join(issueDir, "references")
	outboundPath := filepath.Join(refsDir, "outbound.md")

	// references/ directory must exist.
	_, err := os.Stat(refsDir)
	require.NoError(t, err, "references/ directory should exist")

	// outbound.md must NOT exist.
	assertNotExists(t, outboundPath)
}

// TestWrite_SkipIfExists asserts that when index.md already exists and
// refetch is false, Write returns ErrAlreadyExists (detectable via
// errors.Is) and leaves the original file content untouched.
func TestWrite_SkipIfExists(t *testing.T) {
	dir := t.TempDir()
	const key = "PROJ-1"
	const original = "ORIGINAL CONTENT\n"

	// Pre-create the issue directory and index.md.
	issueDir := output.IssueDir(dir, key)
	require.NoError(t, os.MkdirAll(issueDir, 0755), "MkdirAll")
	indexPath := filepath.Join(issueDir, "index.md")
	require.NoError(t, os.WriteFile(indexPath, []byte(original), 0644), "WriteFile")

	// Call Write with refetch=false.
	err := output.Write(dir, key, "NEW CONTENT\n", "", false)
	require.ErrorIs(t, err, output.ErrAlreadyExists)

	// Original content must be untouched.
	assert.Equal(t, original, readFile(t, indexPath), "index.md must not be modified")
}

// TestWrite_RefetchOverride asserts that when refetch is true, an
// existing index.md is overwritten with the new content.
func TestWrite_RefetchOverride(t *testing.T) {
	dir := t.TempDir()
	const key = "PROJ-2"

	// First write.
	require.NoError(t, output.Write(dir, key, "OLD\n", "", false), "first Write")

	// Second write with refetch=true.
	require.NoError(t, output.Write(dir, key, "NEW\n", "", true), "second Write (refetch)")

	indexPath := filepath.Join(output.IssueDir(dir, key), "index.md")
	assert.Equal(t, "NEW\n", readFile(t, indexPath), "index.md after refetch")
}

// TestWrite_NoTmpFilesAfterSuccess asserts that no .tmp files remain
// in the issue directory after a successful write. This is a minimal
// partial-write safety check.
func TestWrite_NoTmpFilesAfterSuccess(t *testing.T) {
	dir := t.TempDir()
	const key = "PROJ-3"

	require.NoError(t, output.Write(dir, key, "# content\n", "## refs\n", false), "Write")

	issueDir := output.IssueDir(dir, key)
	refsDir := filepath.Join(issueDir, "references")

	for _, searchDir := range []string{issueDir, refsDir} {
		entries, err := os.ReadDir(searchDir)
		require.NoError(t, err, "ReadDir(%q)", searchDir)
		for _, e := range entries {
			name := e.Name()
			if len(name) > 4 && name[len(name)-4:] == ".tmp" {
				assert.Failf(t, "unexpected .tmp file", "in %q: %s", searchDir, name)
			}
			// Also catch the pattern used by os.CreateTemp: "*.tmp.<random>"
			if filepath.Ext(name) != ".md" && !e.IsDir() {
				assert.Failf(t, "unexpected non-.md file", "in %q: %s", searchDir, name)
			}
		}
	}
}

// TestWrite_RejectEmptyKey asserts that Write returns an error when
// the key is an empty string.
func TestWrite_RejectEmptyKey(t *testing.T) {
	dir := t.TempDir()
	err := output.Write(dir, "", "# content\n", "", false)
	assert.Error(t, err, "expected error for empty key")
}

// TestWrite_RejectKeyWithSlash asserts that Write returns an error when
// the key contains a forward slash (path-traversal prevention).
func TestWrite_RejectKeyWithSlash(t *testing.T) {
	dir := t.TempDir()
	err := output.Write(dir, "../evil/PROJ-1", "# content\n", "", false)
	assert.Error(t, err, "expected error for key with path separator")
}

// TestWrite_RejectKeyWithBackslash asserts that Write returns an error
// when the key contains a backslash (Windows path-traversal prevention).
func TestWrite_RejectKeyWithBackslash(t *testing.T) {
	dir := t.TempDir()
	err := output.Write(dir, `PROJ\1`, "# content\n", "", false)
	assert.Error(t, err, "expected error for key with backslash")
}

// TestIssueDir asserts that IssueDir returns the expected canonical path.
func TestIssueDir(t *testing.T) {
	got := output.IssueDir("/tmp/out", "PROJ-1147")
	assert.Equal(t, "/tmp/out/PROJ-1147", got, "IssueDir")
}

// TestWrite_DirectoryCreation asserts that Write creates the full
// directory tree even when neither the issue directory nor the
// references/ subdirectory exists yet.
func TestWrite_DirectoryCreation(t *testing.T) {
	// Use a nested output dir that does not yet exist.
	base := t.TempDir()
	dir := filepath.Join(base, "nested", "output")

	const key = "PROJ-99"
	require.NoError(t, output.Write(dir, key, "# content\n", "## refs\n", false), "Write")

	issueDir := output.IssueDir(dir, key)
	refsDir := filepath.Join(issueDir, "references")

	_, err := os.Stat(issueDir)
	require.NoError(t, err, "issue dir must be created")
	_, err = os.Stat(refsDir)
	require.NoError(t, err, "references dir must be created")
}
