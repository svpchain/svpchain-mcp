package tools

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/bridge"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/builder"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/chain"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/payload"
)

// bridge.go is the SVPBridge cross-chain deposit tool family. Bridging is
// bidirectional and both directions run the SAME algorithm — the only thing that
// differs is the "leg": which chain the deposit tx executes on and which
// SVPBridge deployment / RPC it targets.
//
//   - build_bridge_deposit (outbound): the tx runs on svpchain (home) and the
//     bridge releases the mapped asset on a foreign chain (Sepolia / Arbitrum
//     Sepolia in the v1 deployment).
//   - build_bridge_deposit_inbound: the tx runs on a foreign chain and the bridge
//     releases the asset on svpchain.
//
// Each handler only resolves its direction (which chain is fixed, which is
// looked up) and assembles a bridgeLeg; assembleBridgeDeposit does the rest —
// amount conversion, the native-vs-ERC20 branch, the allowance pre-check, payload
// assembly, and the uniform BridgeDepositOutput. The write flow is the rest of
// the EVM family's: the returned EVMTxPayload is signed on the local signer
// (sign_evm_transaction) and submitted via broadcast_evm_tx, which routes to the
// chain the tx was stamped for; the bridge backend then watches the Deposit event
// and releases the asset on the destination chain after its dispute period.
//
// Routing intent (which destination token a given source token maps to) is NOT
// guessed: it is resolved from the operator-supplied route registry
// (evm_bridge_routes_path), which mirrors the cold-key (sourceToken,
// targetChainId, targetToken) whitelist the bridge contract enforces on chain.
// A pair the registry doesn't know is rejected here, before building a tx the
// contract would revert. The native coin rides as the tx value via depositNative;
// ERC-20s go through deposit, which pulls via transferFrom — so the algorithm
// checks the bridge's allowance first and points at build_erc20_approve
// (spender = the bridge) when it is short, exactly as build_swap does.

// bridgeLeg is the direction-agnostic context for one deposit: the resolved
// route plus the EVM deployment the deposit is built, allowance-checked, and
// assembled against. SrcChainID is where the tx executes (and is stamped into
// the signed payload for EIP-155); DestChainID is where the bridge releases the
// asset. Bridge / Assembler / Client are that source chain's SVPBridge binding,
// fee-filling assembler, and JSON-RPC client.
type bridgeLeg struct {
	Route       bridge.Route
	SrcChainID  uint64
	DestChainID uint64
	Bridge      *builder.Bridge
	Assembler   *builder.EVMAssembler
	Client      chain.EVMClient
}

// BridgeDepositOutput is the uniform result of both bridge tools: the
// ready-to-sign payload plus the resolved route. SourceChainID is the chain the
// deposit tx runs on (== the payload's stamped chain id); DestChainID is the
// chain the asset is released on.
//
// When the source token is an ERC-20 whose bridge allowance is short, the tool
// does NOT error — it returns successfully with ApprovalRequired populated and
// no Payload, so an agent that halts on tool errors can read the structured
// approval step and proceed. Payload is only present when ApprovalRequired is nil.
type BridgeDepositOutput struct {
	Payload          payload.EVMTxPayload `json:"payload"`
	SourceChainID    uint64               `json:"source_chain_id"`             // EVM chain id the deposit tx executes on
	DestChainID      uint64               `json:"dest_chain_id"`               // EVM chain id the asset is released on
	Symbol           string               `json:"symbol"`                      // bridged asset symbol
	SourceToken      string               `json:"source_token"`                // 0x on the source chain, or "native"
	TargetToken      string               `json:"target_token"`                // 0x on the dest chain, or "native"
	Recipient        string               `json:"recipient"`                   // 0x recipient on the dest chain
	AmountBase       string               `json:"amount_base"`                 // base units (integer)
	ApprovalRequired *BridgeApproval      `json:"approval_required,omitempty"` // non-nil when an erc20 approval must precede this deposit
}

// BridgeApproval is the structured "approve first" step returned (instead of an
// error) when the bridge's ERC-20 allowance does not cover the deposit. It is the
// exact set of arguments to feed build_erc20_approve, plus the tool to retry once
// the approval is signed and broadcast. ChainID is the chain the approval must be
// built for — critical for inbound, where it is the foreign source chain, not the
// home chain build_erc20_approve defaults to.
type BridgeApproval struct {
	Tool      string `json:"tool"`       // always "build_erc20_approve"
	Token     string `json:"token"`      // 0x token to approve
	Spender   string `json:"spender"`    // 0x bridge contract to approve as spender
	MinAmount string `json:"min_amount"` // human-units amount the approval must be >=
	ChainID   uint64 `json:"chain_id"`   // EVM chain id to build the approval on
	RetryTool string `json:"retry_tool"` // bridge tool to call again after approving
	Message   string `json:"message"`    // human-readable instruction
}

// buildBridge is the shared entry path for both bridge tools: authorize, resolve
// the sender/recipient, then run the direction-specific resolveLeg (which gates
// config and looks up the route in its direction) and hand the leg to the uniform
// assembleBridgeDeposit. The only thing a handler supplies is its tool name, the
// common inputs, and resolveLeg — everything else is identical across directions.
func (h *Handlers) buildBridge(
	ctx context.Context, tool, amount, recipientArg, clientID string, resolveLeg func() (bridgeLeg, error),
) (*mcp.CallToolResult, BridgeDepositOutput, error) {
	tp, err := h.authorize(ctx, tool)
	if err != nil {
		return nil, BridgeDepositOutput{}, err
	}
	from, recipient, err := bridgeParties(tp.Owner, recipientArg)
	if err != nil {
		return nil, BridgeDepositOutput{}, err
	}
	leg, err := resolveLeg()
	if err != nil {
		return nil, BridgeDepositOutput{}, err
	}
	out, err := h.assembleBridgeDeposit(ctx, leg, from, recipient, amount, clientID, tool)
	if err != nil {
		return nil, BridgeDepositOutput{}, err
	}
	return nil, *out, nil
}

// assembleBridgeDeposit is the uniform deposit-construction algorithm both bridge
// tools share. Given a fully-resolved leg, it converts the human amount via the
// route's decimals, picks depositNative (native coin, value-carried) or deposit
// (ERC-20, allowance-checked on the source chain) accordingly, assembles the
// EVMTxPayload against the leg's assembler, and returns the uniform output.
// toolName is stamped into the payload summary and used in the "approve first"
// retry hint so each direction points back at itself.
func (h *Handlers) assembleBridgeDeposit(
	ctx context.Context, leg bridgeLeg, from, recipient common.Address, amountHuman, clientID, toolName string,
) (*BridgeDepositOutput, error) {
	amount, err := humanToBaseUnits(amountHuman, leg.Route.Decimals)
	if err != nil {
		return nil, fmt.Errorf("amount: %w", err)
	}
	amountBase := amount.BigInt()

	var (
		data  []byte
		value *big.Int
	)
	base := BridgeDepositOutput{
		SourceChainID: leg.SrcChainID,
		DestChainID:   leg.DestChainID,
		Symbol:        leg.Route.Symbol,
		SourceToken:   bridgeTokenLabel(leg.Route.SrcToken),
		TargetToken:   bridgeTokenLabel(leg.Route.TargetToken),
		Recipient:     recipient.Hex(),
		AmountBase:    amountBase.String(),
	}

	if leg.Route.NativeSource() {
		data, err = leg.Bridge.PackDepositNative(leg.DestChainID, leg.Route.TargetToken, recipient)
		value = amountBase // the native coin rides as the tx value
	} else {
		// deposit() pulls via transferFrom — verify the bridge's allowance on the
		// source chain covers this deposit before building. If it is short we do not
		// error: we return a successful output carrying the structured "approve
		// first" step (no payload), so an agent that halts on errors can act on it.
		var approval *BridgeApproval
		approval, err = h.checkBridgeAllowance(ctx, leg.Client, leg.Route.SrcToken, from, leg.Bridge.Contract(), amountBase, amountHuman, toolName, leg.SrcChainID)
		if err != nil {
			return nil, err
		}
		if approval != nil {
			base.ApprovalRequired = approval
			return &base, nil
		}
		data, err = leg.Bridge.PackDeposit(leg.Route.SrcToken, amountBase, leg.DestChainID, leg.Route.TargetToken, recipient)
	}
	if err != nil {
		return nil, err
	}

	p, err := leg.Assembler.Assemble(ctx, builder.EVMArgs{
		ClientID: clientID,
		From:     from,
		To:       leg.Bridge.Contract(),
		Data:     data,
		Value:    value,
		Summary: payload.EVMSummary{
			ToolName: toolName,
			Description: fmt.Sprintf("bridge %s %s from chain %d to chain %d (recipient %s)",
				amountHuman, leg.Route.Symbol, leg.SrcChainID, leg.DestChainID, recipient.Hex()),
		},
	})
	if err != nil {
		return nil, err
	}

	base.Payload = *p
	return &base, nil
}

// bridgeParties resolves the deposit sender (the tenant owner's 0x address, which
// is the same 20 bytes on every chain) and the recipient — the owner by default,
// or an explicit override on the destination chain.
func bridgeParties(owner, recipientArg string) (from, recipient common.Address, err error) {
	from, err = ownerEthAddress(owner)
	if err != nil {
		return common.Address{}, common.Address{}, err
	}
	recipient = from
	if recipientArg != "" {
		recipient, err = parseEVMAddress(recipientArg, "recipient")
		if err != nil {
			return common.Address{}, common.Address{}, err
		}
	}
	return from, recipient, nil
}

// -- build_bridge_deposit (outbound: svpchain -> foreign) --------------

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

// BuildBridgeDeposit constructs an SVPBridge deposit that bridges a token from
// svpchain to a destination network. svpchain is the fixed source; the resolver
// looks up the destination chain and the (token, dest) route, and buildBridge
// runs the shared algorithm. Returns an EVMTxPayload — sign with
// sign_evm_transaction then broadcast_evm_tx.
func (h *Handlers) BuildBridgeDeposit(
	ctx context.Context, _ *mcp.CallToolRequest, in BuildBridgeDepositInput,
) (*mcp.CallToolResult, BridgeDepositOutput, error) {
	return h.buildBridge(ctx, "build_bridge_deposit", in.Amount, in.Recipient, in.ClientID, func() (bridgeLeg, error) {
		br, routes, srcChainID, err := h.requireBridge()
		if err != nil {
			return bridgeLeg{}, err
		}
		destChainID, err := routes.ResolveDestChain(in.DestChain, srcChainID)
		if err != nil {
			return bridgeLeg{}, userErrf("%s", err.Error())
		}
		route, err := routes.Lookup(srcChainID, destChainID, in.Token)
		if err != nil {
			return bridgeLeg{}, userErrf("%s", err.Error())
		}
		return bridgeLeg{
			Route:       route,
			SrcChainID:  srcChainID,
			DestChainID: destChainID,
			Bridge:      br,
			Assembler:   h.Deps.EVM.Assembler,
			Client:      h.Deps.Chain.EVM,
		}, nil
	})
}

// -- build_bridge_deposit_inbound (inbound: foreign -> svpchain) -------

// requireInboundBridge returns the route registry, the home (svpchain) EVM
// chain id, and the configured foreign-chain bundles, or a clean user error
// when the server was started without the inbound-bridge config the tool needs.
func (h *Handlers) requireInboundBridge() (*bridge.Registry, uint64, map[uint64]*ForeignChain, error) {
	if h.Deps.EVM.BridgeRoutes == nil {
		return nil, 0, nil, userErrf("bridging is not enabled on this server (no evm_bridge_addr / evm_bridge_routes_path configured)")
	}
	if len(h.Deps.EVM.ForeignChains) == 0 {
		return nil, 0, nil, userErrf("inbound bridging is not enabled on this server (no evm_foreign_chain configured)")
	}
	return h.Deps.EVM.BridgeRoutes, h.Deps.EVM.HomeChainID, h.Deps.EVM.ForeignChains, nil
}

type BuildBridgeDepositInboundInput struct {
	SourceChain string `json:"source_chain" jsonschema:"foreign source network to bridge FROM: a chain name (\"sepolia\", \"arbitrum_sepolia\") or numeric EVM chain id (\"11155111\", \"421614\")"`
	Token       string `json:"token" jsonschema:"token to bridge: a known symbol (\"USDC\", \"WETH\", \"SVP\"), a 0x source-token address on the foreign chain, or empty/\"native\" for the foreign chain's native coin"`
	Amount      string `json:"amount" jsonschema:"amount in human units, e.g. \"1.5\" (converted via the token's registry decimals)"`
	Recipient   string `json:"recipient,omitempty" jsonschema:"recipient 0x address on svpchain; defaults to your own address when omitted"`
	ClientID    string `json:"client_id" jsonschema:"broadcast-idempotency uuid (echo into broadcast_evm_tx.client_id)"`
}

// BuildBridgeDepositInbound constructs an SVPBridge deposit on a FOREIGN chain
// that bridges a token INTO svpchain — the inbound twin of BuildBridgeDeposit.
// svpchain is the fixed destination; the resolver looks up the foreign source
// chain and the (token, source) route and assembles the foreign leg (foreign
// bridge + assembler + RPC), so buildBridge's shared algorithm stamps the foreign
// chain's nonce/gas/fees and chain id. Returns an EVMTxPayload — sign with
// sign_evm_transaction then broadcast_evm_tx (which routes to the foreign chain
// by the tx's chain id); track it with evm_tx_status passing the returned
// source_chain_id.
func (h *Handlers) BuildBridgeDepositInbound(
	ctx context.Context, _ *mcp.CallToolRequest, in BuildBridgeDepositInboundInput,
) (*mcp.CallToolResult, BridgeDepositOutput, error) {
	return h.buildBridge(ctx, "build_bridge_deposit_inbound", in.Amount, in.Recipient, in.ClientID, func() (bridgeLeg, error) {
		routes, homeChainID, foreigns, err := h.requireInboundBridge()
		if err != nil {
			return bridgeLeg{}, err
		}
		srcChainID, err := routes.ResolveSourceChain(in.SourceChain, homeChainID)
		if err != nil {
			return bridgeLeg{}, userErrf("%s", err.Error())
		}
		fc, ok := foreigns[srcChainID]
		if !ok {
			// The route exists but no RPC/bridge is wired for that chain — config gap.
			return bridgeLeg{}, userErrf(
				"source chain %d has an inbound route but is not configured for inbound bridging (no matching evm_foreign_chain)", srcChainID)
		}
		route, err := routes.Lookup(srcChainID, homeChainID, in.Token)
		if err != nil {
			return bridgeLeg{}, userErrf("%s", err.Error())
		}
		return bridgeLeg{
			Route:       route,
			SrcChainID:  srcChainID,
			DestChainID: homeChainID,
			Bridge:      fc.Bridge,
			Assembler:   fc.Assembler,
			Client:      fc.Client,
		}, nil
	})
}

// -- shared helpers ----------------------------------------------------

// checkBridgeAllowance reads the bridge's allowance on the source token (via the
// given chain's client) and returns a structured "approve first" step pointing at
// retryTool if it does not cover amount (nil when the allowance is sufficient).
// A short allowance is NOT an error — only a genuine RPC/decode failure is — so
// callers can surface the approval step in a successful result. srcChainID is the
// chain the deposit (and thus the approval) lives on; it is surfaced as
// build_erc20_approve's chain_id so the approval is built/signed for the right
// chain — critical for inbound, where the approval must target the foreign chain,
// not the home one.
func (h *Handlers) checkBridgeAllowance(
	ctx context.Context, client chain.EVMClient, token, owner, bridgeAddr common.Address, amount *big.Int, amountHuman, retryTool string, srcChainID uint64,
) (*BridgeApproval, error) {
	data, err := builder.PackERC20Allowance(owner, bridgeAddr)
	if err != nil {
		return nil, err
	}
	out, err := evmCallOn(ctx, client, token, data)
	if err != nil {
		return nil, fmt.Errorf("read allowance for %s: %w", token.Hex(), err)
	}
	allowance, err := builder.UnpackERC20Allowance(out)
	if err != nil {
		return nil, err
	}
	if allowance.Cmp(amount) < 0 {
		return &BridgeApproval{
			Tool:      "build_erc20_approve",
			Token:     token.Hex(),
			Spender:   bridgeAddr.Hex(),
			MinAmount: amountHuman,
			ChainID:   srcChainID,
			RetryTool: retryTool,
			Message: fmt.Sprintf(
				"bridge allowance for %s is insufficient — call build_erc20_approve (token %s, spender %s, amount >= %s, chain_id %d) first, then retry %s",
				token.Hex(), token.Hex(), bridgeAddr.Hex(), amountHuman, srcChainID, retryTool),
		}, nil
	}
	return nil, nil
}

// bridgeTokenLabel renders a registry token for output: "native" for the zero
// address, else the 0x address.
func bridgeTokenLabel(addr common.Address) string {
	if addr == (common.Address{}) {
		return "native"
	}
	return addr.Hex()
}
