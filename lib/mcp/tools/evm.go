package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"

	sdk "github.com/cosmos/cosmos-sdk/types"
	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/payload"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/policy"
)

// This file holds the EVM tool family. The two tools below are
// contract-agnostic engine tools (written once, shared by every EVM contract);
// per-contract build_* tools (e.g. a future build_swap) live alongside them
// and call EVMAssembler.Assemble. ownerEthAddress is shared with the HTTP
// faucet tools (faucet.go) since both map a bech32 owner to its 0x address.

// ownerEthAddress converts a tenant's bech32 owner (svp1…) to its 0x EVM
// address. Both are the same 20 underlying bytes — the same identity the auth
// handshake recovers (see lib/mcp/auth/recover.go), just rendered as hex.
func ownerEthAddress(owner string) (common.Address, error) {
	acc, err := sdk.AccAddressFromBech32(owner)
	if err != nil {
		return common.Address{}, fmt.Errorf("parse owner %q: %w", owner, err)
	}
	return common.BytesToAddress(acc.Bytes()), nil
}

// -- broadcast_evm_tx --------------------------------------------------

type BroadcastEVMTxInput struct {
	ClientID string              `json:"client_id" jsonschema:"payload-level idempotency uuid (must match the EVMTxPayload.client_id that was signed)"`
	SignedTx payload.EVMSignedTx `json:"signed_tx"`
}

type BroadcastEVMTxOutput struct {
	TxHash string `json:"tx_hash"` // 0x hex
}

func (h *Handlers) BroadcastEVMTx(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in BroadcastEVMTxInput,
) (*mcp.CallToolResult, BroadcastEVMTxOutput, error) {
	tp, err := h.authorize(ctx, "broadcast_evm_tx")
	if err != nil {
		return nil, BroadcastEVMTxOutput{}, err
	}
	if h.Deps.Chain.EVM == nil {
		return nil, BroadcastEVMTxOutput{}, userErrf("EVM is not enabled on this server (no evm_rpc_url configured)")
	}
	if err := h.Deps.Idempotency.Claim(tp.TenantID, in.ClientID); err != nil {
		return nil, BroadcastEVMTxOutput{}, err
	}

	rawBytes, err := hexutil.Decode(in.SignedTx.RawTxHex)
	if err != nil {
		return nil, BroadcastEVMTxOutput{}, fmt.Errorf("decode raw_tx_hex: %w", err)
	}
	var tx ethtypes.Transaction
	if err := tx.UnmarshalBinary(rawBytes); err != nil {
		return nil, BroadcastEVMTxOutput{}, fmt.Errorf("decode signed evm tx: %w", err)
	}

	// Recover the sender from the signature and verify it matches the tenant
	// owner — the EVM analog of broadcast_signed_tx's signer/owner check.
	// Without this a tenant could submit a tx signed by some other key.
	from, err := ethtypes.Sender(ethtypes.LatestSignerForChainID(tx.ChainId()), &tx)
	if err != nil {
		return nil, BroadcastEVMTxOutput{}, fmt.Errorf("recover evm sender: %w", err)
	}
	ownerEth, err := ownerEthAddress(tp.Owner)
	if err != nil {
		return nil, BroadcastEVMTxOutput{}, err
	}
	if from != ownerEth {
		return nil, BroadcastEVMTxOutput{}, fmt.Errorf(
			"evm sender %s does not match tenant owner %s", from.Hex(), ownerEth.Hex())
	}

	// Per-symbol daily transfer-out cap (EVM rail): native-value sends and
	// direct ERC-20 transfers accumulate against the same per-tenant ledger as
	// x/bank sends, so e.g. usdc leaving via bank and via its ERC-20 share one
	// daily total. Recorded only after a successful send, below. The router /
	// WSVP addresses (zero when swaps are disabled) let the decoder exclude
	// swap/wrap legs.
	var router, wsvp common.Address
	if h.Deps.EVM.Uniswap != nil {
		router = h.Deps.EVM.Uniswap.Router()
		wsvp = h.Deps.EVM.Uniswap.WSVP()
	}
	transferOut := decodeTransferOut(tx.To(), tx.Value(), tx.Data(), ownerEth, router, wsvp)
	for sym, amt := range transferOut {
		if err := h.Deps.TransferOut.Check(tp.TenantID, sym, amt); err != nil {
			return nil, BroadcastEVMTxOutput{}, err
		}
	}

	txHash, sendErr := h.Deps.Chain.EVM.SendTransaction(ctx, &tx)
	outcome := "broadcast"
	reason := ""
	if sendErr != nil {
		outcome = "chain_reject"
		reason = sendErr.Error()
		txHash = tx.Hash().Hex() // hash is well-defined even if the node rejects it
	}
	_ = h.Deps.Auditor.Append(policy.AuditEntry{
		TenantID: tp.TenantID,
		Owner:    tp.Owner,
		Tool:     "broadcast_evm_tx",
		ClientID: in.ClientID,
		TxHash:   txHash,
		Outcome:  outcome,
		Reason:   reason,
	})
	if sendErr != nil {
		return nil, BroadcastEVMTxOutput{}, fmt.Errorf("broadcast evm tx: %w", sendErr)
	}
	// Spend only after the node accepts the tx — a rejected broadcast doesn't
	// eat the tenant's daily cap.
	for sym, amt := range transferOut {
		h.Deps.TransferOut.Record(tp.TenantID, sym, amt)
	}
	return nil, BroadcastEVMTxOutput{TxHash: txHash}, nil
}

// ERC-20 method selectors (first 4 bytes of keccak256 of the signature).
var (
	selERC20Transfer     = []byte{0xa9, 0x05, 0x9c, 0xbb} // transfer(address,uint256)
	selERC20TransferFrom = []byte{0x23, 0xb8, 0x72, 0xdd} // transferFrom(address,address,uint256)
)

// decodeTransferOut inspects a single signed EVM tx and returns the tenant's
// outbound amounts grouped by cap symbol: `svp` for a native value transfer,
// and a known token's symbol (usdc / usdv) for a direct ERC-20
// transfer/transferFrom to that token's contract.
//
// It matches ONLY top-level transfer/transferFrom calls and plain value sends.
// A Uniswap swap is a call to the router (which pulls tokens internally via its
// own transferFrom) and an approval uses the approve selector — neither is a
// top-level transfer to a known token, so swaps and approvals are not counted,
// honouring the "bank + EVM transfers, not swaps" scope. The router/WSVP guard
// additionally drops the native-value leg of a native→token swap or a wrap.
func decodeTransferOut(to *common.Address, value *big.Int, data []byte, owner, router, wsvp common.Address) map[string]*big.Int {
	out := map[string]*big.Int{}
	addOut := func(sym string, amt *big.Int) {
		if amt == nil || amt.Sign() <= 0 {
			return
		}
		if cur := out[sym]; cur != nil {
			out[sym] = new(big.Int).Add(cur, amt)
		} else {
			out[sym] = new(big.Int).Set(amt)
		}
	}

	// Native SVP value transfer — excluding sends to the router / WSVP, which
	// are the swap and wrap legs we intentionally don't cap.
	if value != nil && value.Sign() > 0 && to != nil && *to != router && *to != wsvp {
		if sym, ok := symbolForNative(); ok {
			addOut(sym, value)
		}
	}

	// Direct ERC-20 transfer / transferFrom to a known token contract.
	if to != nil && len(data) >= 4 {
		if sym, ok := symbolForToken(*to); ok {
			switch {
			case bytes.Equal(data[:4], selERC20Transfer) && len(data) >= 4+64:
				// transfer(to, amount): amount is the 2nd 32-byte word.
				addOut(sym, new(big.Int).SetBytes(data[4+32:4+64]))
			case bytes.Equal(data[:4], selERC20TransferFrom) && len(data) >= 4+96:
				// transferFrom(from, to, amount): count only the owner's own
				// outflow. `from` is the low 20 bytes of the 1st word.
				from := common.BytesToAddress(data[4+12 : 4+32])
				if from == owner {
					addOut(sym, new(big.Int).SetBytes(data[4+64:4+96]))
				}
			}
		}
	}
	return out
}

// -- evm_tx_status -----------------------------------------------------

type EVMTxStatusInput struct {
	TxHash string `json:"tx_hash" jsonschema:"0x hex tx hash returned by broadcast_evm_tx"`
}

type EVMTxStatusOutput struct {
	TxHash      string `json:"tx_hash"`
	Status      string `json:"status"` // "pending" | "success" | "failed"
	BlockNumber int64  `json:"block_number,omitempty"`
	GasUsed     uint64 `json:"gas_used,omitempty"`
}

func (h *Handlers) EVMTxStatus(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in EVMTxStatusInput,
) (*mcp.CallToolResult, EVMTxStatusOutput, error) {
	if _, err := h.authorize(ctx, "evm_tx_status"); err != nil {
		return nil, EVMTxStatusOutput{}, err
	}
	if h.Deps.Chain.EVM == nil {
		return nil, EVMTxStatusOutput{}, userErrf("EVM is not enabled on this server (no evm_rpc_url configured)")
	}
	receipt, err := h.Deps.Chain.EVM.TransactionReceipt(ctx, common.HexToHash(in.TxHash))
	if err != nil {
		// Not-yet-included is a legitimate empty result, not an error —
		// mirror the indexer client's NotFound handling.
		if errors.Is(err, ethereum.NotFound) {
			return nil, EVMTxStatusOutput{TxHash: in.TxHash, Status: "pending"}, nil
		}
		return nil, EVMTxStatusOutput{}, fmt.Errorf("evm receipt %s: %w", in.TxHash, err)
	}
	status := "failed"
	if receipt.Status == ethtypes.ReceiptStatusSuccessful {
		status = "success"
	}
	return nil, EVMTxStatusOutput{
		TxHash:      in.TxHash,
		Status:      status,
		BlockNumber: receipt.BlockNumber.Int64(),
		GasUsed:     receipt.GasUsed,
	}, nil
}
