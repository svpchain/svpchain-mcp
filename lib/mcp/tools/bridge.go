package tools

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/bridge"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/builder"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/payload"
)

// bridge.go is the SVPBridge cross-chain deposit tool — the per-contract EVM
// build_* tool that bridges tokens OFF svpchain to another network (Sepolia /
// Arbitrum Sepolia in the v1 deployment). The write flow mirrors the rest of the
// EVM family: build_bridge_deposit returns an EVMTxPayload the caller signs on
// the local signer (sign_evm_transaction) and submits via broadcast_evm_tx; the
// bridge backend then watches the resulting Deposit event and releases the
// mapped asset on the destination chain after its dispute period.
//
// Routing intent (which destination token a given source token maps to) is NOT
// guessed: it is resolved from the operator-supplied route registry
// (evm_bridge_routes_path), which mirrors the cold-key (sourceToken,
// targetChainId, targetToken) whitelist the bridge contract enforces on chain.
// A pair the registry doesn't know is rejected here, before building a tx the
// contract would revert.
//
// Native SVP rides as the tx value via depositNative; ERC-20s go through
// deposit, which pulls via transferFrom — so this tool checks the bridge's
// allowance first and points at build_erc20_approve (spender = the bridge) when
// it is short, exactly as build_swap does for the router.

// requireBridge returns the bridge binding, the route registry, and the
// configured source chain id, or a clean user error when the server was started
// without the EVM/bridge config the tool needs.
func (h *Handlers) requireBridge() (*builder.Bridge, *bridge.Registry, uint64, error) {
	if h.Deps.Chain.EVM == nil || h.Deps.EVM.Assembler == nil {
		return nil, nil, 0, userErrf("EVM is not enabled on this server (no evm_rpc_url configured)")
	}
	if h.Deps.EVM.Bridge == nil || h.Deps.EVM.BridgeRoutes == nil {
		return nil, nil, 0, userErrf("bridging is not enabled on this server (no evm_bridge_addr / evm_bridge_routes_path configured)")
	}
	return h.Deps.EVM.Bridge, h.Deps.EVM.BridgeRoutes, h.Deps.EVM.BridgeSourceChainID, nil
}

type BuildBridgeDepositInput struct {
	DestChain string `json:"dest_chain" jsonschema:"destination network: a chain name (\"sepolia\", \"arbitrum_sepolia\") or numeric EVM chain id (\"11155111\", \"421614\")"`
	Token     string `json:"token" jsonschema:"token to bridge: a known symbol (\"USDC\", \"WETH\"), a 0x source-token address, or empty/\"native\"/\"svp\" for native SVP"`
	Amount    string `json:"amount" jsonschema:"amount in human units, e.g. \"1.5\" (converted via the token's registry decimals)"`
	Recipient string `json:"recipient,omitempty" jsonschema:"recipient 0x address on the destination chain; defaults to your own address when omitted"`
	ClientID  string `json:"client_id" jsonschema:"broadcast-idempotency uuid (echo into broadcast_evm_tx.client_id)"`
}

type BuildBridgeDepositOutput struct {
	Payload     payload.EVMTxPayload `json:"payload"`
	DestChainID uint64               `json:"dest_chain_id"` // resolved destination EVM chain id
	Symbol      string               `json:"symbol"`        // bridged asset symbol
	SourceToken string               `json:"source_token"`  // 0x on svpchain, or "native"
	TargetToken string               `json:"target_token"`  // 0x on the destination chain, or "native"
	Recipient   string               `json:"recipient"`     // 0x recipient on the destination chain
	AmountBase  string               `json:"amount_base"`   // base units (integer)
}

// BuildBridgeDeposit constructs an SVPBridge deposit that bridges a token from
// svpchain to a destination network. It resolves the (token, dest_chain) pair to
// the destination asset via the route registry, converts the human amount via
// the registry decimals, picks depositNative (native SVP, value-carried) or
// deposit (ERC-20, allowance-checked) accordingly, and returns an EVMTxPayload —
// sign with sign_evm_transaction then broadcast_evm_tx.
func (h *Handlers) BuildBridgeDeposit(
	ctx context.Context, _ *mcp.CallToolRequest, in BuildBridgeDepositInput,
) (*mcp.CallToolResult, BuildBridgeDepositOutput, error) {
	tp, err := h.authorize(ctx, "build_bridge_deposit")
	if err != nil {
		return nil, BuildBridgeDepositOutput{}, err
	}
	br, routes, srcChainID, err := h.requireBridge()
	if err != nil {
		return nil, BuildBridgeDepositOutput{}, err
	}

	from, err := ownerEthAddress(tp.Owner)
	if err != nil {
		return nil, BuildBridgeDepositOutput{}, err
	}
	recipient := from
	if in.Recipient != "" {
		recipient, err = parseEVMAddress(in.Recipient, "recipient")
		if err != nil {
			return nil, BuildBridgeDepositOutput{}, err
		}
	}

	destChainID, err := routes.ResolveDestChain(in.DestChain, srcChainID)
	if err != nil {
		return nil, BuildBridgeDepositOutput{}, userErrf("%s", err.Error())
	}
	route, err := routes.Lookup(srcChainID, destChainID, in.Token)
	if err != nil {
		return nil, BuildBridgeDepositOutput{}, userErrf("%s", err.Error())
	}

	amount, err := humanToBaseUnits(in.Amount, route.Decimals)
	if err != nil {
		return nil, BuildBridgeDepositOutput{}, fmt.Errorf("amount: %w", err)
	}
	amountBase := amount.BigInt()

	var (
		data  []byte
		value *big.Int
	)
	if route.NativeSource() {
		data, err = br.PackDepositNative(destChainID, route.TargetToken, recipient)
		value = amountBase // native SVP rides as the tx value
	} else {
		// deposit() pulls via transferFrom — verify the bridge's allowance covers
		// this deposit before building, so the agent gets a structured "approve
		// first" instead of an on-chain revert after signing.
		if err := h.checkBridgeAllowance(ctx, route.SrcToken, from, br.Contract(), amountBase, in.Amount); err != nil {
			return nil, BuildBridgeDepositOutput{}, err
		}
		data, err = br.PackDeposit(route.SrcToken, amountBase, destChainID, route.TargetToken, recipient)
	}
	if err != nil {
		return nil, BuildBridgeDepositOutput{}, err
	}

	p, err := h.Deps.EVM.Assembler.Assemble(ctx, builder.EVMArgs{
		ClientID: in.ClientID,
		From:     from,
		To:       br.Contract(),
		Data:     data,
		Value:    value,
		Summary: payload.EVMSummary{
			ToolName: "build_bridge_deposit",
			Description: fmt.Sprintf("bridge %s %s from svpchain to chain %d (recipient %s)",
				in.Amount, route.Symbol, destChainID, recipient.Hex()),
		},
	})
	if err != nil {
		return nil, BuildBridgeDepositOutput{}, err
	}

	return nil, BuildBridgeDepositOutput{
		Payload:     *p,
		DestChainID: destChainID,
		Symbol:      route.Symbol,
		SourceToken: bridgeTokenLabel(route.SrcToken),
		TargetToken: bridgeTokenLabel(route.TargetToken),
		Recipient:   recipient.Hex(),
		AmountBase:  amountBase.String(),
	}, nil
}

// checkBridgeAllowance reads the bridge's allowance on the source token and
// returns a user-facing "approve first" error if it does not cover amount.
func (h *Handlers) checkBridgeAllowance(
	ctx context.Context, token, owner, bridgeAddr common.Address, amount *big.Int, amountHuman string,
) error {
	data, err := builder.PackERC20Allowance(owner, bridgeAddr)
	if err != nil {
		return err
	}
	out, err := h.evmCall(ctx, token, data)
	if err != nil {
		return fmt.Errorf("read allowance for %s: %w", token.Hex(), err)
	}
	allowance, err := builder.UnpackERC20Allowance(out)
	if err != nil {
		return err
	}
	if allowance.Cmp(amount) < 0 {
		return userErrf(
			"bridge allowance for %s is insufficient — call build_erc20_approve (token %s, spender %s, amount >= %s) first, then retry build_bridge_deposit",
			token.Hex(), token.Hex(), bridgeAddr.Hex(), amountHuman)
	}
	return nil
}

// bridgeTokenLabel renders a registry token for output: "native" for the zero
// address, else the 0x address.
func bridgeTokenLabel(addr common.Address) string {
	if addr == (common.Address{}) {
		return "native"
	}
	return addr.Hex()
}
