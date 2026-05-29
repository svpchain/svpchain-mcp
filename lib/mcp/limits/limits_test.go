package limits

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// USDC quantums shorthand for test readability — n whole USDC × 10^6.
func usdc(n uint64) uint64 { return n * 1_000_000 }

func TestCheckPerTx(t *testing.T) {
	cfg := Config{DepositMaxUSDC: 1000, WithdrawMaxUSDC: 500, TransferMaxUSDC: 2000}

	t.Run("under limit passes", func(t *testing.T) {
		require.NoError(t, CheckPerTx(cfg, ToolDeposit, usdc(999)))
		require.NoError(t, CheckPerTx(cfg, ToolWithdraw, usdc(500))) // boundary OK
	})

	t.Run("over limit rejects with typed error", func(t *testing.T) {
		// 500.000001 USDC = 500_000_001 quantums — just barely over the 500 cap.
		err := CheckPerTx(cfg, ToolWithdraw, usdc(500)+1)
		require.Error(t, err)
		var ce *ErrCapExceeded
		require.True(t, errors.As(err, &ce))
		require.Equal(t, "per_tx", ce.Kind)
		require.Equal(t, ToolWithdraw, ce.Tool)
		require.Equal(t, "500.000000", ce.Limit)
		require.Equal(t, "500.000001", ce.Requested)
	})

	t.Run("zero limit disables check", func(t *testing.T) {
		require.NoError(t, CheckPerTx(Config{}, ToolWithdraw, 1<<40))
	})

	t.Run("unknown tool — no per-tool cap, passes", func(t *testing.T) {
		require.NoError(t, CheckPerTx(cfg, "unknown", 1<<40))
	})
}

func TestEnforce_DailyCap(t *testing.T) {
	cfg := Config{WithdrawMaxUSDC: 10_000, DailyWithdrawCapUSDC: 5_000}
	clk := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	ledger := NewMemoryLedger(cfg.DailyWithdrawCapUSDC, func() time.Time { return clk })

	// 3000 withdraw — OK; ledger still empty so daily check passes.
	require.NoError(t, Enforce(cfg, ledger, "t1", ToolWithdraw, usdc(3_000)))
	ledger.Record("t1", usdc(3_000))

	// 2000 more — exactly at cap, OK.
	require.NoError(t, Enforce(cfg, ledger, "t1", ToolWithdraw, usdc(2_000)))
	ledger.Record("t1", usdc(2_000))

	// 0.000001 USDC over — rejected with daily kind.
	err := Enforce(cfg, ledger, "t1", ToolWithdraw, 1)
	require.Error(t, err)
	var ce *ErrCapExceeded
	require.True(t, errors.As(err, &ce))
	require.Equal(t, "daily", ce.Kind)
	require.Equal(t, "5000.000000", ce.Limit)
	require.Equal(t, "0.000001", ce.Requested)
	require.Equal(t, "5000.000000", ce.Used)

	// Different tenant — independent counter.
	require.NoError(t, Enforce(cfg, ledger, "t2", ToolWithdraw, usdc(4_999)))

	// Deposit isn't capped daily even when ledger is present.
	require.NoError(t, Enforce(cfg, ledger, "t1", ToolDeposit, 1))
}

func TestMemoryLedger_UTCRollover(t *testing.T) {
	cfg := Config{WithdrawMaxUSDC: 10_000, DailyWithdrawCapUSDC: 5_000}
	clk := time.Date(2026, 5, 28, 23, 59, 0, 0, time.UTC)
	ledger := NewMemoryLedger(cfg.DailyWithdrawCapUSDC, func() time.Time { return clk })

	ledger.Record("t1", usdc(5_000))
	require.EqualValues(t, 0, ledger.Remaining("t1"))

	// Cross UTC midnight.
	clk = clk.Add(2 * time.Minute)
	require.Equal(t, usdc(5_000), ledger.Remaining("t1"), "rollover should reset used")
	require.NoError(t, Enforce(cfg, ledger, "t1", ToolWithdraw, usdc(5_000)))
}

func TestMemoryLedger_ConcurrentRecord(t *testing.T) {
	// 100 goroutines × 10 records of 1 USDC each = 1000 USDC total. The mutex
	// must serialize the read-modify-write or this test races on the count.
	const goroutines = 100
	const perGoroutine = 10
	ledger := NewMemoryLedger(1_000_000, time.Now)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range perGoroutine {
				ledger.Record("t1", usdc(1))
			}
		}()
	}
	wg.Wait()

	used := usdc(1_000_000) - ledger.Remaining("t1")
	require.Equal(t, usdc(uint64(goroutines*perGoroutine)), used)
}

func TestEnforce_NilLedgerOrZeroCap_NoOp(t *testing.T) {
	cfg := Config{WithdrawMaxUSDC: 10_000} // DailyWithdrawCapUSDC = 0

	// No ledger configured.
	require.NoError(t, Enforce(cfg, nil, "t1", ToolWithdraw, usdc(10_000)))

	// Ledger present but daily cap is zero.
	ledger := NewMemoryLedger(0, time.Now)
	require.NoError(t, Enforce(cfg, ledger, "t1", ToolWithdraw, usdc(10_000)))
}

func TestPerToolCapQuantums_OverflowGuard(t *testing.T) {
	// A cap so absurd it overflows uint64 when × 10^6 — treat as disabled
	// rather than wrap around to a tiny number.
	cfg := Config{WithdrawMaxUSDC: ^uint64(0)}
	require.NoError(t, CheckPerTx(cfg, ToolWithdraw, ^uint64(0)))
}
