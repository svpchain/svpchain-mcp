package builder

import (
	"context"
	"fmt"
	"math/big"
	"strconv"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/chain"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/payload"
)

// Gas estimates can be slightly tight (and the contract's gas use can shift
// between estimate and inclusion), so we pad EstimateGas by 25%. Mirrors the
// headroom cast/foundry applies by default.
const (
	evmGasBufferNum = 5
	evmGasBufferDen = 4
)

// EVMAssembler is the contract-agnostic core of the EVM build path. Given a
// destination + calldata produced by a per-contract builder (e.g. a future
// swap), it queries the EVM client for the chain-derived fields
// (nonce/gas/fee-caps/chainID) and stamps a ready-to-sign EVMTxPayload.
//
// This is the seam that keeps new contracts cheap: a per-contract builder only
// has to pack (to, data, value); everything chain-aware lives here, written
// once.
type EVMAssembler struct {
	evm chain.EVMClient
}

// NewEVMAssembler binds an assembler to an EVM JSON-RPC client.
func NewEVMAssembler(evm chain.EVMClient) *EVMAssembler {
	return &EVMAssembler{evm: evm}
}

// EVMArgs bundles the per-build inputs the per-contract layer supplies.
type EVMArgs struct {
	ClientID string         // broadcast-idempotency uuid
	From     common.Address // tenant owner's 0x address (the sender)
	To       common.Address // target contract
	Data     []byte         // ABI-encoded calldata
	Value    *big.Int       // native value; nil == 0
	Summary  payload.EVMSummary
}

// Assemble builds an EIP-1559 EVMTxPayload: pending nonce for From, a gas
// estimate (padded), EIP-1559 fee caps derived from the latest base fee + a
// suggested tip, and the numeric chain id. Returns an error if any RPC call
// fails so the caller surfaces a clean "couldn't build" rather than a payload
// with missing fields.
func (a *EVMAssembler) Assemble(ctx context.Context, args EVMArgs) (*payload.EVMTxPayload, error) {
	value := args.Value
	if value == nil {
		value = big.NewInt(0)
	}

	chainID, err := a.evm.ChainID(ctx)
	if err != nil {
		return nil, fmt.Errorf("evm chain id: %w", err)
	}
	nonce, err := a.evm.PendingNonceAt(ctx, args.From)
	if err != nil {
		return nil, fmt.Errorf("pending nonce for %s: %w", args.From.Hex(), err)
	}
	tip, err := a.evm.SuggestGasTipCap(ctx)
	if err != nil {
		return nil, fmt.Errorf("suggest gas tip cap: %w", err)
	}
	baseFee, err := a.evm.BaseFee(ctx)
	if err != nil {
		return nil, fmt.Errorf("base fee: %w", err)
	}
	// EIP-1559 max fee: cover up to 2x base-fee growth over the next blocks
	// plus the priority tip. Standard wallet heuristic.
	maxFee := new(big.Int).Add(new(big.Int).Mul(baseFee, big.NewInt(2)), tip)

	gas, err := a.evm.EstimateGas(ctx, ethereum.CallMsg{
		From:      args.From,
		To:        &args.To,
		Value:     value,
		Data:      args.Data,
		GasFeeCap: maxFee,
		GasTipCap: tip,
	})
	if err != nil {
		return nil, fmt.Errorf("estimate gas: %w", err)
	}
	gas = gas * evmGasBufferNum / evmGasBufferDen

	return &payload.EVMTxPayload{
		Version:              payload.CurrentVersion,
		ClientID:             args.ClientID,
		EVMChainID:           chainID.String(),
		SignerAddress:        args.From.Hex(),
		TxType:               payload.EVMTxTypeEIP1559,
		To:                   args.To.Hex(),
		Nonce:                strconv.FormatUint(nonce, 10),
		Gas:                  strconv.FormatUint(gas, 10),
		MaxFeePerGas:         maxFee.String(),
		MaxPriorityFeePerGas: tip.String(),
		Value:                value.String(),
		Data:                 hexutil.Encode(args.Data),
		ExpiresAt:            time.Now().UTC().Add(DefaultPayloadTTL),
		Summary:              args.Summary,
	}, nil
}
