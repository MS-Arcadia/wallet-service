package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MS-Arcadia/wallet-service/internal/platform/errs"
)

// TestPathUUIDRefusesAMalformedID covers the 500 this function was added to prevent.
//
// A non-UUID id in the URL used to travel all the way to PostgreSQL and come back as
// `invalid input syntax for type uuid` — a 500 with a database error in the log, for a request
// the caller simply got wrong. Three payment routes and three wallet routes had it.
func TestPathUUIDRefusesAMalformedID(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"the literal string null, which is what a client sends for an unset variable", "null"},
		{"undefined, for the same reason", "undefined"},
		{"an arbitrary word", "not-a-uuid"},
		{"a number", "42"},
		{"a uuid missing a character", "00000000-0000-4000-8000-00000000000"},
		{"a uuid with a character too many", "00000000-0000-4000-8000-0000000000001"},
		{"something SQL-shaped", "1 OR 1=1"},
		{"whitespace only", "   "},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PathUUID(request(tc.value), "id")
			if err == nil {
				t.Fatalf("PathUUID(%q) was accepted; it must be refused before it reaches a query", tc.value)
			}
			// The classification matters as much as the refusal: this is the caller's mistake,
			// so it must be a 400 and not a 500 that pages somebody.
			if code := errs.CodeOf(err); code != errs.CodeInvalidArgument {
				t.Errorf("PathUUID(%q) => code %q, want %q", tc.value, code, errs.CodeInvalidArgument)
			}
			if got := errs.HTTPStatus(errs.CodeOf(err)); got != http.StatusBadRequest {
				t.Errorf("PathUUID(%q) => status %d, want %d", tc.value, got, http.StatusBadRequest)
			}
		})
	}
}

func TestPathUUIDAcceptsAValidIDAndCanonicalisesIt(t *testing.T) {
	// uuid.Parse accepts forms PostgreSQL will not, so returning the caller's spelling would move
	// the same bug one layer down instead of fixing it.
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{"canonical", "3f2504e0-4f89-41d3-9a0c-0305e82c3301", "3f2504e0-4f89-41d3-9a0c-0305e82c3301"},
		{"uppercase", "3F2504E0-4F89-41D3-9A0C-0305E82C3301", "3f2504e0-4f89-41d3-9a0c-0305e82c3301"},
		{"brace-wrapped", "{3f2504e0-4f89-41d3-9a0c-0305e82c3301}", "3f2504e0-4f89-41d3-9a0c-0305e82c3301"},
		{"urn form", "urn:uuid:3f2504e0-4f89-41d3-9a0c-0305e82c3301", "3f2504e0-4f89-41d3-9a0c-0305e82c3301"},
		{"undashed", "3f2504e04f8941d39a0c0305e82c3301", "3f2504e0-4f89-41d3-9a0c-0305e82c3301"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PathUUID(request(tc.value), "id")
			if err != nil {
				t.Fatalf("PathUUID(%q) = %v, want it accepted", tc.value, err)
			}
			if got != tc.want {
				t.Errorf("PathUUID(%q) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

func TestPathUUIDStillReportsAMissingParameter(t *testing.T) {
	// The empty case belongs to PathValue and must keep working through the wrapper: a route
	// registered without its parameter is a wiring mistake, not a bad UUID.
	if _, err := PathUUID(httptest.NewRequest(http.MethodGet, "/v1/wallets/", nil), "id"); err == nil {
		t.Fatal("an absent path parameter was accepted")
	}
}

func request(id string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/wallets/x", nil)
	r.SetPathValue("id", id)
	return r
}
