package tools

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/svpchain/svpchain-mcp/lib/mcp/builder"
	"github.com/svpchain/svpchain-mcp/lib/mcp/chain"
	"github.com/svpchain/svpchain-mcp/lib/mcp/payload"
)

// erc.go is the generic ERC-20 / ERC-721 build_* family: transfer / approve /
// transferFrom for any token contract, and the NFT equivalents. Unlike the swap
// family these target an arbitrary contract address (not a fixed router) and
// need no Uniswap binding — only the EVM client + assembler. The write flow is
// the same as every other EVM build_*: build_erc20_* / build_erc721_* return an
// EVMTxPayload the caller signs on the local signer (sign_evm_transaction) and
// submits via broadcast_evm_tx.
//
// ERC-20 amounts are human units, converted via the token's on-chain decimals()
// (matching build_swap / build_token_approval). ERC-721 token ids are bare
// uint256 integers (NFTs have no decimals). Output goes wherever the caller
// names — these tools take an explicit recipient, unlike the swap family.
//
// Per-symbol daily transfer-out caps are NOT applied here: they are enforced at
// broadcast_evm_tx, which decodes the signed tx's calldata (decodeTransferOut)
// and counts transfers of known tokens. Building is cap-free; broadcasting is
// where the ledger moves.

// requireEVM returns a clean user error if the server was started without the
// EVM JSON-RPC config the build_erc20_* / build_erc721_* tools need. Unlike
// requireSwap it does not require a Uniswap binding.
func (h *Handlers) requireEVM() error {
	if h.Deps.Chain.EVM == nil || h.Deps.EVM.Assembler == nil {
		return userErrf("EVM is not enabled on this server (no evm_rpc_url configured)")
	}
	return nil
}

// parseEVMAddress validates a 0x address argument and returns it.
func parseEVMAddress(s, field string) (common.Address, error) {
	t := strings.TrimSpace(s)
	if !common.IsHexAddress(t) {
		return common.Address{}, userErrf("%s %q is not a valid 0x address", field, s)
	}
	return common.HexToAddress(t), nil
}

// parseTokenID parses a bare decimal uint256 token id (no decimals scaling).
func parseTokenID(s string) (*big.Int, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return nil, userErrf("token_id is required")
	}
	v, ok := new(big.Int).SetString(t, 10)
	if !ok {
		return nil, userErrf("token_id %q is not a base-10 integer", s)
	}
	if v.Sign() < 0 {
		return nil, userErrf("token_id must not be negative, got %q", s)
	}
	return v, nil
}

// erc20Decimals reads a token's on-chain decimals() via eth_call on the home
// (svpchain) client. Generic over any ERC-20 — does not depend on the Uniswap
// binding (cf. tokenDecimals).
func (h *Handlers) erc20Decimals(ctx context.Context, token common.Address) (int64, error) {
	return h.erc20DecimalsOn(ctx, h.Deps.Chain.EVM, token)
}

// erc20DecimalsOn is erc20Decimals against an explicit client, so a tool
// targeting a foreign chain (e.g. build_erc20_approve with chain_id) reads
// decimals on that chain's RPC rather than the home one.
func (h *Handlers) erc20DecimalsOn(ctx context.Context, client chain.EVMClient, token common.Address) (int64, error) {
	data, err := builder.PackERC20Decimals()
	if err != nil {
		return 0, err
	}
	out, err := evmCallOn(ctx, client, token, data)
	if err != nil {
		return 0, fmt.Errorf("read decimals for %s (is it an ERC-20?): %w", token.Hex(), err)
	}
	dec, err := builder.UnpackERC20Decimals(out)
	if err != nil {
		return 0, fmt.Errorf("read decimals for %s: %w", token.Hex(), err)
	}
	return int64(dec), nil
}

// evmAssemblerFor returns the EVM assembler + client to build a tx for chainID:
// the home pair (Deps.EVM.Assembler / Deps.Chain.EVM) when chainID is 0 or the
// home id, else a configured foreign chain's bundle. Mirrors EVMClientFor /
// evm_tx_status's routing so a build tool can target a foreign chain — e.g.
// approving a foreign bridge as spender for an inbound deposit, which must be
// assembled and signed for the foreign chain id, not the home one.
func (h *Handlers) evmAssemblerFor(chainID uint64) (*builder.EVMAssembler, chain.EVMClient, error) {
	if chainID == 0 || chainID == h.Deps.EVM.HomeChainID {
		return h.Deps.EVM.Assembler, h.Deps.Chain.EVM, nil
	}
	fc, ok := h.Deps.EVM.ForeignChains[chainID]
	if !ok {
		return nil, nil, userErrf("chain_id %d is not configured on this server (no home or evm_foreign_chain match)", chainID)
	}
	return fc.Assembler, fc.Client, nil
}

// assembleERC is the shared tail: stamp a ready-to-sign EVMTxPayload for a
// value-0 contract call from the tenant owner to `to` with `data`, using the
// given assembler (home for most tools; a foreign assembler when a tool targets
// another chain). The assembler's bound client fixes the stamped chain id.
func (h *Handlers) assembleERC(
	ctx context.Context, asm *builder.EVMAssembler, owner string, to common.Address, data []byte, clientID, toolName, desc string,
) (*payload.EVMTxPayload, error) {
	from, err := ownerEthAddress(owner)
	if err != nil {
		return nil, err
	}
	return asm.Assemble(ctx, builder.EVMArgs{
		ClientID: clientID,
		From:     from,
		To:       to,
		Data:     data,
		Summary: payload.EVMSummary{
			ToolName:    toolName,
			Description: desc,
		},
	})
}

// -- build_erc20_transfer ----------------------------------------------

type BuildERC20TransferInput struct {
	Token    string `json:"token" jsonschema:"the ERC-20 token contract address (0x)"`
	To       string `json:"to" jsonschema:"recipient 0x address"`
	Amount   string `json:"amount" jsonschema:"amount in human units, e.g. \"1.5\" (converted via the token's on-chain decimals)"`
	ClientID string `json:"client_id" jsonschema:"broadcast-idempotency uuid (echo into broadcast_evm_tx.client_id)"`
}

type BuildERC20Output struct {
	Payload payload.EVMTxPayload `json:"payload"`
}

// BuildERC20Transfer constructs an ERC-20 transfer(to, amount) tx from the
// tenant owner. Returns an EVMTxPayload — sign with sign_evm_transaction then
// broadcast_evm_tx.
func (h *Handlers) BuildERC20Transfer(
	ctx context.Context, _ *mcp.CallToolRequest, in BuildERC20TransferInput,
) (*mcp.CallToolResult, BuildERC20Output, error) {
	tp, err := h.authorize(ctx, "build_erc20_transfer")
	if err != nil {
		return nil, BuildERC20Output{}, err
	}
	if err := h.requireEVM(); err != nil {
		return nil, BuildERC20Output{}, err
	}
	token, err := parseEVMAddress(in.Token, "token")
	if err != nil {
		return nil, BuildERC20Output{}, err
	}
	to, err := parseEVMAddress(in.To, "to")
	if err != nil {
		return nil, BuildERC20Output{}, err
	}
	dec, err := h.erc20Decimals(ctx, token)
	if err != nil {
		return nil, BuildERC20Output{}, err
	}
	amount, err := humanToBaseUnits(in.Amount, dec)
	if err != nil {
		return nil, BuildERC20Output{}, fmt.Errorf("amount: %w", err)
	}
	data, err := builder.PackERC20Transfer(to, amount.BigInt())
	if err != nil {
		return nil, BuildERC20Output{}, err
	}
	p, err := h.assembleERC(ctx, h.Deps.EVM.Assembler, tp.Owner, token, data, in.ClientID, "build_erc20_transfer",
		fmt.Sprintf("transfer %s of %s to %s", in.Amount, token.Hex(), to.Hex()))
	if err != nil {
		return nil, BuildERC20Output{}, err
	}
	return nil, BuildERC20Output{Payload: *p}, nil
}

// -- build_erc20_approve -----------------------------------------------

type BuildERC20ApproveInput struct {
	Token     string `json:"token" jsonschema:"the ERC-20 token contract address (0x)"`
	Spender   string `json:"spender" jsonschema:"spender 0x address authorized to pull tokens"`
	Amount    string `json:"amount,omitempty" jsonschema:"allowance in human units, e.g. \"100\"; omit when unlimited=true"`
	Unlimited bool   `json:"unlimited,omitempty" jsonschema:"approve the maximum (2^256-1); ignores amount"`
	ChainID   uint64 `json:"chain_id,omitempty" jsonschema:"EVM chain id to build the approval for; omit for the home (svpchain) chain. To approve a foreign bridge before an inbound deposit, pass the chain_id from the bridge's 'approve first' message (the source_chain_id of build_bridge_deposit_inbound)."`
	ClientID  string `json:"client_id" jsonschema:"broadcast-idempotency uuid (echo into broadcast_evm_tx.client_id)"`
}

// BuildERC20Approve constructs an ERC-20 approve(spender, amount) tx. Returns an
// EVMTxPayload — sign with sign_evm_transaction then broadcast_evm_tx.
func (h *Handlers) BuildERC20Approve(
	ctx context.Context, _ *mcp.CallToolRequest, in BuildERC20ApproveInput,
) (*mcp.CallToolResult, BuildERC20Output, error) {
	tp, err := h.authorize(ctx, "build_erc20_approve")
	if err != nil {
		return nil, BuildERC20Output{}, err
	}
	if err := h.requireEVM(); err != nil {
		return nil, BuildERC20Output{}, err
	}
	// Resolve the target chain up front: omit/home builds on the home assembler
	// (unchanged), a foreign chain_id builds on that chain's assembler + client so
	// the approval is stamped with the foreign chain id and read against its RPC —
	// required to approve a foreign bridge before an inbound deposit.
	asm, client, err := h.evmAssemblerFor(in.ChainID)
	if err != nil {
		return nil, BuildERC20Output{}, err
	}
	token, err := parseEVMAddress(in.Token, "token")
	if err != nil {
		return nil, BuildERC20Output{}, err
	}
	spender, err := parseEVMAddress(in.Spender, "spender")
	if err != nil {
		return nil, BuildERC20Output{}, err
	}
	amount := maxUint256
	amountLabel := "unlimited"
	if !in.Unlimited {
		dec, err := h.erc20DecimalsOn(ctx, client, token)
		if err != nil {
			return nil, BuildERC20Output{}, err
		}
		base, err := humanToBaseUnits(in.Amount, dec)
		if err != nil {
			return nil, BuildERC20Output{}, fmt.Errorf("amount: %w", err)
		}
		amount = base.BigInt()
		amountLabel = in.Amount
	}
	data, err := builder.PackERC20Approve(spender, amount)
	if err != nil {
		return nil, BuildERC20Output{}, err
	}
	p, err := h.assembleERC(ctx, asm, tp.Owner, token, data, in.ClientID, "build_erc20_approve",
		fmt.Sprintf("approve %s to spend %s of %s", spender.Hex(), amountLabel, token.Hex()))
	if err != nil {
		return nil, BuildERC20Output{}, err
	}
	return nil, BuildERC20Output{Payload: *p}, nil
}

// -- build_erc20_transfer_from -----------------------------------------

type BuildERC20TransferFromInput struct {
	Token    string `json:"token" jsonschema:"the ERC-20 token contract address (0x)"`
	From     string `json:"from" jsonschema:"owner 0x address tokens are pulled from (must have approved the signer)"`
	To       string `json:"to" jsonschema:"recipient 0x address"`
	Amount   string `json:"amount" jsonschema:"amount in human units, e.g. \"1.5\" (converted via the token's on-chain decimals)"`
	ClientID string `json:"client_id" jsonschema:"broadcast-idempotency uuid (echo into broadcast_evm_tx.client_id)"`
}

// BuildERC20TransferFrom constructs an ERC-20 transferFrom(from, to, amount) tx
// — pull tokens from an owner that previously approved the signer. Returns an
// EVMTxPayload — sign with sign_evm_transaction then broadcast_evm_tx.
func (h *Handlers) BuildERC20TransferFrom(
	ctx context.Context, _ *mcp.CallToolRequest, in BuildERC20TransferFromInput,
) (*mcp.CallToolResult, BuildERC20Output, error) {
	tp, err := h.authorize(ctx, "build_erc20_transfer_from")
	if err != nil {
		return nil, BuildERC20Output{}, err
	}
	if err := h.requireEVM(); err != nil {
		return nil, BuildERC20Output{}, err
	}
	token, err := parseEVMAddress(in.Token, "token")
	if err != nil {
		return nil, BuildERC20Output{}, err
	}
	from, err := parseEVMAddress(in.From, "from")
	if err != nil {
		return nil, BuildERC20Output{}, err
	}
	to, err := parseEVMAddress(in.To, "to")
	if err != nil {
		return nil, BuildERC20Output{}, err
	}
	dec, err := h.erc20Decimals(ctx, token)
	if err != nil {
		return nil, BuildERC20Output{}, err
	}
	amount, err := humanToBaseUnits(in.Amount, dec)
	if err != nil {
		return nil, BuildERC20Output{}, fmt.Errorf("amount: %w", err)
	}
	data, err := builder.PackERC20TransferFrom(from, to, amount.BigInt())
	if err != nil {
		return nil, BuildERC20Output{}, err
	}
	p, err := h.assembleERC(ctx, h.Deps.EVM.Assembler, tp.Owner, token, data, in.ClientID, "build_erc20_transfer_from",
		fmt.Sprintf("transferFrom %s -> %s, %s of %s", from.Hex(), to.Hex(), in.Amount, token.Hex()))
	if err != nil {
		return nil, BuildERC20Output{}, err
	}
	return nil, BuildERC20Output{Payload: *p}, nil
}

// -- ERC-721 -----------------------------------------------------------

type BuildERC721Output struct {
	Payload payload.EVMTxPayload `json:"payload"`
}

type BuildERC721TransferInput struct {
	Contract string `json:"contract" jsonschema:"the ERC-721 NFT contract address (0x)"`
	From     string `json:"from" jsonschema:"current owner 0x address"`
	To       string `json:"to" jsonschema:"recipient 0x address"`
	TokenID  string `json:"token_id" jsonschema:"the NFT token id (decimal string)"`
	ClientID string `json:"client_id" jsonschema:"broadcast-idempotency uuid (echo into broadcast_evm_tx.client_id)"`
}

// buildERC721Transfer is shared by the two NFT transfer tools, differing only in
// which packer / selector they use.
func (h *Handlers) buildERC721Transfer(
	ctx context.Context, tool string, in BuildERC721TransferInput,
	pack func(from, to common.Address, tokenID *big.Int) ([]byte, error),
) (*mcp.CallToolResult, BuildERC721Output, error) {
	tp, err := h.authorize(ctx, tool)
	if err != nil {
		return nil, BuildERC721Output{}, err
	}
	if err := h.requireEVM(); err != nil {
		return nil, BuildERC721Output{}, err
	}
	contract, err := parseEVMAddress(in.Contract, "contract")
	if err != nil {
		return nil, BuildERC721Output{}, err
	}
	from, err := parseEVMAddress(in.From, "from")
	if err != nil {
		return nil, BuildERC721Output{}, err
	}
	to, err := parseEVMAddress(in.To, "to")
	if err != nil {
		return nil, BuildERC721Output{}, err
	}
	id, err := parseTokenID(in.TokenID)
	if err != nil {
		return nil, BuildERC721Output{}, err
	}
	data, err := pack(from, to, id)
	if err != nil {
		return nil, BuildERC721Output{}, err
	}
	p, err := h.assembleERC(ctx, h.Deps.EVM.Assembler, tp.Owner, contract, data, in.ClientID, tool,
		fmt.Sprintf("%s token %s on %s: %s -> %s", tool, in.TokenID, contract.Hex(), from.Hex(), to.Hex()))
	if err != nil {
		return nil, BuildERC721Output{}, err
	}
	return nil, BuildERC721Output{Payload: *p}, nil
}

// BuildERC721TransferFrom constructs an ERC-721 transferFrom(from, to, tokenId) tx.
func (h *Handlers) BuildERC721TransferFrom(
	ctx context.Context, _ *mcp.CallToolRequest, in BuildERC721TransferInput,
) (*mcp.CallToolResult, BuildERC721Output, error) {
	return h.buildERC721Transfer(ctx, "build_erc721_transfer_from", in, builder.PackERC721TransferFrom)
}

// BuildERC721SafeTransferFrom constructs the 3-arg ERC-721
// safeTransferFrom(from, to, tokenId) tx.
func (h *Handlers) BuildERC721SafeTransferFrom(
	ctx context.Context, _ *mcp.CallToolRequest, in BuildERC721TransferInput,
) (*mcp.CallToolResult, BuildERC721Output, error) {
	return h.buildERC721Transfer(ctx, "build_erc721_safe_transfer_from", in, builder.PackERC721SafeTransferFrom)
}

// -- build_erc721_approve ----------------------------------------------

type BuildERC721ApproveInput struct {
	Contract string `json:"contract" jsonschema:"the ERC-721 NFT contract address (0x)"`
	Spender  string `json:"spender" jsonschema:"0x address granted control of the single token (zero address clears it)"`
	TokenID  string `json:"token_id" jsonschema:"the NFT token id to approve (decimal string)"`
	ClientID string `json:"client_id" jsonschema:"broadcast-idempotency uuid (echo into broadcast_evm_tx.client_id)"`
}

// BuildERC721Approve constructs an ERC-721 approve(spender, tokenId) tx granting
// control of one NFT. Returns an EVMTxPayload — sign then broadcast_evm_tx.
func (h *Handlers) BuildERC721Approve(
	ctx context.Context, _ *mcp.CallToolRequest, in BuildERC721ApproveInput,
) (*mcp.CallToolResult, BuildERC721Output, error) {
	tp, err := h.authorize(ctx, "build_erc721_approve")
	if err != nil {
		return nil, BuildERC721Output{}, err
	}
	if err := h.requireEVM(); err != nil {
		return nil, BuildERC721Output{}, err
	}
	contract, err := parseEVMAddress(in.Contract, "contract")
	if err != nil {
		return nil, BuildERC721Output{}, err
	}
	spender, err := parseEVMAddress(in.Spender, "spender")
	if err != nil {
		return nil, BuildERC721Output{}, err
	}
	id, err := parseTokenID(in.TokenID)
	if err != nil {
		return nil, BuildERC721Output{}, err
	}
	data, err := builder.PackERC721Approve(spender, id)
	if err != nil {
		return nil, BuildERC721Output{}, err
	}
	p, err := h.assembleERC(ctx, h.Deps.EVM.Assembler, tp.Owner, contract, data, in.ClientID, "build_erc721_approve",
		fmt.Sprintf("approve %s for token %s on %s", spender.Hex(), in.TokenID, contract.Hex()))
	if err != nil {
		return nil, BuildERC721Output{}, err
	}
	return nil, BuildERC721Output{Payload: *p}, nil
}

// -- build_erc721_set_approval_for_all ---------------------------------

type BuildERC721SetApprovalForAllInput struct {
	Contract string `json:"contract" jsonschema:"the ERC-721 NFT contract address (0x)"`
	Operator string `json:"operator" jsonschema:"operator 0x address to grant or revoke"`
	Approved bool   `json:"approved" jsonschema:"true to grant the operator control of the whole collection, false to revoke"`
	ClientID string `json:"client_id" jsonschema:"broadcast-idempotency uuid (echo into broadcast_evm_tx.client_id)"`
}

// BuildERC721SetApprovalForAll constructs an ERC-721 setApprovalForAll(operator,
// approved) tx. Returns an EVMTxPayload — sign then broadcast_evm_tx.
func (h *Handlers) BuildERC721SetApprovalForAll(
	ctx context.Context, _ *mcp.CallToolRequest, in BuildERC721SetApprovalForAllInput,
) (*mcp.CallToolResult, BuildERC721Output, error) {
	tp, err := h.authorize(ctx, "build_erc721_set_approval_for_all")
	if err != nil {
		return nil, BuildERC721Output{}, err
	}
	if err := h.requireEVM(); err != nil {
		return nil, BuildERC721Output{}, err
	}
	contract, err := parseEVMAddress(in.Contract, "contract")
	if err != nil {
		return nil, BuildERC721Output{}, err
	}
	operator, err := parseEVMAddress(in.Operator, "operator")
	if err != nil {
		return nil, BuildERC721Output{}, err
	}
	data, err := builder.PackERC721SetApprovalForAll(operator, in.Approved)
	if err != nil {
		return nil, BuildERC721Output{}, err
	}
	verb := "revoke operator"
	if in.Approved {
		verb = "grant operator"
	}
	p, err := h.assembleERC(ctx, h.Deps.EVM.Assembler, tp.Owner, contract, data, in.ClientID, "build_erc721_set_approval_for_all",
		fmt.Sprintf("%s %s for all of %s", verb, operator.Hex(), contract.Hex()))
	if err != nil {
		return nil, BuildERC721Output{}, err
	}
	return nil, BuildERC721Output{Payload: *p}, nil
}
