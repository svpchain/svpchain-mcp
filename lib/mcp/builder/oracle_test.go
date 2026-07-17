package builder_test

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"

	"github.com/svpchain/svpchain-mcp/lib/mcp/builder"
)

var testOracleAddr = common.HexToAddress("0xAE351F2dF66DF1A7d2eB0D7574BcDb909E680B56")

func newOracle(t *testing.T) *builder.OracleFeed {
	t.Helper()
	o, err := builder.NewOracleFeed(testOracleAddr)
	require.NoError(t, err)
	return o
}

func TestOracleFeed_Address(t *testing.T) {
	require.Equal(t, testOracleAddr, newOracle(t).Address())
}

func TestOracleFeed_Selectors(t *testing.T) {
	o := newOracle(t)

	dec, err := o.PackDecimals()
	require.NoError(t, err)
	require.Equal(t, crypto.Keccak256([]byte("decimals()"))[:4], dec[:4])

	desc, err := o.PackDescription()
	require.NoError(t, err)
	require.Equal(t, crypto.Keccak256([]byte("description()"))[:4], desc[:4])

	rd, err := o.PackLatestRoundData()
	require.NoError(t, err)
	require.Equal(t, crypto.Keccak256([]byte("latestRoundData()"))[:4], rd[:4])
}

func TestOracleFeed_UnpackDecimals(t *testing.T) {
	o := newOracle(t)
	d, err := o.UnpackDecimals(common.LeftPadBytes(big.NewInt(8).Bytes(), 32))
	require.NoError(t, err)
	require.Equal(t, uint8(8), d)
}

func TestOracleFeed_UnpackDescription(t *testing.T) {
	o := newOracle(t)
	typ, err := abi.NewType("string", "", nil)
	require.NoError(t, err)
	encoded, err := abi.Arguments{{Type: typ}}.Pack("BTC / USD")
	require.NoError(t, err)

	got, err := o.UnpackDescription(encoded)
	require.NoError(t, err)
	require.Equal(t, "BTC / USD", got)
}

// TestOracleFeed_UnpackLatestRoundData roundtrips the (uint80,int256,uint256,
// uint256,uint80) tuple through the ABI output encoding, proving the unpacker
// pulls answer/round/updatedAt from the right tuple positions.
func TestOracleFeed_UnpackLatestRoundData(t *testing.T) {
	o := newOracle(t)

	u80, err := abi.NewType("uint80", "", nil)
	require.NoError(t, err)
	i256, err := abi.NewType("int256", "", nil)
	require.NoError(t, err)
	u256, err := abi.NewType("uint256", "", nil)
	require.NoError(t, err)
	args := abi.Arguments{{Type: u80}, {Type: i256}, {Type: u256}, {Type: u256}, {Type: u80}}

	roundID := big.NewInt(42)
	answer := big.NewInt(6_523_412_000_000) // e.g. 65234.12 at 8 decimals
	updatedAt := big.NewInt(1_700_000_000)
	encoded, err := args.Pack(roundID, answer, big.NewInt(1_699_999_000), updatedAt, roundID)
	require.NoError(t, err)

	rd, err := o.UnpackLatestRoundData(encoded)
	require.NoError(t, err)
	require.Equal(t, roundID, rd.RoundID)
	require.Equal(t, answer, rd.Answer)
	require.Equal(t, updatedAt, rd.UpdatedAt)
}
