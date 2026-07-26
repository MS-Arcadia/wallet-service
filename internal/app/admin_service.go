package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/MS-Arcadia/arcadia-platform/pkg/authn"
	"github.com/MS-Arcadia/arcadia-platform/pkg/errs"
	"github.com/MS-Arcadia/arcadia-platform/pkg/logx"
	"github.com/MS-Arcadia/arcadia-platform/pkg/money"
	"github.com/MS-Arcadia/wallet-service/internal/app/port"
	"github.com/MS-Arcadia/wallet-service/internal/domain/interest"
	"github.com/MS-Arcadia/wallet-service/internal/domain/wallet"
)

// reconcileBatchSize and accrualBatchSize page the maintenance jobs so that memory
// stays flat no matter how many wallets exist.
const (
	reconcileBatchSize = 500
	accrualBatchSize   = 200
)

// AdminService implements operational use cases: reconciliation, interest accrual,
// freezing and manual adjustment.
type AdminService struct {
	*core
	interestPolicy interest.Policy
}

// NewAdminService builds the service.
func NewAdminService(deps Deps, interestPolicy interest.Policy) *AdminService {
	return &AdminService{core: newCore(deps), interestPolicy: interestPolicy}
}

// AdjustCommand is a manual balance correction.
type AdjustCommand struct {
	UserID         string
	Direction      wallet.Direction
	Amount         money.Money
	Reason         string
	IdempotencyKey string
}

// AccrueInterestCommand runs one accrual cycle.
type AccrueInterestCommand struct {
	// AccrualDate defaults to today when empty. Re-running a past date is a no-op.
	AccrualDate string
	// AnnualRateBps overrides the configured rate, for replaying a historic run.
	AnnualRateBps int32
	// DryRun computes and reports without crediting anything.
	DryRun bool
}

// Reconcile verifies that every wallet balance equals the sum of its ledger.
//
// This is the service's most important safety net. The balance column is a cached
// projection of the ledger; if the two ever disagree, either a bug wrote a balance
// without an entry or somebody edited the database by hand. Either way it is a P1
// incident, so a mismatch is published as an event and surfaced as a gauge the
// alerting rules page on.
func (s *AdminService) Reconcile(ctx context.Context, userID string) (ReconcileResult, error) {
	if _, err := authn.RequireRole(ctx, authn.RoleAdmin, authn.RoleService); err != nil {
		return ReconcileResult{}, err
	}

	mismatches, err := s.deps.Ledger.FindMismatches(ctx, s.deps.Reader, userID, reconcileBatchSize)
	if err != nil {
		return ReconcileResult{}, err
	}

	checked, err := s.deps.Wallets.Count(ctx, s.deps.Reader)
	if err != nil {
		return ReconcileResult{}, err
	}
	if userID != "" {
		checked = 1
	}

	result := ReconcileResult{
		WalletsChecked: checked,
		Mismatches:     make([]ReconcileMismatch, 0, len(mismatches)),
	}
	for _, mismatch := range mismatches {
		result.Mismatches = append(result.Mismatches, ReconcileMismatch{
			WalletID:      mismatch.WalletID,
			UserID:        mismatch.UserID,
			StoredBalance: mismatch.StoredBalance,
			LedgerBalance: mismatch.LedgerBalance,
			Delta:         mismatch.Delta,
		})
	}

	s.deps.Metrics.LedgerMismatch(int64(len(result.Mismatches)))
	if len(result.Mismatches) == 0 {
		logx.FromContext(ctx).Info("reconciliation clean", slog.Int64("wallets_checked", checked))
		return result, nil
	}

	now := s.deps.Clock.Now()
	logx.FromContext(ctx).Error("LEDGER MISMATCH DETECTED — this is a P1 incident",
		slog.Int("mismatch_count", len(result.Mismatches)),
	)

	// Publishing each mismatch gives the audit pipeline a permanent record, which is
	// what makes the incident investigable after the fact.
	publishErr := s.deps.TxManager.WithinTx(ctx, func(ctx context.Context, tx port.Tx) error {
		for _, mismatch := range result.Mismatches {
			if err := s.emit(ctx, tx, EventLedgerMismatchDetected, aggregateWallet, mismatch.WalletID, now,
				LedgerMismatchPayload{
					WalletID:      mismatch.WalletID,
					UserID:        mismatch.UserID,
					StoredBalance: mismatch.StoredBalance,
					LedgerBalance: mismatch.LedgerBalance,
					Delta:         mismatch.Delta,
					DetectedAt:    now.Format(time.RFC3339),
				}); err != nil {
				return err
			}
		}
		return nil
	})
	if publishErr != nil {
		logx.FromContext(ctx).Error("failed to publish ledger mismatch events",
			slog.String("error", publishErr.Error()))
	} else {
		s.publisher.Notify()
	}

	// The mismatches are returned rather than raised as an error: the caller asked
	// for a report, and it got one. The alert comes from the gauge.
	return result, nil
}

// AccrueInterest credits one day of interest to every eligible wallet.
//
// Each wallet is its own transaction with its own idempotency key of
// interest:<wallet>:<date>. That combination is what makes the job safe to re-run
// after a crash: wallets already credited for the date are skipped, and the run
// picks up where it left off instead of paying everyone twice.
func (s *AdminService) AccrueInterest(ctx context.Context, cmd AccrueInterestCommand) (AccrueInterestResult, error) {
	if _, err := authn.RequireRole(ctx, authn.RoleAdmin, authn.RoleService); err != nil {
		return AccrueInterestResult{}, err
	}

	policy := s.interestPolicy
	if cmd.AnnualRateBps > 0 {
		overridden, err := policy.WithRate(int64(cmd.AnnualRateBps))
		if err != nil {
			return AccrueInterestResult{}, err
		}
		policy = overridden
	}

	accrualDate := interest.AccrualDate(s.deps.Clock.Now())
	if cmd.AccrualDate != "" {
		parsed, err := interest.ParseAccrualDate(cmd.AccrualDate)
		if err != nil {
			return AccrueInterestResult{}, err
		}
		accrualDate = parsed
	}

	result := AccrueInterestResult{
		TotalInterest: money.Zero(s.deps.Currency),
		AccrualDate:   accrualDate.Format("2006-01-02"),
		DryRun:        cmd.DryRun,
	}

	if !policy.Enabled() {
		logx.FromContext(ctx).Info("interest accrual is disabled; nothing to do")
		return result, nil
	}

	afterID := ""
	for {
		batch, err := s.deps.Wallets.ListActivePage(ctx, s.deps.Reader, afterID, accrualBatchSize)
		if err != nil {
			return result, err
		}
		if len(batch) == 0 {
			break
		}

		for _, candidate := range batch {
			afterID = candidate.ID()
			result.WalletsProcessed++

			accrual, err := policy.Calculate(candidate.Balance())
			if err != nil {
				logx.FromContext(ctx).Error("failed to calculate interest",
					slog.String("wallet_id", candidate.ID()),
					slog.String("error", err.Error()),
				)
				continue
			}
			if !accrual.Eligible || accrual.Amount.IsZero() {
				continue
			}
			if cmd.DryRun {
				result.WalletsCredited++
				if total, err := result.TotalInterest.Add(accrual.Amount); err == nil {
					result.TotalInterest = total
				}
				continue
			}

			credited, err := s.creditInterest(ctx, candidate.UserID(), accrual.Amount,
				policy.AnnualRateBps(), accrualDate)
			if err != nil {
				// One wallet failing must not abandon the rest of the run.
				logx.FromContext(ctx).Error("failed to credit interest",
					slog.String("wallet_id", candidate.ID()),
					slog.String("error", err.Error()),
				)
				continue
			}
			if credited {
				result.WalletsCredited++
				if total, err := result.TotalInterest.Add(accrual.Amount); err == nil {
					result.TotalInterest = total
				}
			}
		}

		if len(batch) < accrualBatchSize {
			break
		}
	}

	if result.WalletsCredited > 0 && !cmd.DryRun {
		s.publisher.Notify()
	}
	logx.FromContext(ctx).Info("interest accrual finished",
		slog.String("accrual_date", result.AccrualDate),
		slog.Int64("processed", result.WalletsProcessed),
		slog.Int64("credited", result.WalletsCredited),
		slog.String("total", result.TotalInterest.String()),
		slog.Bool("dry_run", cmd.DryRun),
	)
	return result, nil
}

// creditInterest credits one wallet, reporting false when the accrual for that
// wallet and date had already been applied.
func (s *AdminService) creditInterest(
	ctx context.Context,
	userID string,
	amount money.Money,
	annualRateBps int64,
	accrualDate time.Time,
) (bool, error) {
	credited := false

	err := s.deps.TxManager.WithinTx(ctx, func(ctx context.Context, tx port.Tx) error {
		w, err := s.deps.Wallets.LockByUserID(ctx, tx, userID)
		if err != nil {
			return err
		}
		key := interest.IdempotencyKey(w.ID(), accrualDate)

		claimed, _, err := s.claim(ctx, tx, opAccrueInterest, key, userID, key)
		if err != nil {
			return err
		}
		if !claimed {
			// Already accrued for this date. Nothing to do, and nothing wrong.
			return nil
		}

		versionAtLoad := w.Version()
		balanceBefore := w.Balance()
		now := s.deps.Clock.Now()

		movement, err := w.Credit(amount, wallet.ReasonInterest,
			accrualDate.Format("2006-01-02"), "daily interest", now)
		if err != nil {
			return err
		}
		if _, err := s.recordMovement(ctx, tx, w, movement, versionAtLoad, key, "system", now); err != nil {
			return err
		}
		if err := s.emit(ctx, tx, EventInterestAccrued, aggregateWallet, w.ID(), now, InterestAccruedPayload{
			WalletID:      w.ID(),
			UserID:        w.UserID(),
			Amount:        amount,
			AnnualRateBps: annualRateBps,
			AccrualDate:   accrualDate.Format("2006-01-02"),
			BalanceBefore: balanceBefore,
		}); err != nil {
			return err
		}

		credited = true
		return s.saveResponse(ctx, tx, opAccrueInterest, key, map[string]any{
			"credited_minor": amount.Minor(),
			"accrual_date":   accrualDate.Format("2006-01-02"),
		})
	})
	if err != nil {
		return false, err
	}
	return credited, nil
}

// FreezeWallet suspends all movement on a wallet. Staff only.
func (s *AdminService) FreezeWallet(ctx context.Context, userID, reason string) (WalletView, error) {
	principal, err := authn.RequireStaff(ctx)
	if err != nil {
		return WalletView{}, err
	}
	if userID == "" {
		return WalletView{}, errs.InvalidArgument("a user id is required")
	}
	if reason == "" {
		// Freezing locks a user out of their own money; an unexplained freeze is not
		// auditable and not defensible.
		return WalletView{}, errs.InvalidArgument("a reason is required to freeze a wallet")
	}
	return s.setStatus(ctx, userID, reason, principal.UserID, true)
}

// UnfreezeWallet restores normal operation. Staff only.
func (s *AdminService) UnfreezeWallet(ctx context.Context, userID, reason string) (WalletView, error) {
	principal, err := authn.RequireStaff(ctx)
	if err != nil {
		return WalletView{}, err
	}
	if userID == "" {
		return WalletView{}, errs.InvalidArgument("a user id is required")
	}
	return s.setStatus(ctx, userID, reason, principal.UserID, false)
}

func (s *AdminService) setStatus(ctx context.Context, userID, reason, actorID string, freeze bool) (WalletView, error) {
	var view WalletView

	err := s.deps.TxManager.WithinTx(ctx, func(ctx context.Context, tx port.Tx) error {
		w, err := s.deps.Wallets.LockByUserID(ctx, tx, userID)
		if err != nil {
			return err
		}
		versionAtLoad := w.Version()
		now := s.deps.Clock.Now()

		if freeze {
			err = w.Freeze(now)
		} else {
			err = w.Unfreeze(now)
		}
		if err != nil {
			return err
		}
		if err := s.deps.Wallets.Update(ctx, tx, w, versionAtLoad); err != nil {
			return err
		}

		eventType := EventWalletUnfrozen
		if freeze {
			eventType = EventWalletFrozen
		}
		if err := s.emit(ctx, tx, eventType, aggregateWallet, w.ID(), now, WalletStatusPayload{
			WalletID: w.ID(),
			UserID:   w.UserID(),
			Status:   string(w.Status()),
			Reason:   reason,
			ActorID:  actorID,
		}); err != nil {
			return err
		}

		view = newWalletView(w)
		return nil
	})
	if err != nil {
		return WalletView{}, err
	}

	s.publisher.Notify()
	action := "unfrozen"
	if freeze {
		action = "frozen"
	}
	logx.FromContext(ctx).Warn("wallet status changed by staff",
		slog.String("user_id", userID),
		slog.String("action", action),
		slog.String("actor_id", actorID),
		slog.String("reason", reason),
	)
	return view, nil
}

// Adjust applies a manual balance correction. Admin only.
//
// This exists because reality occasionally requires it — a failed refund, a
// duplicated credit found during reconciliation. It is the one operation that can
// change a balance without an upstream business event, so it demands a written
// justification and lands in the ledger as ADJUSTMENT attributed to the operator who
// made it.
func (s *AdminService) Adjust(ctx context.Context, cmd AdjustCommand) (TransactionResult, error) {
	principal, err := authn.RequireRole(ctx, authn.RoleAdmin)
	if err != nil {
		return TransactionResult{}, err
	}
	switch {
	case cmd.UserID == "":
		return TransactionResult{}, errs.InvalidArgument("a user id is required")
	case cmd.IdempotencyKey == "":
		return TransactionResult{}, errs.InvalidArgument("an idempotency key is required for an adjustment")
	case !cmd.Amount.IsPositive():
		return TransactionResult{}, wallet.ErrAmountNotPositive(cmd.Amount)
	case cmd.Reason == "":
		return TransactionResult{}, errs.InvalidArgument("a written justification is required for a manual adjustment")
	case cmd.Direction != wallet.DirectionDebit && cmd.Direction != wallet.DirectionCredit:
		return TransactionResult{}, errs.InvalidArgument("the direction must be DEBIT or CREDIT")
	}

	walletService := &WalletService{core: s.core}
	result, err := walletService.applyMovement(ctx, movementRequest{
		Operation:      opAdjust,
		UserID:         cmd.UserID,
		Amount:         cmd.Amount,
		Reason:         wallet.ReasonAdjustment,
		ReferenceID:    "adjustment:" + principal.UserID,
		Description:    cmd.Reason,
		IdempotencyKey: cmd.IdempotencyKey,
		Direction:      cmd.Direction,
	})
	if err != nil {
		return TransactionResult{}, err
	}

	logx.FromContext(ctx).Warn("manual balance adjustment applied",
		slog.String("user_id", cmd.UserID),
		slog.String("direction", cmd.Direction.String()),
		slog.String("amount", cmd.Amount.String()),
		slog.String("actor_id", principal.UserID),
		slog.String("justification", cmd.Reason),
	)
	return result, nil
}
