package builder_test

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"

	"github.com/svpchain/svpchain-mcp/lib/mcp/builder"
)

var (
	ercTo      = common.HexToAddress("0x2222222222222222222222222222222222222222")
	ercFrom    = common.HexToAddress("0x3333333333333333333333333333333333333333")
	ercSpender = common.HexToAddress("0x4444444444444444444444444444444444444444")
)

// hex4 returns the lower-case hex of the 4-byte selector at the head of data.
func hex4(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 8)
	for i := 0; i < 4; i++ {
		out[i*2] = digits[b[i]>>4]
		out[i*2+1] = digits[b[i]&0xf]
	}
	return string(out)
}

func TestPackERC20_Selectors(t *testing.T) {
	tr, err := builder.PackERC20Transfer(ercTo, big.NewInt(1_000_000))
	require.NoError(t, err)
	require.Equal(t, "a9059cbb", hex4(tr))
	require.Len(t, tr, 4+64) // selector + 2 words

	ap, err := builder.PackERC20Approve(ercSpender, big.NewInt(0))
	require.NoError(t, err)
	require.Equal(t, "095ea7b3", hex4(ap))
	require.Len(t, ap, 4+64)

	tf, err := builder.PackERC20TransferFrom(ercFrom, ercTo, big.NewInt(42))
	require.NoError(t, err)
	require.Equal(t, "23b872dd", hex4(tf))
	require.Len(t, tf, 4+96) // selector + 3 words
}

func TestPackERC20_TransferEncoding(t *testing.T) {
	// transfer(to, 0xf4240): recipient in word 1, amount in word 2.
	data, err := builder.PackERC20Transfer(ercTo, big.NewInt(1_000_000))
	require.NoError(t, err)
	require.Equal(t, ercTo, common.BytesToAddress(data[4:4+32]))
	require.Equal(t, big.NewInt(1_000_000), new(big.Int).SetBytes(data[4+32:4+64]))
}

func TestPackERC20Decimals_RoundTrip(t *testing.T) {
	data, err := builder.PackERC20Decimals()
	require.NoError(t, err)
	require.Equal(t, "313ce567", hex4(data))

	// Encode an on-chain decimals() return of 6 and decode it back.
	ret := common.LeftPadBytes(big.NewInt(6).Bytes(), 32)
	dec, err := builder.UnpackERC20Decimals(ret)
	require.NoError(t, err)
	require.Equal(t, uint8(6), dec)
}

func TestPackERC721_Selectors(t *testing.T) {
	tf, err := builder.PackERC721TransferFrom(ercFrom, ercTo, big.NewInt(7))
	require.NoError(t, err)
	require.Equal(t, "23b872dd", hex4(tf))

	safe, err := builder.PackERC721SafeTransferFrom(ercFrom, ercTo, big.NewInt(7))
	require.NoError(t, err)
	require.Equal(t, "42842e0e", hex4(safe))
	require.Len(t, safe, 4+96)

	ap, err := builder.PackERC721Approve(ercSpender, big.NewInt(7))
	require.NoError(t, err)
	require.Equal(t, "095ea7b3", hex4(ap))

	grant, err := builder.PackERC721SetApprovalForAll(ercSpender, true)
	require.NoError(t, err)
	require.Equal(t, "a22cb465", hex4(grant))
	// bool true => last word's low byte is 1.
	require.Equal(t, byte(1), grant[len(grant)-1])

	revoke, err := builder.PackERC721SetApprovalForAll(ercSpender, false)
	require.NoError(t, err)
	require.Equal(t, byte(0), revoke[len(revoke)-1])
}
