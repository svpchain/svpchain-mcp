package tools

import (
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
