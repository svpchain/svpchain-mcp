package builder

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"time"

	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/cosmos/gogoproto/proto"

	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/payload"
)

// ClobGasLimit is the gas limit set on CLOB-msg txs. svpchain CLOB msgs
// are gas-free (Fee.Amount stays empty), so this is just a comfortable
// constant well above real consumption. Mirrors
// cmd/dex-bench/cosmos_signing.go:42.
const ClobGasLimit uint64 = 1_000_000

// DefaultPayloadTTL is how long a built TxPayload remains valid for
// broadcast — beyond this the local signer's signature is still
// cryptographically valid but the server rejects broadcast as stale to
// keep async-sign-then-broadcast races bounded.
const DefaultPayloadTTL = 30 * time.Second

// Assembler turns msgs + account state into a TxPayload.
//
// v0.1 encodes only TxBody bytes; the local signer constructs AuthInfo
// (with its own pubkey) and computes sign-bytes itself. v0.2 will
// precompute AuthInfo and sign-bytes when the tenant config carries the
// signer's pubkey.
type Assembler struct {
	chainID string
}

// NewAssembler returns an Assembler bound to a chain id.
func NewAssembler(chainID string) *Assembler {
	return &Assembler{chainID: chainID}
}

// Args bundles the per-build inputs.
type Args struct {
	Msgs          []sdk.Msg
	SignerAddress string
	AccountNumber uint64
	Sequence      uint64
	ClientID      string // broadcast-idempotency key (uuid)
	Summary       payload.Summary
}

// Assemble proto-marshals a TxBody containing args.Msgs (no memo, no
// timeout-height in v0.1) and returns a fully-populated TxPayload modulo
// the optional pre-computed sign-bytes (left empty in v0.1).
func (a *Assembler) Assemble(args Args) (*payload.TxPayload, error) {
	bodyBytes, err := encodeTxBody(args.Msgs)
	if err != nil {
		return nil, err
	}
	return &payload.TxPayload{
		Version:         payload.CurrentVersion,
		ClientID:        args.ClientID,
		ChainID:         a.chainID,
		SignerAddress:   args.SignerAddress,
		AccountNumber:   strconv.FormatUint(args.AccountNumber, 10),
		Sequence:        strconv.FormatUint(args.Sequence, 10),
		IsShortTermCLOB: IsShortTermClobMsgs(args.Msgs),
		TxBodyBytesB64:  base64.StdEncoding.EncodeToString(bodyBytes),
		Fee: payload.Fee{
			GasLimit: strconv.FormatUint(ClobGasLimit, 10),
			Amount:   []payload.Coin{}, // CLOB: empty
		},
		Summary:   args.Summary,
		ExpiresAt: time.Now().UTC().Add(DefaultPayloadTTL),
	}, nil
}

// encodeTxBody proto-marshals msgs into a cosmos.tx.v1beta1.TxBody. The
// local signer takes these bytes verbatim into AuthInfo+SignDoc to compute
// SIGN_MODE_DIRECT sign-bytes.
func encodeTxBody(msgs []sdk.Msg) ([]byte, error) {
	anyMsgs := make([]*codectypes.Any, 0, len(msgs))
	for i, m := range msgs {
		anyMsg, err := codectypes.NewAnyWithValue(m)
		if err != nil {
			return nil, fmt.Errorf("wrap msg %d in Any: %w", i, err)
		}
		anyMsgs = append(anyMsgs, anyMsg)
	}
	body := &txtypes.TxBody{Messages: anyMsgs}
	out, err := proto.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal TxBody: %w", err)
	}
	return out, nil
}
