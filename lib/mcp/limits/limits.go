// Package limits enforces per-tool size caps and per-tenant daily withdraw
// limits for the MCP server's funds tools.
//
// Two enforcement points share this package:
//
//   - build_* tool handlers, which call Enforce before assembling a tx so
//     the caller gets a structured rejection without burning a sign.
//   - broadcast_signed_tx, which decodes the tx, finds any funds messages,
//     and runs Enforce again — guards against a caller that hand-crafts an
//     unsigned tx to bypass the build_* checks.
//
// Cap state is in-memory (MemoryLedger). Replacing it with a durable backend
// (postgres) only requires implementing the WithdrawLedger interface — no
// handler-side changes.
package limits

import (
	"fmt"
	"sync"
	"time"
)

// Config holds the operator-configured caps. A zero value for any field
// disables that cap (treated as +∞) — useful for dev/test configs.
type Config struct {
	DepositMaxUSDC       uint64
	WithdrawMaxUSDC      uint64
	TransferMaxUSDC      uint64
	DailyWithdrawCapUSDC uint64
}

// Tool names recognised by CheckPerTx and Enforce. Kept as constants so
// typos surface at compile time and the per-tool cap lookup stays explicit.
const (
	ToolDeposit  = "deposit"
	ToolWithdraw = "withdraw"
	ToolTransfer = "transfer"
)

// ErrCapExceeded is the typed error returned when a per-tx or daily cap
// rejects an operation. Tool handlers surface this so the caller sees which
// cap fired and what the headroom was, not an opaque "rejected" string.
type ErrCapExceeded struct {
	Kind      string // "per_tx" or "daily"
	Tool      string // ToolDeposit / ToolWithdraw / ToolTransfer
	Limit     uint64 // configured cap in human USDC
	Requested uint64 // amount the caller asked for, in human USDC
	Used      uint64 // (daily only) amount already spent today
}

func (e *ErrCapExceeded) Error() string {
	if e.Kind == "daily" {
		return fmt.Sprintf(
			"daily_withdraw_cap exceeded: requested %d USDC + used %d USDC > limit %d USDC",
			e.Requested, e.Used, e.Limit,
		)
	}
	return fmt.Sprintf(
		"%s_max_usdc exceeded: requested %d USDC > limit %d USDC",
		e.Tool, e.Requested, e.Limit,
	)
}

// CheckPerTx rejects a single-operation amount against the per-tool cap.
// A zero limit disables the check. Pure function — safe to call from any
// goroutine.
func CheckPerTx(cfg Config, tool string, humanUSDC uint64) error {
	limit := perToolLimit(cfg, tool)
	if limit == 0 {
		return nil
	}
	if humanUSDC > limit {
		return &ErrCapExceeded{Kind: "per_tx", Tool: tool, Limit: limit, Requested: humanUSDC}
	}
	return nil
}

func perToolLimit(cfg Config, tool string) uint64 {
	switch tool {
	case ToolDeposit:
		return cfg.DepositMaxUSDC
	case ToolWithdraw:
		return cfg.WithdrawMaxUSDC
	case ToolTransfer:
		return cfg.TransferMaxUSDC
	default:
		return 0
	}
}

// WithdrawLedger tracks how much a tenant has withdrawn in the current UTC
// day. Implementations must be safe for concurrent use.
//
// Reserve is intentionally absent: the v0.2.3 design records a spend only
// after the chain accepts the broadcast (so a failed CheckTx doesn't eat
// cap). The trade-off is documented in the package doc — a withdraw that
// silently succeeds after a client-side timeout could be uncounted; a
// durable ledger with a reserve/commit two-phase API would close that gap.
type WithdrawLedger interface {
	// Remaining returns headroom under DailyWithdrawCapUSDC for the tenant.
	Remaining(tenantID string) uint64
	// Record commits a spend; called only after a successful broadcast.
	Record(tenantID string, humanUSDC uint64)
}

// Enforce runs both per-tx and daily checks. It is the entry point for tool
// handlers. The withdraw daily check only fires for ToolWithdraw and only
// when the ledger and cap are non-nil/non-zero.
func Enforce(cfg Config, ledger WithdrawLedger, tenantID, tool string, humanUSDC uint64) error {
	if err := CheckPerTx(cfg, tool, humanUSDC); err != nil {
		return err
	}
	if tool != ToolWithdraw || ledger == nil || cfg.DailyWithdrawCapUSDC == 0 {
		return nil
	}
	remaining := ledger.Remaining(tenantID)
	if humanUSDC > remaining {
		used := cfg.DailyWithdrawCapUSDC - remaining
		return &ErrCapExceeded{
			Kind:      "daily",
			Tool:      tool,
			Limit:     cfg.DailyWithdrawCapUSDC,
			Requested: humanUSDC,
			Used:      used,
		}
	}
	return nil
}

// MemoryLedger is the default in-process WithdrawLedger. State resets on
// restart — acceptable for the current single-instance deployment, and the
// known limitation is documented in the package doc.
type MemoryLedger struct {
	cap uint64 // DailyWithdrawCapUSDC, captured at construction
	now func() time.Time

	mu    sync.Mutex
	day   string                  // UTC date "2006-01-02" of `used`'s validity
	used  map[string]uint64       // tenant_id → human USDC spent today
}

// NewMemoryLedger constructs a ledger pinned to the supplied daily cap.
// Pass time.Now to use wall-clock time; tests inject a fake clock.
func NewMemoryLedger(dailyCap uint64, now func() time.Time) *MemoryLedger {
	if now == nil {
		now = time.Now
	}
	return &MemoryLedger{
		cap:  dailyCap,
		now:  now,
		used: map[string]uint64{},
	}
}

// Remaining returns the tenant's headroom for the current UTC day, rolling
// the ledger over if the day has changed since the last call.
func (l *MemoryLedger) Remaining(tenantID string) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rolloverLocked()
	used := l.used[tenantID]
	if used >= l.cap {
		return 0
	}
	return l.cap - used
}

// Record adds a spent amount to the tenant's daily total. Overflow on the
// per-tenant counter is clamped to MaxUint64 — practically unreachable but
// keeps the arithmetic well-defined.
func (l *MemoryLedger) Record(tenantID string, humanUSDC uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rolloverLocked()
	cur := l.used[tenantID]
	sum := cur + humanUSDC
	if sum < cur { // overflow
		sum = ^uint64(0)
	}
	l.used[tenantID] = sum
}

func (l *MemoryLedger) rolloverLocked() {
	today := l.now().UTC().Format("2006-01-02")
	if l.day == today {
		return
	}
	l.day = today
	// Reset all tenants — preserving the map allocation is cheap and avoids
	// reallocating on every rollover.
	for k := range l.used {
		delete(l.used, k)
	}
}
