package api

import (
	"net/http"
	"net/url"
	"testing"
)

// TestSuppressionPathTakesAnEncodedAddress pins the one thing the spec now
// spells out about /suppressions/{email}: the segment is percent-encoded, so a
// plus-addressed recipient has to survive the round trip. A bare `+` in a path
// segment is a literal `+` here (it is only a space in a query string), and
// `%2B` has to decode to the same thing — otherwise "suppress a+b@example.com"
// and "is a+b@example.com suppressed" would disagree.
func TestSuppressionPathTakesAnEncodedAddress(t *testing.T) {
	e := newEnv(t)
	const address = "a+b@example.com"
	encoded := url.PathEscape(address) // a+b%40example.com
	fully := "a%2Bb%40example.com"

	w := e.do(http.MethodPut, "/api/v1/suppressions/"+fully,
		SuppressionInput{Reason: SuppressionReasonHardBounce})
	got := decodeInto[Suppression](t, w, http.StatusOK)
	if string(got.EmailNorm) != address {
		t.Fatalf("stored address = %q, want %q", got.EmailNorm, address)
	}

	// The same entry, reached through the other encoding of the same segment.
	read := decodeInto[Suppression](t,
		e.do(http.MethodGet, "/api/v1/suppressions/"+encoded, nil), http.StatusOK)
	if string(read.EmailNorm) != address {
		t.Fatalf("read back %q, want %q", read.EmailNorm, address)
	}

	// And the send path agrees with the API about who is suppressed.
	ok, _, err := e.st.Suppressions().IsSuppressed(t.Context(), address, e.now)
	if err != nil {
		t.Fatalf("IsSuppressed: %v", err)
	}
	if !ok {
		t.Fatalf("%s is not suppressed in the store", address)
	}

	if w := e.do(http.MethodDelete, "/api/v1/suppressions/"+fully, nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204; body: %s", w.Code, w.Body.String())
	}
	w = e.do(http.MethodGet, "/api/v1/suppressions/"+encoded, nil)
	decodeError(t, w, http.StatusNotFound, ErrorCodeNotFound)
}
