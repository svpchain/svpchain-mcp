package builder

import (
	_ "embed"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
)

// Per-contract ABIs are vendored from the contracts' source repo (they do not
// live in protocol/) and embedded here, so the EVM build tools can ABI-encode
// calldata without a live node or an explorer. Adding a contract = drop its
// ABI JSON in abis/ and add a //go:embed + parsed var below.

//go:embed abis/faucet.json
var faucetABIJSON string

// FaucetABI is the parsed faucet contract ABI.
//
// TODO(faucet): faucet.json currently holds a placeholder `claim()` interface.
// Replace it with the real ABI exported from the faucet contract's repo, and
// adjust BuildFaucetClaim + build_faucet_claim's inputs if the claim method
// takes arguments (e.g. drip(address) / requestTokens(address)).
var FaucetABI = mustParseABI("faucet", faucetABIJSON)

func mustParseABI(name, jsonStr string) abi.ABI {
	parsed, err := abi.JSON(strings.NewReader(jsonStr))
	if err != nil {
		panic(fmt.Sprintf("parse vendored %s ABI: %v", name, err))
	}
	return parsed
}
