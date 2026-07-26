package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/MS-Arcadia/wallet-service/internal/app/port"
	"github.com/MS-Arcadia/wallet-service/internal/domain/ledger"
	"github.com/MS-Arcadia/wallet-service/internal/domain/wallet"
	"github.com/MS-Arcadia/wallet-service/internal/platform/authn"
	"github.com/MS-Arcadia/wallet-service/internal/platform/errs"
	"github.com/MS-Arcadia/wallet-service/internal/platform/logx"
	"github.com/MS-Arcadia/wallet-service/internal/platform/money"
)

// core holds the plumbing every use-case service shares: dependencies, event
// emission, idempotency and the one code path that writes to the ledger.
//
// The services embed it rather than reimplementing any of this. Gift-card
// redemption, interest accrual and a saga debit are very different use cases, but
// all three must update a balance, append an immutable entry, publish an event and
// write an audit record — and if any one of them forgot a step, reconciliation
// would start failing.
type core struct {
	deps Deps
	*emitter
}

func newCore(deps Deps) *core {
	return &core{
		deps:    deps,
		emitter: newEmitter(deps.Publisher, deps.Producer, deps.SchemaVersion, deps.IDs),
	}
}

// recordMovement persists a balance change atomically with its ledger entry, its
// domain event and its audit record.
func (c *core) recordMovement(
	ctx context.Context,
	tx port.Tx,
	w *wallet.Wallet,
	movement wallet.Movement,
	versionAtLoad int64,
	idempotencyKey, actorID string,
	now time.Time,
) (ledger.Entry, error) {
	if err := c.deps.Wallets.Update(ctx, tx, w, versionAtLoad); err != nil {
		return ledger.Entry{}, err
	}

	entry, err := ledger.NewEntry(c.deps.IDs.NewID(), w, movement,
		logx.CorrelationID(ctx), idempotencyKey, now)
	if err != nil {
		return ledger.Entry{}, err
	}
	if err := c.deps.Ledger.Append(ctx, tx, entry); err != nil {
		return ledger.Entry{}, err
	}

	if err := c.emit(ctx, tx, movementEventType(movement.Direction), aggregateWallet, w.ID(), now,
		movementPayload(entry)); err != nil {
		return ledger.Entry{}, err
	}
	if err := c.emitAudit(ctx, tx, entry, actorID); err != nil {
		return ledger.Entry{}, err
	}
	return entry, nil
}

// claim reserves an idempotency key inside tx.
//
// It returns claimed=true when the caller should perform the operation, and
// claimed=false together with the stored record when the request has already been
// processed and its response must be replayed.
func (c *core) claim(
	ctx context.Context,
	tx port.Tx,
	operation, key, walletScope string,
	request any,
) (claimed bool, existing *port.IdempotencyRecord, err error) {
	record := port.IdempotencyRecord{
		Key:         key,
		Operation:   operation,
		RequestHash: hashRequest(request),
		WalletID:    walletScope,
		CreatedAt:   c.deps.Clock.Now(),
	}

	found, claimed, err := c.deps.Idempotency.Claim(ctx, tx, record)
	if err != nil {
		return false, nil, err
	}
	if claimed {
		return true, nil, nil
	}

	// Same key, different payload. That is a client bug — a key reused for a
	// different request — and quietly returning the old answer would hide it.
	if found.RequestHash != record.RequestHash {
		return false, nil, errs.Conflict(
			"idempotency key %q was already used for a different request", key).
			WithReason("IDEMPOTENCY_KEY_REUSED")
	}
	if len(found.Response) == 0 {
		// The original attempt claimed the key and then failed before storing a
		// response, or is still in flight. Retrying is right, but not instantly.
		return false, nil, errs.Aborted(
			"a request with idempotency key %q is still in progress; retry shortly", key).
			WithReason("IDEMPOTENCY_IN_PROGRESS")
	}
	return false, found, nil
}

// saveResponse attaches a result to a claimed key so that a retry replays it.
func (c *core) saveResponse(ctx context.Context, tx port.Tx, operation, key string, response any) error {
	encoded, err := json.Marshal(response)
	if err != nil {
		return errs.Internal("failed to encode the idempotent response").WithCause(err)
	}
	return c.deps.Idempotency.SaveResponse(ctx, tx, key, operation, encoded)
}

// replay decodes a stored idempotent response into target.
func (c *core) replay(existing *port.IdempotencyRecord, target any) error {
	if err := json.Unmarshal(existing.Response, target); err != nil {
		return errs.Internal("failed to decode a stored idempotent response").WithCause(err)
	}
	return nil
}

// authoriseMovement checks that the caller may move money in the target wallet.
func (c *core) authoriseMovement(ctx context.Context, userID string) error {
	principal, err := authn.RequirePrincipal(ctx)
	if err != nil {
		return err
	}
	// A user may spend from their own wallet; a service may act on their behalf
	// inside a saga; staff may make an audited correction. Nobody else.
	if principal.UserID == userID || principal.IsService() || principal.IsStaff() {
		return nil
	}
	return errs.PermissionDenied("you may only move money in your own wallet")
}

// actorFrom returns the principal's id for the audit trail, or "" when the work
// originated from an inbound event with no user attached.
func (c *core) actorFrom(ctx context.Context) string {
	if principal, ok := authn.PrincipalFrom(ctx); ok {
		return principal.UserID
	}
	return ""
}

// recordFailure updates the metrics for a rejected operation.
func (c *core) recordFailure(operation string, err error) {
	c.deps.Metrics.WalletOperation(operation, "failure")
	if reason := errs.ReasonOf(err); reason != "" {
		c.deps.Metrics.BusinessRuleRejection(reason)
	}
}

// recordSuccess updates the metrics for a completed money movement.
func (c *core) recordSuccess(operation string, direction wallet.Direction, reason wallet.Reason, amount money.Money) {
	c.deps.Metrics.WalletOperation(operation, "success")
	c.deps.Metrics.MoneyMoved(direction.String(), reason.String(), amount.Currency(), amount.Minor())
}

// recordReplay updates the metrics and logs for a short-circuited duplicate.
func (c *core) recordReplay(ctx context.Context, operation, key string) {
	c.deps.Metrics.IdempotentReplay(operation)
	logx.FromContext(ctx).Info("replayed an idempotent request",
		slog.String("operation", operation),
		slog.String("idempotency_key", key),
	)
}

func validateMovement(userID string, amount money.Money, reason wallet.Reason, idempotencyKey string) error {
	switch {
	case userID == "":
		return errs.InvalidArgument("a user id is required")
	case idempotencyKey == "":
		// Without a key a network retry would move money twice. This is refused
		// rather than defaulted: a generated key would make every retry look like a
		// brand new request, which is exactly the failure it is meant to prevent.
		return errs.InvalidArgument("an idempotency key is required for any operation that moves money")
	case !amount.IsPositive():
		return wallet.ErrAmountNotPositive(amount)
	case !reason.Valid():
		return errs.InvalidArgument("unknown ledger reason %q", reason)
	}
	return nil
}

// hashRequest fingerprints a command so that reusing an idempotency key for a
// different payload can be detected.
func hashRequest(request any) string {
	encoded, err := json.Marshal(request)
	if err != nil {
		// An unmarshallable command cannot be fingerprinted. Return a value that can
		// never match a stored hash, so the mismatch is reported rather than ignored.
		return fmt.Sprintf("unhashable:%T", request)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// paging converts a 1-based page and size into a limit and offset, clamping the
// size so that a client cannot ask for an entire ledger in one response.
func paging(page, pageSize int) (limit, offset int) {
	if pageSize <= 0 {
		pageSize = defaultPageSize
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}
	if page <= 0 {
		page = 1
	}
	return pageSize, (page - 1) * pageSize
}

// asService attaches a service principal for work that originates from an inbound
// event rather than from a user request.
func asService(ctx context.Context) context.Context {
	if _, ok := authn.PrincipalFrom(ctx); ok {
		return ctx
	}
	return authn.WithPrincipal(ctx, authn.Principal{
		UserID: "wallet-service",
		Role:   authn.RoleService,
	})
}
