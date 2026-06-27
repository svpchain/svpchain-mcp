package limits

import (
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"
)

// This file implements the per-symbol daily "transfer out" cap (svp / usdc /
// usdv). Funds leave a wallet through two rails — x/bank sends and EVM
// transfers — and one symbol (usdc is both an x/bank denom and an ERC-20) can
// leave through either, so caps and usage are keyed by symbol and sum across
// both rails. Caps and usage are keyed by the owner wallet address (not the
// per-auth tenant id), so all of a wallet's concurrent agents / re-auths share
// one cap and one daily total. Caps are set at runtime (set_transfer_out_cap);
// there is no operator config. Usage is metered per UTC day and resets at
// midnight; caps persist until changed.
//
// The package stays a dependency-free leaf: amounts are math/big base units,
// no SDK / go-ethereum imports. The symbol<->rail registry lives in the tools
// layer, which converts denoms / token addresses to a symbol before calling in.

// SymbolCap is one token's daily cap: the limit in base units plus the symbol
// and decimals needed to render human error / display strings.
type SymbolCap struct {
	Symbol   string
	Decimals int64
	CapBase  *big.Int
}

// ErrSymbolCapExceeded is the typed error returned when a transfer-out would
// exceed the cap. Amounts are rendered human (decimal-adjusted, trailing zeros
// trimmed) so the message matches the operator/agent's intuition.
type ErrSymbolCapExceeded struct {
	Symbol    string
	Limit     string
	Requested string
	Used      string
}

func (e *ErrSymbolCapExceeded) Error() string {
	return fmt.Sprintf(
		"daily_transfer_out_cap exceeded for %s: requested %s + used %s > limit %s %s",
		e.Symbol, e.Requested, e.Used, e.Limit, strings.ToUpper(e.Symbol),
	)
}

// MemoryTransferOutStore holds each owner wallet's per-symbol daily caps and
// usage in one place. Caps persist until the owner changes them; the usage
// tally resets at UTC midnight. Safe for concurrent use; in-memory only (state
// resets on restart). A nil *MemoryTransferOutStore is usable and treats
// everything as uncapped — the methods short-circuit.
type MemoryTransferOutStore struct {
	now func() time.Time

	mu   sync.Mutex
	day  string                          // UTC date the `used` map is valid for
	caps map[string]map[string]SymbolCap // owner -> symbol -> finite cap
	used map[string]map[string]*big.Int  // owner -> symbol -> base units today
}

// NewMemoryTransferOutStore returns an empty store (every symbol uncapped until
// an owner sets a cap). Pass time.Now for wall-clock; tests inject a fake clock.
func NewMemoryTransferOutStore(now func() time.Time) *MemoryTransferOutStore {
	if now == nil {
		now = time.Now
	}
	return &MemoryTransferOutStore{
		now:  now,
		caps: map[string]map[string]SymbolCap{},
		used: map[string]map[string]*big.Int{},
	}
}

// SetCap sets a finite daily cap for the owner's symbol (keyed by c.Symbol).
func (s *MemoryTransferOutStore) SetCap(owner string, c SymbolCap) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	bySym := s.caps[owner]
	if bySym == nil {
		bySym = map[string]SymbolCap{}
		s.caps[owner] = bySym
	}
	bySym[c.Symbol] = c
}

// SetUnlimited removes any cap for the owner's symbol (uncapped).
func (s *MemoryTransferOutStore) SetUnlimited(owner, symbol string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.caps[owner], symbol)
}

// Cap returns the owner's finite cap for the symbol, if one is set.
func (s *MemoryTransferOutStore) Cap(owner, symbol string) (SymbolCap, bool) {
	if s == nil {
		return SymbolCap{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.caps[owner][symbol]
	return c, ok
}

// Used returns a copy of the base units the owner has transferred out of the
// symbol so far in the current UTC day.
func (s *MemoryTransferOutStore) Used(owner, symbol string) *big.Int {
	if s == nil {
		return big.NewInt(0)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rolloverLocked()
	if v := s.used[owner][symbol]; v != nil {
		return new(big.Int).Set(v)
	}
	return big.NewInt(0)
}

// Record adds to today's usage; called only after a successful broadcast so a
// rejected tx doesn't eat the cap. Non-positive amounts are ignored.
func (s *MemoryTransferOutStore) Record(owner, symbol string, amt *big.Int) {
	if s == nil || amt == nil || amt.Sign() <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rolloverLocked()
	bySym := s.used[owner]
	if bySym == nil {
		bySym = map[string]*big.Int{}
		s.used[owner] = bySym
	}
	cur := bySym[symbol]
	if cur == nil {
		cur = big.NewInt(0)
	}
	bySym[symbol] = new(big.Int).Add(cur, amt)
}

// Check rejects a transfer-out of amt base units of symbol when it would push
// the owner's running daily total past the cap. Uncapped symbols and
// non-positive amounts pass; a nil store passes (feature inert).
func (s *MemoryTransferOutStore) Check(owner, symbol string, amt *big.Int) error {
	if s == nil || amt == nil || amt.Sign() <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rolloverLocked()
	c, ok := s.caps[owner][symbol]
	if !ok || c.CapBase == nil {
		return nil
	}
	used := big.NewInt(0)
	if v := s.used[owner][symbol]; v != nil {
		used = v
	}
	if new(big.Int).Add(used, amt).Cmp(c.CapBase) > 0 {
		return &ErrSymbolCapExceeded{
			Symbol:    c.Symbol,
			Limit:     baseToHuman(c.CapBase, c.Decimals),
			Requested: baseToHuman(amt, c.Decimals),
			Used:      baseToHuman(used, c.Decimals),
		}
	}
	return nil
}

func (s *MemoryTransferOutStore) rolloverLocked() {
	today := s.now().UTC().Format("2006-01-02")
	if s.day == today {
		return
	}
	s.day = today
	for k := range s.used {
		delete(s.used, k)
	}
}

// baseToHuman renders a base-unit integer as a decimal string with `decimals`
// fractional places, trailing zeros trimmed: baseToHuman(1_500_000, 6) -> "1.5".
func baseToHuman(base *big.Int, decimals int64) string {
	if base == nil {
		return "0"
	}
	if decimals <= 0 {
		return base.String()
	}
	neg := base.Sign() < 0
	abs := new(big.Int).Abs(base)
	divisor := new(big.Int).Exp(big.NewInt(10), big.NewInt(decimals), nil)
	whole := new(big.Int)
	frac := new(big.Int)
	whole.QuoRem(abs, divisor, frac)

	out := whole.String()
	if frac.Sign() > 0 {
		fracStr := frac.String()
		for int64(len(fracStr)) < decimals {
			fracStr = "0" + fracStr
		}
		fracStr = strings.TrimRight(fracStr, "0")
		out = out + "." + fracStr
	}
	if neg {
		out = "-" + out
	}
	return out
}
