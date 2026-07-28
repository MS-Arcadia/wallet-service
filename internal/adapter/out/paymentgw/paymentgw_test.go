package paymentgw

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/MS-Arcadia/wallet-service/internal/platform/errs"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestTranslateReclassifiesOurOwnRefusal covers the bug this method exists for.
//
// Every bank top-up failed as a **401** because the wallet sent no service token to the Payment
// Adapter at all. The adapter answered UNAUTHENTICATED, the wallet forwarded it unchanged, and the
// buyer was told their credentials were bad — for a request their session had nothing to do with,
// and with the only useful remedy (log in again) guaranteed not to help.
func TestTranslateReclassifiesOurOwnRefusal(t *testing.T) {
	client := &Client{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	tests := []struct {
		name string
		code codes.Code
	}{
		{"the adapter rejected our credential", codes.Unauthenticated},
		{"the adapter accepted it and refused the role", codes.PermissionDenied},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := client.translate(status.Error(tc.code, "boom"), "starting a top-up")

			if got := errs.CodeOf(err); got != errs.CodeUnavailable {
				t.Errorf("translate(%s) => %s, want %s", tc.code, got, errs.CodeUnavailable)
			}
			// 503, so a client retries or shows an outage rather than sending the user to a
			// login page that cannot fix anything.
			if got := errs.HTTPStatus(errs.CodeOf(err)); got != 503 {
				t.Errorf("translate(%s) => HTTP %d, want 503", tc.code, got)
			}
			if got := errs.ReasonOf(err); got != "PAYMENT_GATEWAY_UNAUTHORIZED" {
				t.Errorf("translate(%s) => reason %q, want PAYMENT_GATEWAY_UNAUTHORIZED", tc.code, got)
			}
		})
	}
}

func TestTranslatePassesEverythingElseThrough(t *testing.T) {
	// The reclassification is narrow on purpose. A missing intent really is missing, and a bad
	// amount really was the caller's mistake — flattening those into 503 would hide real answers
	// behind a retry the client would make forever.
	client := &Client{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	tests := []struct {
		name string
		code codes.Code
		want errs.Code
	}{
		{"a missing intent stays not found", codes.NotFound, errs.CodeNotFound},
		{"a bad request stays a bad request", codes.InvalidArgument, errs.CodeInvalidArgument},
		{"an outage stays an outage", codes.Unavailable, errs.CodeUnavailable},
		{"a deadline stays a deadline", codes.DeadlineExceeded, errs.CodeDeadlineExceeded},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := client.translate(status.Error(tc.code, "boom"), "reading intent %s", "abc")
			if got := errs.CodeOf(err); got != tc.want {
				t.Errorf("translate(%s) => %s, want %s", tc.code, got, tc.want)
			}
		})
	}
}

func TestTranslateKeepsTheContextMessage(t *testing.T) {
	// The wrapping message is what says *which* call failed. A reclassified error that lost it
	// would leave an operator with "the payment adapter is not accepting requests" and no idea
	// whether a top-up or a reconciliation read produced it.
	client := &Client{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	err := client.translate(status.Error(codes.Unauthenticated, "boom"), "starting a top-up for %s", "u-1")
	if !strings.Contains(err.Error(), "starting a top-up for u-1") {
		t.Errorf("translate() = %q, want it to name the call", err.Error())
	}
}
