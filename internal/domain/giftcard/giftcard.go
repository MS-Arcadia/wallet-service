// Package giftcard models prepaid gift cards.
//
// A gift-card code is a bearer instrument: whoever knows it can spend it. The code
// is therefore treated like a password — only its salted hash is stored, the
// plaintext is returned exactly once at issuance, and lookups happen by hash. A
// dump of the gift_cards table yields nothing spendable.
package giftcard

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/MS-Arcadia/arcadia-platform/pkg/errs"
	"github.com/MS-Arcadia/arcadia-platform/pkg/idgen"
	"github.com/MS-Arcadia/arcadia-platform/pkg/money"
)

// Status is the gift card's lifecycle state.
type Status string

const (
	// StatusActive — issued and unredeemed.
	StatusActive Status = "ACTIVE"
	// StatusUsed — redeemed. Terminal; the requirements specify no expiry, so a card
	// stays valid until it is spent.
	StatusUsed Status = "USED"
	// StatusRevoked — cancelled by Support before redemption, e.g. a batch printed
	// with the wrong value.
	StatusRevoked Status = "REVOKED"
)

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	switch s {
	case StatusActive, StatusUsed, StatusRevoked:
		return true
	default:
		return false
	}
}

// Reason codes returned by this package.
const (
	ReasonCodeNotFound    = "GIFT_CARD_NOT_FOUND"
	ReasonCodeAlreadyUsed = "GIFT_CARD_ALREADY_USED"
	ReasonCodeRevoked     = "GIFT_CARD_REVOKED"
	ReasonCodeSelfIssued  = "GIFT_CARD_SELF_REDEMPTION"
)

// GiftCard is the aggregate.
type GiftCard struct {
	id string
	// codeHash is the HMAC of the normalised code. The plaintext is never stored.
	codeHash string
	// codeHint is the last four characters, so that Support can identify a card in
	// a list without being able to spend it.
	codeHint string
	value    money.Money
	status   Status
	// issuedBy is the Support user that minted the card.
	issuedBy string
	// batchID groups cards minted in one request.
	batchID string
	note    string
	// redeemedBy and redeemedAt are set exactly once.
	redeemedBy string
	redeemedAt *time.Time
	revokedAt  *time.Time
	revokeNote string
	createdAt  time.Time
	version    int64
}

// Hasher derives the stored hash from a plaintext code.
//
// An HMAC with a server-side pepper is used rather than a bare SHA-256: gift-card
// codes come from a 32-symbol alphabet, so a plain digest would be brute-forceable
// offline by anyone who obtained the table. Without the pepper — which lives in a
// secret, not the database — precomputation is useless.
type Hasher struct {
	pepper []byte
}

// NewHasher builds a Hasher. The pepper must be at least 32 bytes.
func NewHasher(pepper string) (*Hasher, error) {
	if len(pepper) < 32 {
		return nil, errs.Internal("the gift card pepper must be at least 32 bytes")
	}
	return &Hasher{pepper: []byte(pepper)}, nil
}

// Hash returns the stored representation of a code. The code is normalised first,
// so formatting differences do not matter.
func (h *Hasher) Hash(code string) string {
	mac := hmac.New(sha256.New, h.pepper)
	mac.Write([]byte(idgen.NormalizeCode(code)))
	return hex.EncodeToString(mac.Sum(nil))
}

// Issue mints a new gift card and returns it together with its plaintext code.
//
// The plaintext is returned rather than stored; the caller must hand it to the
// Support user immediately, because it can never be recovered afterwards.
func Issue(
	id string,
	value money.Money,
	issuedBy, batchID, note string,
	hasher *Hasher,
	now time.Time,
) (*GiftCard, string, error) {
	if id == "" {
		return nil, "", errs.Internal("a gift card requires an id")
	}
	if !value.IsPositive() {
		return nil, "", errs.InvalidArgument("a gift card value must be greater than zero, got %s", value)
	}
	if issuedBy == "" {
		return nil, "", errs.InvalidArgument("the issuing support user is required")
	}

	code, err := idgen.NewGiftCardCode()
	if err != nil {
		return nil, "", errs.Internal("failed to generate a gift card code").WithCause(err)
	}
	normalised := idgen.NormalizeCode(code)

	return &GiftCard{
		id:        id,
		codeHash:  hasher.Hash(code),
		codeHint:  normalised[len(normalised)-4:],
		value:     value,
		status:    StatusActive,
		issuedBy:  issuedBy,
		batchID:   batchID,
		note:      note,
		createdAt: now.UTC(),
		version:   1,
	}, code, nil
}

// Rehydrate reconstructs a GiftCard from stored state.
func Rehydrate(
	id, codeHash, codeHint string,
	value money.Money,
	status Status,
	issuedBy, batchID, note, redeemedBy string,
	redeemedAt, revokedAt *time.Time,
	revokeNote string,
	createdAt time.Time,
	version int64,
) (*GiftCard, error) {
	if id == "" {
		return nil, errs.Internal("cannot rehydrate a gift card without an id")
	}
	if !status.Valid() {
		return nil, errs.Internal("cannot rehydrate gift card %s with unknown status %q", id, status)
	}
	return &GiftCard{
		id:         id,
		codeHash:   codeHash,
		codeHint:   codeHint,
		value:      value,
		status:     status,
		issuedBy:   issuedBy,
		batchID:    batchID,
		note:       note,
		redeemedBy: redeemedBy,
		redeemedAt: redeemedAt,
		revokedAt:  revokedAt,
		revokeNote: revokeNote,
		createdAt:  createdAt.UTC(),
		version:    version,
	}, nil
}

// Accessors.

// ID returns the identifier.
func (g *GiftCard) ID() string { return g.id }

// CodeHash returns the stored hash.
func (g *GiftCard) CodeHash() string { return g.codeHash }

// CodeHint returns the last four characters of the code.
func (g *GiftCard) CodeHint() string { return g.codeHint }

// Value returns the card's face value.
func (g *GiftCard) Value() money.Money { return g.value }

// Status returns the lifecycle state.
func (g *GiftCard) Status() Status { return g.status }

// IssuedBy returns the minting Support user.
func (g *GiftCard) IssuedBy() string { return g.issuedBy }

// BatchID returns the issuance batch.
func (g *GiftCard) BatchID() string { return g.batchID }

// Note returns the issuance note.
func (g *GiftCard) Note() string { return g.note }

// RedeemedBy returns the redeeming user, or "".
func (g *GiftCard) RedeemedBy() string { return g.redeemedBy }

// RedeemedAt returns the redemption time, or nil.
func (g *GiftCard) RedeemedAt() *time.Time { return g.redeemedAt }

// RevokedAt returns the revocation time, or nil.
func (g *GiftCard) RevokedAt() *time.Time { return g.revokedAt }

// RevokeNote returns the revocation justification.
func (g *GiftCard) RevokeNote() string { return g.revokeNote }

// CreatedAt returns the issuance time.
func (g *GiftCard) CreatedAt() time.Time { return g.createdAt }

// Version returns the optimistic-concurrency version.
func (g *GiftCard) Version() int64 { return g.version }

// Redeem marks the card as spent by userID.
//
// The state check here is only the first line of defence against double
// redemption; the decisive one is a conditional UPDATE in the repository that only
// matches a row still in ACTIVE. Two concurrent redemptions of one code are a
// realistic race, and the database is the only place it can be settled.
func (g *GiftCard) Redeem(userID string, now time.Time) error {
	if userID == "" {
		return errs.InvalidArgument("the redeeming user is required")
	}
	switch g.status {
	case StatusUsed:
		return errs.Conflict("this gift card has already been redeemed").
			WithReason(ReasonCodeAlreadyUsed)
	case StatusRevoked:
		return errs.Conflict("this gift card has been revoked").
			WithReason(ReasonCodeRevoked)
	}
	// A Support user redeeming a card they issued themselves is the classic insider
	// fraud pattern, and it is trivial to block.
	if userID == g.issuedBy {
		return errs.PermissionDenied("a gift card cannot be redeemed by the user who issued it").
			WithReason(ReasonCodeSelfIssued)
	}

	redeemedAt := now.UTC()
	g.status = StatusUsed
	g.redeemedBy = userID
	g.redeemedAt = &redeemedAt
	g.version++
	return nil
}

// Revoke cancels an unredeemed card.
func (g *GiftCard) Revoke(note string, now time.Time) error {
	switch g.status {
	case StatusUsed:
		// Revoking a spent card would mean clawing money back out of a user's wallet,
		// which is an adjustment, not a revocation.
		return errs.Conflict("a redeemed gift card cannot be revoked").
			WithReason(ReasonCodeAlreadyUsed)
	case StatusRevoked:
		return errs.Conflict("this gift card is already revoked").
			WithReason(ReasonCodeRevoked)
	}

	revokedAt := now.UTC()
	g.status = StatusRevoked
	g.revokedAt = &revokedAt
	g.revokeNote = note
	g.version++
	return nil
}

// ErrNotFound reports an unknown code.
//
// The message never distinguishes "no such code" from "wrong format": telling an
// attacker which of their guesses were structurally valid would turn the endpoint
// into an oracle for enumerating live codes.
func ErrNotFound() error {
	return errs.NotFound("this gift card code is not valid").WithReason(ReasonCodeNotFound)
}

// Filter narrows a gift-card query.
type Filter struct {
	Status  Status
	BatchID string
	Limit   int
	Offset  int
}
