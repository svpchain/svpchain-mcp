package tools

import (
	"context"
	"errors"
	"fmt"

	sdk "github.com/cosmos/cosmos-sdk/types"
	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/builder"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/payload"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/policy"
)

// This file holds the EVM tool family. The two tools below are
// contract-agnostic engine tools (written once, shared by every EVM contract);
// per-contract build_* tools (build_faucet_claim, later build_swap, …) live
// alongside them and call EVMAssembler.Assemble.

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

// -- build_faucet_claim (per-contract) ---------------------------------

type BuildFaucetClaimInput struct {
	PayloadClientID string `json:"payload_client_id" jsonschema:"broadcast-idempotency uuid; reuse the same value when you later call broadcast_evm_tx"`
}

type BuildFaucetClaimOutput struct {
	Payload payload.EVMTxPayload `json:"payload"`
}

func (h *Handlers) BuildFaucetClaim(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in BuildFaucetClaimInput,
) (*mcp.CallToolResult, BuildFaucetClaimOutput, error) {
	tp, err := h.authorize(ctx, "build_faucet_claim")
	if err != nil {
		return nil, BuildFaucetClaimOutput{}, err
	}
	if h.Deps.EVM.Assembler == nil {
		return nil, BuildFaucetClaimOutput{}, userErrf("EVM is not enabled on this server (no evm_rpc_url configured)")
	}
	if h.Deps.EVM.FaucetAddress == "" {
		return nil, BuildFaucetClaimOutput{}, userErrf("faucet contract is not configured (set [evm.faucet] address)")
	}

	from, err := ownerEthAddress(tp.Owner)
	if err != nil {
		return nil, BuildFaucetClaimOutput{}, err
	}
	to, data, err := builder.BuildFaucetClaim(builder.FaucetClaimArgs{
		Contract: common.HexToAddress(h.Deps.EVM.FaucetAddress),
	})
	if err != nil {
		return nil, BuildFaucetClaimOutput{}, err
	}
	p, err := h.Deps.EVM.Assembler.Assemble(ctx, builder.EVMArgs{
		ClientID: in.PayloadClientID,
		From:     from,
		To:       to,
		Data:     data,
		Summary: payload.EVMSummary{
			ToolName:    "build_faucet_claim",
			Description: "faucet claim()",
		},
	})
	if err != nil {
		return nil, BuildFaucetClaimOutput{}, err
	}
	return nil, BuildFaucetClaimOutput{Payload: *p}, nil
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
	return nil, BroadcastEVMTxOutput{TxHash: txHash}, nil
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
