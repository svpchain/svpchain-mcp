package builder

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
)

// faucet.go is the faucet per-contract layer: it turns typed args into
// (to, calldata) using the vendored FaucetABI. It knows nothing about nonces,
// gas, or signing — EVMAssembler.Assemble fills those. This is the template
// every future EVM contract (swap, …) follows.

// FaucetClaimArgs are the inputs to a faucet claim.
//
// The placeholder claim() takes no parameters, so only the contract address is
// needed. When the real ABI lands, add the claim method's args here (e.g. a
// token or recipient address) and pass them to Pack below.
type FaucetClaimArgs struct {
	Contract common.Address
}

// BuildFaucetClaim ABI-encodes the faucet's claim call and returns the target
// contract + calldata for EVMAssembler.Assemble. Pure and node-free so it's
// trivially unit-testable.
func BuildFaucetClaim(args FaucetClaimArgs) (to common.Address, data []byte, err error) {
	data, err = FaucetABI.Pack("claim")
	if err != nil {
		return common.Address{}, nil, fmt.Errorf("pack faucet claim: %w", err)
	}
	return args.Contract, data, nil
}
