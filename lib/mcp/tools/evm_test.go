package tools

import (
	"math/big"
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"
)

// The svp bech32 prefix is configured by app's init (imported via
// cross_test.go in this same test binary), so AccAddressFromBech32 on a
// "svp1…" owner resolves here.

func TestOwnerEthAddress_MapsToSameBytes(t *testing.T) {
	eth, err := ownerEthAddress(testTxOwner)
	require.NoError(t, err)

	acc, err := sdk.AccAddressFromBech32(testTxOwner)
	require.NoError(t, err)

	// The 0x address is exactly the bech32-decoded 20 bytes — the property
	// broadcast_evm_tx relies on to match a recovered EVM sender against the
	// tenant owner.
	require.Equal(t, common.BytesToAddress(acc.Bytes()), eth)
	require.Len(t, eth.Bytes(), 20)
}

func TestOwnerEthAddress_InvalidBech32(t *testing.T) {
	_, err := ownerEthAddress("not-a-valid-bech32-owner")
	require.Error(t, err)
}

func TestDecodeTransferOut(t *testing.T) {
	usdc, _ := assetForSymbol("usdc")
	usdcAddr := usdc.erc20
	owner := common.BytesToAddress([]byte{0x11})
	recipient := common.BytesToAddress([]byte{0x22})
	router := common.BytesToAddress([]byte{0x33})
	wsvp := common.BytesToAddress([]byte{0x44})

	transferData := func(to common.Address, amt *big.Int) []byte {
		d := append([]byte{}, selERC20Transfer...)
		d = append(d, common.LeftPadBytes(to.Bytes(), 32)...)
		return append(d, common.LeftPadBytes(amt.Bytes(), 32)...)
	}
	transferFromData := func(from, to common.Address, amt *big.Int) []byte {
		d := append([]byte{}, selERC20TransferFrom...)
		d = append(d, common.LeftPadBytes(from.Bytes(), 32)...)
		d = append(d, common.LeftPadBytes(to.Bytes(), 32)...)
		return append(d, common.LeftPadBytes(amt.Bytes(), 32)...)
	}

	t.Run("native value to EOA -> svp", func(t *testing.T) {
		out := decodeTransferOut(&recipient, big.NewInt(5), nil, owner, router, wsvp)
		require.Equal(t, "5", out["svp"].String())
	})

	t.Run("native value to router excluded (swap leg)", func(t *testing.T) {
		require.Empty(t, decodeTransferOut(&router, big.NewInt(5), nil, owner, router, wsvp))
	})

	t.Run("native value to WSVP excluded (wrap)", func(t *testing.T) {
		require.Empty(t, decodeTransferOut(&wsvp, big.NewInt(5), nil, owner, router, wsvp))
	})

	t.Run("erc20 transfer to known token -> symbol", func(t *testing.T) {
		out := decodeTransferOut(&usdcAddr, big.NewInt(0), transferData(recipient, big.NewInt(1_500_000)), owner, router, wsvp)
		require.Equal(t, "1500000", out["usdc"].String())
	})

	t.Run("transferFrom from owner counts", func(t *testing.T) {
		out := decodeTransferOut(&usdcAddr, big.NewInt(0), transferFromData(owner, recipient, big.NewInt(2_000_000)), owner, router, wsvp)
		require.Equal(t, "2000000", out["usdc"].String())
	})

	t.Run("transferFrom from a third party is ignored", func(t *testing.T) {
		out := decodeTransferOut(&usdcAddr, big.NewInt(0), transferFromData(recipient, owner, big.NewInt(2_000_000)), owner, router, wsvp)
		require.Empty(t, out)
	})

	t.Run("transfer to unknown token is uncapped", func(t *testing.T) {
		unknown := common.BytesToAddress([]byte{0x99})
		out := decodeTransferOut(&unknown, big.NewInt(0), transferData(recipient, big.NewInt(10)), owner, router, wsvp)
		require.Empty(t, out)
	})

	t.Run("approve to known token is not a transfer", func(t *testing.T) {
		approve := []byte{0x09, 0x5e, 0xa7, 0xb3} // approve(address,uint256)
		d := append(approve, common.LeftPadBytes(router.Bytes(), 32)...)
		d = append(d, common.LeftPadBytes(big.NewInt(100).Bytes(), 32)...)
		require.Empty(t, decodeTransferOut(&usdcAddr, big.NewInt(0), d, owner, router, wsvp))
	})

	t.Run("contract creation (nil to) is ignored", func(t *testing.T) {
		require.Empty(t, decodeTransferOut(nil, big.NewInt(5), nil, owner, router, wsvp))
	})
}
