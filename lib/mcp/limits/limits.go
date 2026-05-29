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
// Internally the package operates in USDC quantums (uint64); operator config
// is expressed in whole human USDC and converted to quantums at check time.
// That keeps the API precise (no ceiling-rounding bugs) while keeping the
// TOML readable.
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

// Config holds the operator-configured caps in whole human USDC. A zero
// value disables that cap (treated as +∞) — useful for dev/test configs.
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
//
// All fields are reported in human USDC (with up-to-6 decimals) so the
// error message matches operator intuition.
type ErrCapExceeded struct {
	Kind      string // "per_tx" or "daily"
	Tool      string // ToolDeposit / ToolWithdraw / ToolTransfer
	Limit     string // configured cap, e.g. "5000.000000"
	Requested string // amount the caller asked for, e.g. "100.500000"
	Used      string // (daily only) amount already spent today
}

func (e *ErrCapExceeded) Error() string {
	if e.Kind == "daily" {
		return fmt.Sprintf(
			"daily_withdraw_cap exceeded: requested %s USDC + used %s USDC > limit %s USDC",
			e.Requested, e.Used, e.Limit,
		)
	}
	return fmt.Sprintf(
		"%s_max_usdc exceeded: requested %s USDC > limit %s USDC",
		e.Tool, e.Requested, e.Limit,
	)
}

// CheckPerTx rejects a single-operation amount against the per-tool cap.
// Inputs are USDC quantums (uint64 atomic units); use HumanToQuantums to
// convert. A zero limit disables the check.
func CheckPerTx(cfg Config, tool string, quantums uint64) error {
	capQ, ok := perToolCapQuantums(cfg, tool)
	if !ok {
		return nil
	}
	if quantums > capQ {
		return &ErrCapExceeded{
			Kind:      "per_tx",
			Tool:      tool,
			Limit:     QuantumsToHuman(capQ),
			Requested: QuantumsToHuman(quantums),
		}
	}
	return nil
}

// perToolCapQuantums returns the per-tool cap as quantums plus a flag
// indicating whether the check is enabled. Zero-config = disabled; an
// overflow on the multiply (cap so large it doesn't fit in uint64) is also
// treated as disabled — operators don't get to set "≈ +∞" by accident.
func perToolCapQuantums(cfg Config, tool string) (uint64, bool) {
	var capUSDC uint64
	switch tool {
	case ToolDeposit:
		capUSDC = cfg.DepositMaxUSDC
	case ToolWithdraw:
		capUSDC = cfg.WithdrawMaxUSDC
	case ToolTransfer:
		capUSDC = cfg.TransferMaxUSDC
	default:
		return 0, false
	}
	if capUSDC == 0 {
		return 0, false
	}
	if capUSDC > ^uint64(0)/quantumsPerUSDC {
		return 0, false // overflow guard
	}
	return capUSDC * quantumsPerUSDC, true
}

// dailyCapQuantums is perToolCapQuantums's twin for the daily withdraw cap.
func dailyCapQuantums(cfg Config) (uint64, bool) {
	if cfg.DailyWithdrawCapUSDC == 0 {
		return 0, false
	}
	if cfg.DailyWithdrawCapUSDC > ^uint64(0)/quantumsPerUSDC {
		return 0, false
	}
	return cfg.DailyWithdrawCapUSDC * quantumsPerUSDC, true
}

// WithdrawLedger tracks how much a tenant has withdrawn in the current UTC
// day. Implementations must be safe for concurrent use. All amounts are in
// USDC quantums.
//
// Reserve is intentionally absent: the v0.2.3 design records a spend only
// after the chain accepts the broadcast (so a failed CheckTx doesn't eat
// cap). The trade-off is documented in the package doc — a withdraw that
// silently succeeds after a client-side timeout could be uncounted; a
// durable ledger with a reserve/commit two-phase API would close that gap.
type WithdrawLedger interface {
	// Remaining returns headroom under DailyWithdrawCapUSDC for the tenant,
	// in quantums.
	Remaining(tenantID string) uint64
	// Record commits a spend (in quantums); called only after a successful
	// broadcast.
	Record(tenantID string, quantums uint64)
}

// Enforce runs both per-tx and daily checks. It is the entry point for tool
// handlers. The withdraw daily check only fires for ToolWithdraw and only
// when the ledger and cap are non-nil/non-zero. quantums is the requested
// amount in USDC atomic units.
func Enforce(cfg Config, ledger WithdrawLedger, tenantID, tool string, quantums uint64) error {
	if err := CheckPerTx(cfg, tool, quantums); err != nil {
		return err
	}
	if tool != ToolWithdraw || ledger == nil {
		return nil
	}
	dailyCapQ, ok := dailyCapQuantums(cfg)
	if !ok {
		return nil
	}
	remaining := ledger.Remaining(tenantID)
	if quantums > remaining {
		usedQ := dailyCapQ - remaining
		return &ErrCapExceeded{
			Kind:      "daily",
			Tool:      tool,
			Limit:     QuantumsToHuman(dailyCapQ),
			Requested: QuantumsToHuman(quantums),
			Used:      QuantumsToHuman(usedQ),
		}
	}
	return nil
}

// MemoryLedger is the default in-process WithdrawLedger. State resets on
// restart — acceptable for the current single-instance deployment, and the
// known limitation is documented in the package doc. Internal accounting
// is in quantums.
type MemoryLedger struct {
	capQuantums uint64 // DailyWithdrawCapUSDC × 10^6, captured at construction
	now         func() time.Time

	mu   sync.Mutex
	day  string            // UTC date "2006-01-02" of `used`'s validity
	used map[string]uint64 // tenant_id → quantums spent today
}

// NewMemoryLedger constructs a ledger pinned to the supplied daily cap
// (whole human USDC). Pass time.Now to use wall-clock time; tests inject a
// fake clock. A zero dailyCapUSDC creates a ledger whose Remaining always
// returns 0 (effectively unusable) — production callers should gate on
// `cfg.DailyWithdrawCapUSDC > 0` before constructing.
func NewMemoryLedger(dailyCapUSDC uint64, now func() time.Time) *MemoryLedger {
	if now == nil {
		now = time.Now
	}
	var capQ uint64
	if dailyCapUSDC > 0 && dailyCapUSDC <= ^uint64(0)/quantumsPerUSDC {
		capQ = dailyCapUSDC * quantumsPerUSDC
	}
	return &MemoryLedger{
		capQuantums: capQ,
		now:         now,
		used:        map[string]uint64{},
	}
}

// Remaining returns the tenant's headroom for the current UTC day (quantums),
// rolling the ledger over if the day has changed since the last call.
func (l *MemoryLedger) Remaining(tenantID string) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rolloverLocked()
	used := l.used[tenantID]
	if used >= l.capQuantums {
		return 0
	}
	return l.capQuantums - used
}

// Record adds a spent amount (quantums) to the tenant's daily total.
// Overflow on the per-tenant counter is clamped to MaxUint64 — practically
// unreachable but keeps the arithmetic well-defined.
func (l *MemoryLedger) Record(tenantID string, quantums uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rolloverLocked()
	cur := l.used[tenantID]
	sum := cur + quantums
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
