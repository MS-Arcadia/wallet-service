package consumer

import (
	"testing"

	"github.com/MS-Arcadia/wallet-service/internal/platform/event"
)

// The bytes auth-profile-service actually publishes for a new account.
//
// Kept verbatim rather than constructed, because the point is to fail when the *other* service
// changes shape. A helper that built this from the wallet's own structs would agree with itself
// forever and prove nothing.
//
// It earns its place: this service has consumed arcadia.auth.v1.UserRegistered since before an
// auth service existed, and the first one written published the payload flat with no event_id,
// no aggregate_id and no schema_version. Every event it emitted was rejected by this decoder, so
// no wallet was ever provisioned and nothing anywhere said why.
const authUserRegisteredEnvelope = `{
  "event_id": "11111111-1111-4111-8111-111111111111",
  "event_type": "arcadia.auth.v1.UserRegistered",
  "schema_version": 1,
  "occurred_at": "2026-07-28T12:00:00+00:00",
  "producer": "auth-profile-service",
  "aggregate_type": "User",
  "aggregate_id": "22222222-2222-4222-8222-222222222222",
  "payload": {
    "user_id": "22222222-2222-4222-8222-222222222222",
    "email": "player@example.com",
    "display_name": "A Player",
    "role": "BASIC_USER",
    "state": "PENDING"
  }
}`

func TestTheAuthServicesUserRegisteredIsReadable(t *testing.T) {
	envelope, err := event.Unmarshal([]byte(authUserRegisteredEnvelope))
	if err != nil {
		t.Fatalf("this service cannot read what auth publishes: %v", err)
	}

	if envelope.EventType != EventUserRegistered {
		t.Fatalf("event_type = %q, want %q — the router keys on this exact string",
			envelope.EventType, EventUserRegistered)
	}

	var payload UserRegisteredPayload
	if err := envelope.DecodePayload(&payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.UserID != "22222222-2222-4222-8222-222222222222" {
		t.Errorf("user_id = %q, want the aggregate id", payload.UserID)
	}
	if payload.Role != "BASIC_USER" {
		t.Errorf("role = %q, want BASIC_USER", payload.Role)
	}
	// The extra fields auth sends — display_name, state — are ignored rather than fatal, which is
	// what lets that service add a field without a coordinated release here.
}

func TestTheEnvelopeIsRejectedWhenItIsFlat(t *testing.T) {
	// What the auth service published before it was fixed: the payload merged into the top level.
	// This is here so a regression there fails a test here, rather than silently provisioning no
	// wallets.
	flat := `{"event_id":"1","event_type":"arcadia.auth.v1.UserRegistered",
	          "user_id":"u-1","role":"BASIC_USER","occurred_at":"2026-07-28T12:00:00Z"}`

	if _, err := event.Unmarshal([]byte(flat)); err == nil {
		t.Fatal("a flat event was accepted; the envelope contract is not being enforced")
	}
}
