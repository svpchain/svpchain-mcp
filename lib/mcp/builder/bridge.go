package builder

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

// bridge.go is the per-contract ABI layer for svpchain's SVPBridge — the
// unified cross-chain bridge/gateway — the seam the build_bridge_deposit tool
// sits on top of. Like uniswap.go / oracle.go it binds one config-supplied
// deployment address to the parsed ABI and owns nothing chain-aware
// (nonce/gas/fees live in EVMAssembler); it only turns the two outbound deposit
// entrypoints into calldata.
//
// The SVPBridge deposit functions are NOT renamed Uniswap-style (no *ETH*->*SVP*
// trap here), but the same rule applies: a function's 4-byte selector is
// keccak256 of its canonical signature, so the names + argument types below MUST
// match the deployed SVPBridge.sol verbatim — a stray field would compute a
// selector the contract does not answer.

// svpBridgeABI is the slice of SVPBridge the deposit tool needs: the two
// outbound entrypoints. deposit() locks an ERC-20 (caller must approve the
// bridge first); depositNative() carries the native coin as msg.value. Both
// declare the destination chain id, the asset address on the destination chain
// (targetToken; address(0) = native there), and the recipient (destination).
// The withdrawal side of the contract is validator-driven and never built here.
const svpBridgeABI = `[
  {"name":"deposit","type":"function","stateMutability":"nonpayable","inputs":[{"name":"token","type":"address"},{"name":"amount","type":"uint256"},{"name":"targetChainId","type":"uint64"},{"name":"targetToken","type":"address"},{"name":"destination","type":"address"}],"outputs":[]},
  {"name":"depositNative","type":"function","stateMutability":"payable","inputs":[{"name":"targetChainId","type":"uint64"},{"name":"targetToken","type":"address"},{"name":"destination","type":"address"}],"outputs":[]}
]`

// Bridge binds one SVPBridge deployment address to the parsed ABI. It is
// constructed once at wire time and shared by every build_bridge_deposit call;
// all methods are read-only on it, so it is safe for concurrent use.
type Bridge struct {
	addr common.Address
	abi  abi.ABI
}

// NewBridge parses the embedded ABI and binds it to a deployment's bridge
// address. Returns an error only if the embedded ABI JSON fails to parse, which
// would be a build-time programming error (cf. NewUniswapV2 / NewOracleFeed).
func NewBridge(addr common.Address) (*Bridge, error) {
	a, err := abi.JSON(strings.NewReader(svpBridgeABI))
	if err != nil {
		return nil, fmt.Errorf("parse bridge abi: %w", err)
	}
	return &Bridge{addr: addr, abi: a}, nil
}

// Contract returns the bridge address deposits target (the tx `to`).
func (b *Bridge) Contract() common.Address { return b.addr }

// PackDeposit encodes deposit(token, amount, targetChainId, targetToken,
// destination) — lock an ERC-20 for bridging. The caller must have approved the
// bridge to spend `amount` of `token` first (the contract pulls via
// transferFrom). amount is in base units.
func (b *Bridge) PackDeposit(
	token common.Address, amount *big.Int, targetChainID uint64, targetToken, destination common.Address,
) ([]byte, error) {
	return b.abi.Pack("deposit", token, amount, targetChainID, targetToken, destination)
}

// PackDepositNative encodes depositNative(targetChainId, targetToken,
// destination) — bridge the native coin. The amount rides as the tx value, so it
// is not an ABI argument.
func (b *Bridge) PackDepositNative(
	targetChainID uint64, targetToken, destination common.Address,
) ([]byte, error) {
	return b.abi.Pack("depositNative", targetChainID, targetToken, destination)
}
