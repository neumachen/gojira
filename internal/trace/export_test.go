package trace

// SetRandReadForTest swaps the package-level random source used by newID
// with fn and returns a restore function. Tests must defer or t.Cleanup the
// restore to avoid leaking the override into other tests.
//
// This file is only compiled into the test binary, so the helper is not
// part of the package's public API.
func SetRandReadForTest(fn func(p []byte) (int, error)) (restore func()) {
	prev := randRead
	randRead = fn
	return func() { randRead = prev }
}
