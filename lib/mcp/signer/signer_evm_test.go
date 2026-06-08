package signer_test

import (
	"testing"
	"time"

	"github.com/cosmos/evm/crypto/ethsecp256k1"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	ethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"

	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/payload"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/signer"
)

// ethAddrOf derives the 0x address of an eth_secp256k1 key the same way SignEVM
// does internally, so tests can set EVMTxPayload.SignerAddress consistently.
func ethAddrOf(t *testing.T, priv *ethsecp256k1.PrivKey) common.Address {
	t.Helper()
	key, err := ethcrypto.ToECDSA(priv.Key)
	require.NoError(t, err)
	return ethcrypto.PubkeyToAddress(key.PublicKey)
}

func newEVMPayload(signerAddr string) *payload.EVMTxPayload {
	return &payload.EVMTxPayload{
		Version:              payload.CurrentVersion,
		ClientID:             "evm-client-id",
		EVMChainID:           "262144",
		SignerAddress:        signerAddr,
		TxType:               payload.EVMTxTypeEIP1559,
		To:                   "0x000000000000000000000000000000000000dEaD",
		Nonce:                "7",
		Gas:                  "125000",
		MaxFeePerGas:         "5000000000",
		MaxPriorityFeePerGas: "1000000000",
		Value:                "0",
		Data:                 "0x4e71d92d", // arbitrary 4-byte selector
		ExpiresAt:            time.Now().UTC().Add(30 * time.Second),
		Summary:              payload.EVMSummary{ToolName: "build_evm_tx", Description: "evm tx"},
	}
}

func TestSignEVM_RoundTripRecoversSender(t *testing.T) {
	priv := newRandomPriv(t)
	from := ethAddrOf(t, priv)

	p := newEVMPayload(from.Hex())
	signed, err := signer.SignEVM(priv, p)
	require.NoError(t, err)
	require.NotEmpty(t, signed.RawTxHex)

	raw, err := hexutil.Decode(signed.RawTxHex)
	require.NoError(t, err)
	var tx ethtypes.Transaction
	require.NoError(t, tx.UnmarshalBinary(raw))

	// Recovered sender must equal the key's address — exactly what
	// broadcast_evm_tx checks against the tenant owner.
	recovered, err := ethtypes.Sender(ethtypes.LatestSignerForChainID(tx.ChainId()), &tx)
	require.NoError(t, err)
	require.Equal(t, from, recovered)

	// EIP-1559 fields survive the round-trip.
	require.Equal(t, uint8(ethtypes.DynamicFeeTxType), tx.Type())
	require.Equal(t, uint64(7), tx.Nonce())
	require.Equal(t, "262144", tx.ChainId().String())
	require.Equal(t, common.HexToAddress("0x000000000000000000000000000000000000dEaD"), *tx.To())
}

func TestSignEVM_RejectsAddressMismatch(t *testing.T) {
	priv := newRandomPriv(t)
	other := newRandomPriv(t)

	p := newEVMPayload(ethAddrOf(t, other).Hex())
	_, err := signer.SignEVM(priv, p)
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not match payload.signer_address")
}

func TestSignEVM_RejectsBadVersion(t *testing.T) {
	priv := newRandomPriv(t)
	p := newEVMPayload(ethAddrOf(t, priv).Hex())
	p.Version = payload.CurrentVersion + 1
	_, err := signer.SignEVM(priv, p)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported EVMTxPayload version")
}
