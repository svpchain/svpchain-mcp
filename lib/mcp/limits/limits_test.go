package limits

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCheckPerTx(t *testing.T) {
	cfg := Config{DepositMaxUSDC: 1000, WithdrawMaxUSDC: 500, TransferMaxUSDC: 2000}

	t.Run("under limit passes", func(t *testing.T) {
		require.NoError(t, CheckPerTx(cfg, ToolDeposit, 999))
		require.NoError(t, CheckPerTx(cfg, ToolWithdraw, 500)) // boundary OK
	})

	t.Run("over limit rejects with typed error", func(t *testing.T) {
		err := CheckPerTx(cfg, ToolWithdraw, 501)
		require.Error(t, err)
		var ce *ErrCapExceeded
		require.True(t, errors.As(err, &ce))
		require.Equal(t, "per_tx", ce.Kind)
		require.Equal(t, ToolWithdraw, ce.Tool)
		require.EqualValues(t, 500, ce.Limit)
		require.EqualValues(t, 501, ce.Requested)
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

	// First 3000 withdraw — OK; ledger still empty so daily check passes.
	require.NoError(t, Enforce(cfg, ledger, "t1", ToolWithdraw, 3_000))
	ledger.Record("t1", 3_000)

	// 2000 more — exactly at cap, OK.
	require.NoError(t, Enforce(cfg, ledger, "t1", ToolWithdraw, 2_000))
	ledger.Record("t1", 2_000)

	// 1 more — over cap, rejected with daily kind.
	err := Enforce(cfg, ledger, "t1", ToolWithdraw, 1)
	require.Error(t, err)
	var ce *ErrCapExceeded
	require.True(t, errors.As(err, &ce))
	require.Equal(t, "daily", ce.Kind)
	require.EqualValues(t, 5_000, ce.Limit)
	require.EqualValues(t, 1, ce.Requested)
	require.EqualValues(t, 5_000, ce.Used)

	// Different tenant — independent counter.
	require.NoError(t, Enforce(cfg, ledger, "t2", ToolWithdraw, 4_999))

	// Deposit isn't capped daily even when ledger is present.
	require.NoError(t, Enforce(cfg, ledger, "t1", ToolDeposit, 1))
}

func TestMemoryLedger_UTCRollover(t *testing.T) {
	cfg := Config{WithdrawMaxUSDC: 10_000, DailyWithdrawCapUSDC: 5_000}
	clk := time.Date(2026, 5, 28, 23, 59, 0, 0, time.UTC)
	ledger := NewMemoryLedger(cfg.DailyWithdrawCapUSDC, func() time.Time { return clk })

	ledger.Record("t1", 5_000)
	require.EqualValues(t, 0, ledger.Remaining("t1"))

	// Cross UTC midnight.
	clk = clk.Add(2 * time.Minute)
	require.EqualValues(t, 5_000, ledger.Remaining("t1"), "rollover should reset used")
	require.NoError(t, Enforce(cfg, ledger, "t1", ToolWithdraw, 5_000))
}

func TestMemoryLedger_ConcurrentRecord(t *testing.T) {
	// 100 goroutines × 10 records of 1 USDC each = 1000 total. The mutex
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
				ledger.Record("t1", 1)
			}
		}()
	}
	wg.Wait()

	used := 1_000_000 - ledger.Remaining("t1")
	require.EqualValues(t, goroutines*perGoroutine, used)
}

func TestEnforce_NilLedgerOrZeroCap_NoOp(t *testing.T) {
	cfg := Config{WithdrawMaxUSDC: 10_000} // DailyWithdrawCapUSDC = 0

	// No ledger configured.
	require.NoError(t, Enforce(cfg, nil, "t1", ToolWithdraw, 10_000))

	// Ledger present but daily cap is zero.
	ledger := NewMemoryLedger(0, time.Now)
	require.NoError(t, Enforce(cfg, ledger, "t1", ToolWithdraw, 10_000))
}
