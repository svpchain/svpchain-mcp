package builder_test

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"

	"github.com/svpchain/svpchain-mcp/lib/mcp/builder"
)

var testBridgeAddr = common.HexToAddress("0x78Aca10afd5b28E838ECf0De20c5621CE39D9F4a")

func newBridge(t *testing.T) *builder.Bridge {
	t.Helper()
	b, err := builder.NewBridge(testBridgeAddr)
	require.NoError(t, err)
	return b
}

func TestBridge_Contract(t *testing.T) {
	require.Equal(t, testBridgeAddr, newBridge(t).Contract())
}

// The deposit selectors must match SVPBridge.sol's exact signatures — a wrong
// argument type (e.g. uint256 instead of uint64 for targetChainId) would
// compute a selector the contract never answers, so the tx would revert.
func TestBridge_DepositSelectors(t *testing.T) {
	b := newBridge(t)
	target := common.HexToAddress("0x1111111111111111111111111111111111111111")
	token := common.HexToAddress("0x2222222222222222222222222222222222222222")
	dest := common.HexToAddress("0x3333333333333333333333333333333333333333")

	dep, err := b.PackDeposit(token, big.NewInt(1), 11155111, target, dest)
	require.NoError(t, err)
	require.Equal(t,
		crypto.Keccak256([]byte("deposit(address,uint256,uint64,address,address)"))[:4],
		dep[:4],
	)

	depNative, err := b.PackDepositNative(421614, target, dest)
	require.NoError(t, err)
	require.Equal(t,
		crypto.Keccak256([]byte("depositNative(uint64,address,address)"))[:4],
		depNative[:4],
	)
}

func TestBridge_ERC20Allowance(t *testing.T) {
	owner := common.HexToAddress("0x4444444444444444444444444444444444444444")
	spender := common.HexToAddress("0x5555555555555555555555555555555555555555")
	data, err := builder.PackERC20Allowance(owner, spender)
	require.NoError(t, err)
	require.Equal(t, crypto.Keccak256([]byte("allowance(address,address)"))[:4], data[:4])

	// A uint256 result decodes back to the same big.Int.
	want := big.NewInt(123456789)
	wire := common.LeftPadBytes(want.Bytes(), 32)
	got, err := builder.UnpackERC20Allowance(wire)
	require.NoError(t, err)
	require.Equal(t, want, got)
}
