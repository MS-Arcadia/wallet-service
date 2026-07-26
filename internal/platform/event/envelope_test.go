package event_test

import (
	"testing"
	"time"

	"github.com/MS-Arcadia/wallet-service/internal/platform/event"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type walletDebited struct {
	WalletID    string `json:"wallet_id"`
	AmountMinor int64  `json:"amount_minor"`
}

func TestBuilderRoundTrip(t *testing.T) {
	at := time.Date(2026, 7, 26, 10, 30, 0, 0, time.UTC)
	builder := event.NewBuilder("wallet-service", 1)

	envelope, err := builder.Build(
		"evt-1", "arcadia.wallet.v1.WalletDebited", "wallet", "w-1", at,
		walletDebited{WalletID: "w-1", AmountMinor: 5000},
	)
	require.NoError(t, err)
	assert.Equal(t, "wallet-service", envelope.Producer)
	assert.Equal(t, 1, envelope.SchemaVersion)
	assert.Equal(t, "w-1", envelope.PartitionKey())

	raw, err := envelope.Marshal()
	require.NoError(t, err)

	decoded, err := event.Unmarshal(raw)
	require.NoError(t, err)
	assert.Equal(t, envelope.EventID, decoded.EventID)
	assert.Equal(t, envelope.EventType, decoded.EventType)
	assert.True(t, at.Equal(decoded.OccurredAt))

	var payload walletDebited
	require.NoError(t, decoded.DecodePayload(&payload))
	assert.Equal(t, "w-1", payload.WalletID)
	assert.EqualValues(t, 5000, payload.AmountMinor)
}

func TestBuilderDefaultsSchemaVersion(t *testing.T) {
	envelope, err := event.NewBuilder("wallet-service", 0).
		Build("evt-1", "t", "wallet", "w-1", time.Now(), struct{}{})
	require.NoError(t, err)
	assert.Equal(t, 1, envelope.SchemaVersion)
}

func TestValidateRejectsIncompleteEnvelopes(t *testing.T) {
	valid := event.Envelope{
		EventID:       "evt-1",
		EventType:     "t",
		SchemaVersion: 1,
		OccurredAt:    time.Now(),
		AggregateID:   "w-1",
	}
	require.NoError(t, valid.Validate())

	missingID := valid
	missingID.EventID = ""
	assert.ErrorIs(t, missingID.Validate(), event.ErrMissingEventID)

	missingType := valid
	missingType.EventType = ""
	assert.ErrorIs(t, missingType.Validate(), event.ErrMissingEventType)

	missingAggregate := valid
	missingAggregate.AggregateID = ""
	assert.ErrorIs(t, missingAggregate.Validate(), event.ErrMissingAggregateID)

	missingTime := valid
	missingTime.OccurredAt = time.Time{}
	assert.ErrorIs(t, missingTime.Validate(), event.ErrMissingOccurredAt)

	badVersion := valid
	badVersion.SchemaVersion = 0
	assert.ErrorIs(t, badVersion.Validate(), event.ErrUnsupportedVersion)
}

func TestUnmarshalRejectsGarbage(t *testing.T) {
	_, err := event.Unmarshal([]byte("not json"))
	assert.ErrorIs(t, err, event.ErrMalformedEnvelope)
}

func TestUnmarshalValidates(t *testing.T) {
	_, err := event.Unmarshal([]byte(`{"event_type":"t","schema_version":1,"aggregate_id":"w-1"}`))
	assert.ErrorIs(t, err, event.ErrMissingEventID)
}

func TestDecodePayloadRejectsEmpty(t *testing.T) {
	var target walletDebited
	err := event.Envelope{EventType: "t"}.DecodePayload(&target)
	assert.Error(t, err)
}

func TestBuildValidatesResult(t *testing.T) {
	_, err := event.NewBuilder("wallet-service", 1).
		Build("", "t", "wallet", "w-1", time.Now(), struct{}{})
	assert.ErrorIs(t, err, event.ErrMissingEventID)
}

func TestOccurredAtIsNormalisedToUTC(t *testing.T) {
	tehran := time.FixedZone("Asia/Tehran", int(3*time.Hour/time.Second)+1800)
	local := time.Date(2026, 7, 26, 14, 0, 0, 0, tehran)

	envelope, err := event.NewBuilder("wallet-service", 1).
		Build("evt-1", "t", "wallet", "w-1", local, struct{}{})
	require.NoError(t, err)
	assert.Equal(t, time.UTC, envelope.OccurredAt.Location())
	assert.True(t, local.Equal(envelope.OccurredAt))
}
