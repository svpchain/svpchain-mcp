package builder

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

// oracle.go is the per-contract ABI layer for svpchain's OffChainAggregator — a
// Chainlink AggregatorV3-style price feed — the seam the get_oracle_price tool
// sits on top of. Like uniswap.go it binds one config-supplied deployment
// address to the parsed ABI and owns nothing chain-aware (there is no write
// path here at all: every method is a read-only view call). It only turns the
// getters into calldata and decodes their results.

// oracleFeedABI is the slice of the AggregatorV3 interface the read tool needs:
// the latest round (answer + timestamps + round id) plus the two metadata
// getters used to render a human-readable price (decimals to scale, description
// to label).
const oracleFeedABI = `[
  {"name":"decimals","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"uint8"}]},
  {"name":"description","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"string"}]},
  {"name":"latestRoundData","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"roundId","type":"uint80"},{"name":"answer","type":"int256"},{"name":"startedAt","type":"uint256"},{"name":"updatedAt","type":"uint256"},{"name":"answeredInRound","type":"uint80"}]}
]`

// OracleRoundData is the decoded subset of latestRoundData() the read tool
// surfaces: the price answer (raw int256, scaled by the feed's decimals), the
// round it belongs to, and when that round was last updated (unix seconds).
type OracleRoundData struct {
	RoundID   *big.Int
	Answer    *big.Int
	UpdatedAt *big.Int
}

// OracleFeed binds one deployment's aggregator address to the parsed ABI. It is
// constructed once at wire time and shared by every get_oracle_price call; all
// methods are read-only on it, so it is safe for concurrent use.
type OracleFeed struct {
	addr common.Address
	abi  abi.ABI
}

// NewOracleFeed parses the embedded ABI and binds it to a deployment's
// aggregator address. Returns an error only if the embedded ABI JSON fails to
// parse, which would be a build-time programming error (cf. NewUniswapV2).
func NewOracleFeed(addr common.Address) (*OracleFeed, error) {
	a, err := abi.JSON(strings.NewReader(oracleFeedABI))
	if err != nil {
		return nil, fmt.Errorf("parse oracle abi: %w", err)
	}
	return &OracleFeed{addr: addr, abi: a}, nil
}

// Address returns the aggregator address reads target.
func (o *OracleFeed) Address() common.Address { return o.addr }

// PackDecimals encodes the decimals() getter for an eth_call.
func (o *OracleFeed) PackDecimals() ([]byte, error) {
	return o.abi.Pack("decimals")
}

// UnpackDecimals decodes the uint8 returned by decimals().
func (o *OracleFeed) UnpackDecimals(data []byte) (uint8, error) {
	out, err := o.abi.Unpack("decimals", data)
	if err != nil {
		return 0, fmt.Errorf("decode decimals: %w", err)
	}
	v, ok := out[0].(uint8)
	if !ok {
		return 0, fmt.Errorf("decimals returned an unexpected shape")
	}
	return v, nil
}

// PackDescription encodes the description() getter for an eth_call.
func (o *OracleFeed) PackDescription() ([]byte, error) {
	return o.abi.Pack("description")
}

// UnpackDescription decodes the string returned by description() (e.g.
// "BTC / USD"). Empty is valid — a feed need not set one.
func (o *OracleFeed) UnpackDescription(data []byte) (string, error) {
	out, err := o.abi.Unpack("description", data)
	if err != nil {
		return "", fmt.Errorf("decode description: %w", err)
	}
	v, ok := out[0].(string)
	if !ok {
		return "", fmt.Errorf("description returned an unexpected shape")
	}
	return strings.TrimSpace(v), nil
}

// PackLatestRoundData encodes the latestRoundData() getter for an eth_call.
func (o *OracleFeed) PackLatestRoundData() ([]byte, error) {
	return o.abi.Pack("latestRoundData")
}

// UnpackLatestRoundData decodes the (roundId, answer, startedAt, updatedAt,
// answeredInRound) tuple, returning the fields the read tool needs.
// uint80/uint256/int256 all decode to *big.Int.
func (o *OracleFeed) UnpackLatestRoundData(data []byte) (OracleRoundData, error) {
	out, err := o.abi.Unpack("latestRoundData", data)
	if err != nil {
		return OracleRoundData{}, fmt.Errorf("decode latestRoundData: %w", err)
	}
	if len(out) != 5 {
		return OracleRoundData{}, fmt.Errorf("latestRoundData returned %d values, want 5", len(out))
	}
	roundID, ok1 := out[0].(*big.Int)
	answer, ok2 := out[1].(*big.Int)
	updatedAt, ok3 := out[3].(*big.Int)
	if !ok1 || !ok2 || !ok3 {
		return OracleRoundData{}, fmt.Errorf("latestRoundData returned an unexpected shape")
	}
	return OracleRoundData{RoundID: roundID, Answer: answer, UpdatedAt: updatedAt}, nil
}
