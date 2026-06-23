package builder

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

// erc.go is the per-contract ABI layer for plain ERC-20 / ERC-721 calls —
// transfer / approve / transferFrom and the NFT equivalents — the seam the
// build_erc20_* / build_erc721_* tools sit on top of. Like uniswap.go it owns
// nothing chain-aware (nonce/gas/fees live in EVMAssembler); it only turns typed
// arguments into calldata and decodes the one read it needs (decimals).
//
// Unlike UniswapV2 these ABIs bind to no deployment address — they are the
// standard token interfaces, parsed once at package init and shared by every
// call (the parsed abi.ABI is read-only, safe for concurrent use). So the build
// tools depend only on the EVM client + assembler, not on any per-deployment
// binding.

// ercTransferERC20ABI is the slice of ERC-20 the transfer/approve build tools
// need: the two write methods plus decimals() to convert human amounts at the
// boundary (matching build_swap / build_token_approval), and allowance() so a
// build tool that pulls via transferFrom (e.g. build_bridge_deposit) can verify
// the spender is already approved before building a tx that would otherwise
// revert.
const ercTransferERC20ABI = `[
  {"name":"transfer","type":"function","stateMutability":"nonpayable","inputs":[{"name":"to","type":"address"},{"name":"amount","type":"uint256"}],"outputs":[{"name":"","type":"bool"}]},
  {"name":"transferFrom","type":"function","stateMutability":"nonpayable","inputs":[{"name":"from","type":"address"},{"name":"to","type":"address"},{"name":"amount","type":"uint256"}],"outputs":[{"name":"","type":"bool"}]},
  {"name":"approve","type":"function","stateMutability":"nonpayable","inputs":[{"name":"spender","type":"address"},{"name":"amount","type":"uint256"}],"outputs":[{"name":"","type":"bool"}]},
  {"name":"allowance","type":"function","stateMutability":"view","inputs":[{"name":"owner","type":"address"},{"name":"spender","type":"address"}],"outputs":[{"name":"","type":"uint256"}]},
  {"name":"decimals","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"uint8"}]}
]`

// erc721ABI is the slice of ERC-721 the NFT build tools need. safeTransferFrom
// is the 3-arg overload (no trailing bytes); approve grants control of one
// token id; setApprovalForAll grants/revokes an operator over the whole
// collection.
const erc721ABI = `[
  {"name":"transferFrom","type":"function","stateMutability":"nonpayable","inputs":[{"name":"from","type":"address"},{"name":"to","type":"address"},{"name":"tokenId","type":"uint256"}],"outputs":[]},
  {"name":"safeTransferFrom","type":"function","stateMutability":"nonpayable","inputs":[{"name":"from","type":"address"},{"name":"to","type":"address"},{"name":"tokenId","type":"uint256"}],"outputs":[]},
  {"name":"approve","type":"function","stateMutability":"nonpayable","inputs":[{"name":"to","type":"address"},{"name":"tokenId","type":"uint256"}],"outputs":[]},
  {"name":"setApprovalForAll","type":"function","stateMutability":"nonpayable","inputs":[{"name":"operator","type":"address"},{"name":"approved","type":"bool"}],"outputs":[]}
]`

// Parsed once at init. A parse failure is a build-time programming error in the
// embedded JSON above, so we panic rather than thread an error through every
// caller (same rationale as NewUniswapV2's "build-time" comment).
var (
	ercERC20ABI  = mustParseABI(ercTransferERC20ABI)
	ercERC721ABI = mustParseABI(erc721ABI)
)

func mustParseABI(j string) abi.ABI {
	a, err := abi.JSON(strings.NewReader(j))
	if err != nil {
		panic(fmt.Sprintf("parse embedded ERC abi: %v", err))
	}
	return a
}

// -- ERC-20 ------------------------------------------------------------

// PackERC20Transfer encodes transfer(to, amount). amount is in base units.
func PackERC20Transfer(to common.Address, amount *big.Int) ([]byte, error) {
	return ercERC20ABI.Pack("transfer", to, amount)
}

// PackERC20Approve encodes approve(spender, amount). amount is in base units.
func PackERC20Approve(spender common.Address, amount *big.Int) ([]byte, error) {
	return ercERC20ABI.Pack("approve", spender, amount)
}

// PackERC20TransferFrom encodes transferFrom(from, to, amount). amount is in
// base units.
func PackERC20TransferFrom(from, to common.Address, amount *big.Int) ([]byte, error) {
	return ercERC20ABI.Pack("transferFrom", from, to, amount)
}

// PackERC20Allowance encodes the allowance(owner, spender) getter for an
// eth_call. Used to check a spender's remaining approval before building a
// transferFrom-style tx (cf. build_bridge_deposit).
func PackERC20Allowance(owner, spender common.Address) ([]byte, error) {
	return ercERC20ABI.Pack("allowance", owner, spender)
}

// UnpackERC20Allowance decodes the uint256 returned by allowance().
func UnpackERC20Allowance(data []byte) (*big.Int, error) {
	out, err := ercERC20ABI.Unpack("allowance", data)
	if err != nil {
		return nil, fmt.Errorf("decode allowance: %w", err)
	}
	v, ok := out[0].(*big.Int)
	if !ok {
		return nil, fmt.Errorf("allowance returned an unexpected shape")
	}
	return v, nil
}

// PackERC20Decimals encodes the decimals() getter for an eth_call.
func PackERC20Decimals() ([]byte, error) {
	return ercERC20ABI.Pack("decimals")
}

// UnpackERC20Decimals decodes the uint8 returned by decimals().
func UnpackERC20Decimals(data []byte) (uint8, error) {
	out, err := ercERC20ABI.Unpack("decimals", data)
	if err != nil {
		return 0, fmt.Errorf("decode decimals: %w", err)
	}
	v, ok := out[0].(uint8)
	if !ok {
		return 0, fmt.Errorf("decimals returned an unexpected shape")
	}
	return v, nil
}

// -- ERC-721 -----------------------------------------------------------

// PackERC721TransferFrom encodes transferFrom(from, to, tokenId).
func PackERC721TransferFrom(from, to common.Address, tokenID *big.Int) ([]byte, error) {
	return ercERC721ABI.Pack("transferFrom", from, to, tokenID)
}

// PackERC721SafeTransferFrom encodes the 3-arg safeTransferFrom(from, to,
// tokenId) overload (no trailing data).
func PackERC721SafeTransferFrom(from, to common.Address, tokenID *big.Int) ([]byte, error) {
	return ercERC721ABI.Pack("safeTransferFrom", from, to, tokenID)
}

// PackERC721Approve encodes approve(to, tokenId) — grant control of one token.
func PackERC721Approve(to common.Address, tokenID *big.Int) ([]byte, error) {
	return ercERC721ABI.Pack("approve", to, tokenID)
}

// PackERC721SetApprovalForAll encodes setApprovalForAll(operator, approved).
func PackERC721SetApprovalForAll(operator common.Address, approved bool) ([]byte, error) {
	return ercERC721ABI.Pack("setApprovalForAll", operator, approved)
}
