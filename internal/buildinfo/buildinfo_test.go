package buildinfo

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestUnstampedDefaults verifies the un-stamped (development) reporting
// contract. In the test context, commit="dev" and ref="", and
// debug.ReadBuildInfo() returns Main.Version="(devel)" (no vcs.revision
// in test binaries). The functions should fall back to "dev".
func TestUnstampedDefaults(t *testing.T) {
	// In test context, ReadBuildInfo returns (devel) and no VCS info,
	// so all functions should return "dev" or derivatives.
	assert.Equal(t, "dev", Version(),
		"Version() must return 'dev' when unstamped and ReadBuildInfo returns (devel)")
	assert.Equal(t, "gojira/dev", UserAgent(),
		"UserAgent() must be gojira/<Version()>")

	// Revision may return "dev" or a real SHA if tests are run from a
	// git checkout with VCS info embedded. We accept either.
	rev := Revision()
	assert.NotEmpty(t, rev, "Revision() must return a non-empty string")

	// FullVersion depends on Version and Revision
	full := FullVersion()
	assert.NotEmpty(t, full, "FullVersion() must return a non-empty string")
}

// TestVersionResolution tests the Version() resolution logic by checking
// the priority order documentation. Since we can't mutate package consts,
// we test the observable behavior in the default state.
func TestVersionResolution(t *testing.T) {
	// With default consts (commit="dev", ref=""), Version() should either:
	// - Return the module version from ReadBuildInfo if available and not "(devel)"
	// - Return "dev" otherwise
	//
	// In test context, ReadBuildInfo typically returns "(devel)", so we expect "dev".
	v := Version()
	// We can't assert exactly "dev" because if tests are run via
	// `go test -cover` from a versioned module install, it might differ.
	// But it should be non-empty and not contain error text.
	assert.NotEmpty(t, v)
	assert.NotContains(t, v, "error")
}

// TestFullVersionFormat verifies the FullVersion format logic.
func TestFullVersionFormat(t *testing.T) {
	full := FullVersion()
	v := Version()

	// If version is "dev", full should just be "dev" (no @)
	if v == "dev" {
		assert.Equal(t, "dev", full,
			"FullVersion should be 'dev' when Version is 'dev'")
	}
	// If version has a real value, full might include @revision
	// We just verify it starts with the version
	if v != "dev" {
		assert.Contains(t, full, v,
			"FullVersion should contain the Version value")
	}
}

// TestUserAgentFormat verifies UserAgent follows the expected format.
func TestUserAgentFormat(t *testing.T) {
	ua := UserAgent()
	assert.Regexp(t, `^gojira/`, ua, "UserAgent must start with 'gojira/'")
	assert.Equal(t, "gojira/"+Version(), ua, "UserAgent must be 'gojira/' + Version()")
}
