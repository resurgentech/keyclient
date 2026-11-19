package request

import "time"

// SetSecond changes the unit of intervals and of expires_in, so that tests run
// fast. Ensure reads it once per call, in the caller's goroutine.
func SetSecond(d time.Duration) (restore func()) {
	old := second
	second = d
	return func() { second = old }
}

// SetRequestTimeout changes the bound of each HTTP call (RequestTimeout), so
// that a test of a hanging server runs fast.
func SetRequestTimeout(d time.Duration) (restore func()) {
	old := requestTimeout
	requestTimeout = d
	return func() { requestTimeout = old }
}
