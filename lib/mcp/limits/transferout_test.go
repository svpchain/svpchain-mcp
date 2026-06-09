package limits

import (
	"errors"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// bi parses a base-10 integer into a *big.Int for test readability.
func bi(s string) *big.Int {
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic("bad big.Int literal: " + s)
	}
	return n
}

func usdcCap(base string) SymbolCap {
	return SymbolCap{Symbol: "usdc", Decimals: 6, CapBase: bi(base)}
}

func TestBaseToHuman(t *testing.T) {
	require.Equal(t, "1.5", baseToHuman(bi("1500000"), 6))
	require.Equal(t, "500", baseToHuman(bi("500000000"), 6))
	require.Equal(t, "0.000001", baseToHuman(bi("1"), 6))
	require.Equal(t, "10", baseToHuman(bi("10000000000000000000"), 18))
	require.Equal(t, "0", baseToHuman(big.NewInt(0), 6))
	require.Equal(t, "42", baseToHuman(big.NewInt(42), 0))
}

func TestTransferOutStore_CapLifecycle(t *testing.T) {
	s := NewMemoryTransferOutStore(time.Now)

	t.Run("uncapped until set", func(t *testing.T) {
		_, ok := s.Cap("t1", "usdc")
		require.False(t, ok)
		require.NoError(t, s.Check("t1", "usdc", bi("999999999999")))
	})

	t.Run("set then enforced", func(t *testing.T) {
		s.SetCap("t1", usdcCap("500000000")) // 500
		require.NoError(t, s.Check("t1", "usdc", bi("500000000")))
		err := s.Check("t1", "usdc", bi("500000001"))
		require.Error(t, err)
		var ce *ErrSymbolCapExceeded
		require.True(t, errors.As(err, &ce))
		require.Equal(t, "usdc", ce.Symbol)
		require.Equal(t, "500", ce.Limit)
		require.Equal(t, "500.000001", ce.Requested)
	})

	t.Run("raise cap — no ceiling", func(t *testing.T) {
		s.SetCap("t1", usdcCap("1000000000")) // 1000
		require.NoError(t, s.Check("t1", "usdc", bi("900000000")))
	})

	t.Run("set unlimited clears the cap", func(t *testing.T) {
		s.SetUnlimited("t1", "usdc")
		_, ok := s.Cap("t1", "usdc")
		require.False(t, ok)
		require.NoError(t, s.Check("t1", "usdc", bi("999999999999")))
	})

	t.Run("non-positive amount and nil store pass", func(t *testing.T) {
		s.SetCap("t1", usdcCap("1"))
		require.NoError(t, s.Check("t1", "usdc", big.NewInt(0)))
		var nilStore *MemoryTransferOutStore
		require.NoError(t, nilStore.Check("t1", "usdc", bi("999")))
	})
}

func TestTransferOutStore_UsageAndIsolation(t *testing.T) {
	s := NewMemoryTransferOutStore(time.Now)
	s.SetCap("t1", usdcCap("500000000"))

	// Spend to the cap, cumulative.
	require.NoError(t, s.Check("t1", "usdc", bi("300000000")))
	s.Record("t1", "usdc", bi("300000000"))
	require.NoError(t, s.Check("t1", "usdc", bi("200000000")))
	s.Record("t1", "usdc", bi("200000000"))

	err := s.Check("t1", "usdc", bi("1"))
	require.Error(t, err)
	var ce *ErrSymbolCapExceeded
	require.True(t, errors.As(err, &ce))
	require.Equal(t, "500", ce.Used)

	// A different tenant's usage + cap are independent.
	require.NoError(t, s.Check("t2", "usdc", bi("999999999")))
	require.EqualValues(t, 0, s.Used("t2", "usdc").Int64())

	// svp is a separate symbol, unaffected by usdc usage.
	require.EqualValues(t, 0, s.Used("t1", "svp").Int64())
}

func TestTransferOutStore_UTCRollover(t *testing.T) {
	clk := time.Date(2026, 5, 28, 23, 59, 0, 0, time.UTC)
	s := NewMemoryTransferOutStore(func() time.Time { return clk })
	s.SetCap("t1", usdcCap("500000000"))

	s.Record("t1", "usdc", bi("500000000"))
	require.Error(t, s.Check("t1", "usdc", bi("1")))

	// Cross UTC midnight — usage resets, the cap persists.
	clk = clk.Add(2 * time.Minute)
	require.EqualValues(t, 0, s.Used("t1", "usdc").Int64())
	require.NoError(t, s.Check("t1", "usdc", bi("500000000")))
	_, ok := s.Cap("t1", "usdc")
	require.True(t, ok, "cap should survive a day rollover")
}

func TestTransferOutStore_UsedReturnsCopy(t *testing.T) {
	s := NewMemoryTransferOutStore(time.Now)
	s.Record("t1", "usdc", bi("100"))
	got := s.Used("t1", "usdc")
	got.Add(got, bi("999"))
	require.Equal(t, bi("100"), s.Used("t1", "usdc"))
}

func TestTransferOutStore_ConcurrentRecord(t *testing.T) {
	const goroutines = 100
	const perGoroutine = 10
	s := NewMemoryTransferOutStore(time.Now)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range perGoroutine {
				s.Record("t1", "usdc", big.NewInt(1))
			}
		}()
	}
	wg.Wait()
	require.EqualValues(t, goroutines*perGoroutine, s.Used("t1", "usdc").Int64())
}
