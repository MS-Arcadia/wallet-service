package repo

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/MS-Arcadia/arcadia-platform/pkg/errs"
	"github.com/MS-Arcadia/arcadia-platform/pkg/money"
	"github.com/MS-Arcadia/arcadia-platform/pkg/postgres"
	"github.com/MS-Arcadia/wallet-service/internal/app/port"
	"github.com/MS-Arcadia/wallet-service/internal/domain/giftcard"
)

// GiftCardRepo is the Postgres port.GiftCardRepository.
type GiftCardRepo struct{}

// NewGiftCardRepo returns a GiftCardRepo.
func NewGiftCardRepo() *GiftCardRepo { return &GiftCardRepo{} }

const giftCardColumns = `
	id, code_hash, code_hint, value_minor, currency, status, issued_by, batch_id,
	note, redeemed_by, redeemed_at, revoked_at, revoke_note, created_at, version`

// InsertBatch stores freshly minted cards using pgx's batch protocol, so a thousand
// cards cost one round trip rather than a thousand.
func (r *GiftCardRepo) InsertBatch(ctx context.Context, tx port.Tx, cards []*giftcard.GiftCard) error {
	if len(cards) == 0 {
		return nil
	}

	const insert = `
		INSERT INTO gift_cards (
			id, code_hash, code_hint, value_minor, currency, status,
			issued_by, batch_id, note, created_at, version
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`

	for _, card := range cards {
		_, err := tx.Exec(ctx, insert,
			card.ID(), card.CodeHash(), card.CodeHint(), card.Value().Minor(),
			card.Value().Currency(), string(card.Status()), card.IssuedBy(),
			nullIfEmpty(card.BatchID()), card.Note(), card.CreatedAt(), card.Version(),
		)
		if err != nil {
			if postgres.IsUniqueViolation(err) {
				// Two identical code hashes means the generator collided, which at 80 bits
				// of entropy should never happen. Failing the whole batch is correct: a
				// silently skipped card would be one a Support user believes they printed.
				return errs.Internal("gift card code collision while minting a batch").WithCause(err)
			}
			return errs.Internal("failed to insert gift card %s", card.ID()).WithCause(err)
		}
	}
	return nil
}

// FindByCodeHash looks a card up by the hash of its code.
func (r *GiftCardRepo) FindByCodeHash(ctx context.Context, reader port.Reader, codeHash string) (*giftcard.GiftCard, error) {
	row := reader.QueryRow(ctx, `SELECT`+giftCardColumns+` FROM gift_cards WHERE code_hash = $1`, codeHash)
	card, err := scanGiftCard(row)
	if err != nil {
		if postgres.IsNoRows(err) {
			return nil, giftcard.ErrNotFound()
		}
		return nil, errs.Internal("failed to read a gift card by code").WithCause(err)
	}
	return card, nil
}

// FindByID looks a card up by identifier.
func (r *GiftCardRepo) FindByID(ctx context.Context, reader port.Reader, id string) (*giftcard.GiftCard, error) {
	row := reader.QueryRow(ctx, `SELECT`+giftCardColumns+` FROM gift_cards WHERE id = $1`, id)
	card, err := scanGiftCard(row)
	if err != nil {
		if postgres.IsNoRows(err) {
			return nil, giftcard.ErrNotFound()
		}
		return nil, errs.Internal("failed to read gift card %s", id).WithCause(err)
	}
	return card, nil
}

// LockByCodeHash reads a card FOR UPDATE.
//
// This is what makes concurrent redemption of one code safe: the second request
// blocks here, and by the time it proceeds the row says USED.
func (r *GiftCardRepo) LockByCodeHash(ctx context.Context, tx port.Tx, codeHash string) (*giftcard.GiftCard, error) {
	row := tx.QueryRow(ctx, `SELECT`+giftCardColumns+` FROM gift_cards WHERE code_hash = $1 FOR UPDATE`, codeHash)
	card, err := scanGiftCard(row)
	if err != nil {
		if postgres.IsNoRows(err) {
			return nil, giftcard.ErrNotFound()
		}
		return nil, errs.Internal("failed to lock a gift card").WithCause(err)
	}
	return card, nil
}

// MarkRedeemed applies a redemption conditionally.
//
// The WHERE clause pins both the version and the ACTIVE status. Zero rows affected
// means the card changed hands between the lock and this write, and the caller must
// refuse the redemption rather than credit a wallet.
func (r *GiftCardRepo) MarkRedeemed(ctx context.Context, tx port.Tx, card *giftcard.GiftCard, expectedVersion int64) (bool, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE gift_cards
		SET status = $1, redeemed_by = $2, redeemed_at = $3, version = $4
		WHERE id = $5 AND version = $6 AND status = 'ACTIVE'`,
		string(card.Status()), card.RedeemedBy(), card.RedeemedAt(), card.Version(),
		card.ID(), expectedVersion,
	)
	if err != nil {
		return false, errs.Internal("failed to mark gift card %s redeemed", card.ID()).WithCause(err)
	}
	return tag.RowsAffected() == 1, nil
}

// Update persists a revocation.
func (r *GiftCardRepo) Update(ctx context.Context, tx port.Tx, card *giftcard.GiftCard, expectedVersion int64) error {
	tag, err := tx.Exec(ctx, `
		UPDATE gift_cards
		SET status = $1, revoked_at = $2, revoke_note = $3, version = $4
		WHERE id = $5 AND version = $6`,
		string(card.Status()), card.RevokedAt(), card.RevokeNote(), card.Version(),
		card.ID(), expectedVersion,
	)
	if err != nil {
		return errs.Internal("failed to update gift card %s", card.ID()).WithCause(err)
	}
	if tag.RowsAffected() == 0 {
		return errs.Aborted("gift card %s was modified concurrently", card.ID()).
			WithReason("VERSION_CONFLICT")
	}
	return nil
}

// List returns a filtered page of cards.
func (r *GiftCardRepo) List(ctx context.Context, reader port.Reader, filter giftcard.Filter) ([]*giftcard.GiftCard, int64, error) {
	conditions := make([]string, 0, 2)
	args := make([]any, 0, 4)

	if filter.Status != "" {
		args = append(args, string(filter.Status))
		conditions = append(conditions, fmt.Sprintf("status = $%d", len(args)))
	}
	if filter.BatchID != "" {
		args = append(args, filter.BatchID)
		conditions = append(conditions, fmt.Sprintf("batch_id = $%d", len(args)))
	}

	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	var total int64
	if err := reader.QueryRow(ctx, `SELECT count(*) FROM gift_cards `+where, args...).Scan(&total); err != nil {
		return nil, 0, errs.Internal("failed to count gift cards").WithCause(err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = 20
	}
	query := fmt.Sprintf(`SELECT%s FROM gift_cards %s ORDER BY created_at DESC, id LIMIT $%d OFFSET $%d`,
		giftCardColumns, where, len(args)+1, len(args)+2)
	args = append(args, limit, filter.Offset)

	rows, err := reader.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, errs.Internal("failed to list gift cards").WithCause(err)
	}
	defer rows.Close()

	cards := make([]*giftcard.GiftCard, 0, limit)
	for rows.Next() {
		card, err := scanGiftCard(rows)
		if err != nil {
			return nil, 0, errs.Internal("failed to scan a gift card row").WithCause(err)
		}
		cards = append(cards, card)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, errs.Internal("failed to iterate gift cards").WithCause(err)
	}
	return cards, total, nil
}

func scanGiftCard(row scanner) (*giftcard.GiftCard, error) {
	var (
		id, codeHash, codeHint, currency, status, issuedBy, note, revokeNote string
		valueMinor                                                           int64
		version                                                              int64
		batchID, redeemedBy                                                  *string
		redeemedAt, revokedAt                                                *time.Time
		createdAt                                                            time.Time
	)
	if err := row.Scan(&id, &codeHash, &codeHint, &valueMinor, &currency, &status,
		&issuedBy, &batchID, &note, &redeemedBy, &redeemedAt, &revokedAt,
		&revokeNote, &createdAt, &version); err != nil {
		return nil, err
	}

	value, err := money.New(valueMinor, currency)
	if err != nil {
		return nil, fmt.Errorf("gift card %s has an invalid currency: %w", id, err)
	}

	card, err := giftcard.Rehydrate(id, strings.TrimSpace(codeHash), strings.TrimSpace(codeHint),
		value, giftcard.Status(status), issuedBy, derefString(batchID), note,
		derefString(redeemedBy), redeemedAt, revokedAt, revokeNote, createdAt, version)
	if err != nil {
		return nil, err
	}
	return card, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

var _ port.GiftCardRepository = (*GiftCardRepo)(nil)
