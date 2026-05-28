package main

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/cosmos/cosmos-sdk/types/tx/signing"
	"github.com/cosmos/evm/crypto/ethsecp256k1"
	"github.com/cosmos/gogoproto/proto"

	appconfig "github.com/dydxprotocol/v4-chain/protocol/app/config"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/payload"
)

func init() {
	// Set the svp bech32 prefix on package init so any sdk.AccAddress
	// stringification matches the chain.
	appconfig.SetAddressPrefixes()
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "devsign: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	inPath := flag.String("in", "", "path to TxPayload JSON (default: stdin)")
	outPath := flag.String("out", "", "path to write SignedTx JSON (default: stdout)")
	keyHex := flag.String("key-hex", "", "32-byte hex private key (no 0x prefix required; also reads DEVSIGN_KEY_HEX env)")
	flag.Parse()
	if *keyHex == "" {
		*keyHex = os.Getenv("DEVSIGN_KEY_HEX")
	}
	if *keyHex == "" {
		return fmt.Errorf("--key-hex (or DEVSIGN_KEY_HEX env) is required")
	}

	priv, err := parsePrivKey(*keyHex)
	if err != nil {
		return fmt.Errorf("parse key: %w", err)
	}

	pInput, err := readInput(*inPath)
	if err != nil {
		return fmt.Errorf("read input: %w", err)
	}
	var p payload.TxPayload
	if err := json.Unmarshal(pInput, &p); err != nil {
		return fmt.Errorf("decode TxPayload: %w", err)
	}
	if p.Version != payload.CurrentVersion {
		return fmt.Errorf("unsupported TxPayload version %d (want %d)", p.Version, payload.CurrentVersion)
	}

	signed, err := sign(priv, &p)
	if err != nil {
		return err
	}

	out, err := json.MarshalIndent(signed, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal SignedTx: %w", err)
	}
	if *outPath == "" || *outPath == "-" {
		_, err = os.Stdout.Write(append(out, '\n'))
		return err
	}
	return os.WriteFile(*outPath, append(out, '\n'), 0o600)
}

func parsePrivKey(s string) (*ethsecp256k1.PrivKey, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "0x")
	bz, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("hex decode: %w", err)
	}
	if len(bz) != 32 {
		return nil, fmt.Errorf("private key must be 32 bytes (got %d)", len(bz))
	}
	return &ethsecp256k1.PrivKey{Key: bz}, nil
}

func readInput(path string) ([]byte, error) {
	if path == "" || path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

// sign turns p into a SignedTx by building an AuthInfo with the signer's
// pubkey, computing the SIGN_MODE_DIRECT sign-bytes via
// payload.DirectSignBytes (shared with the server so both sides agree on
// the byte layout), signing with priv, and proto-marshaling a TxRaw.
//
// Cross-checks: the signer address derived from priv must match
// p.SignerAddress; the sequence in AuthInfo is taken verbatim from p.
func sign(priv *ethsecp256k1.PrivKey, p *payload.TxPayload) (*payload.SignedTx, error) {
	pub := priv.PubKey()
	signerAddr := sdk.AccAddress(pub.Address()).String()
	if p.SignerAddress != "" && signerAddr != p.SignerAddress {
		return nil, fmt.Errorf("key-derived signer address %s does not match payload.signer_address %s",
			signerAddr, p.SignerAddress)
	}

	accNum, err := strconv.ParseUint(p.AccountNumber, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse account_number %q: %w", p.AccountNumber, err)
	}
	seq, err := strconv.ParseUint(p.Sequence, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse sequence %q: %w", p.Sequence, err)
	}
	gasLimit, err := strconv.ParseUint(p.Fee.GasLimit, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse fee.gas_limit %q: %w", p.Fee.GasLimit, err)
	}

	pubAny, err := codectypes.NewAnyWithValue(pub)
	if err != nil {
		return nil, fmt.Errorf("wrap pubkey in Any: %w", err)
	}

	authInfo := &txtypes.AuthInfo{
		SignerInfos: []*txtypes.SignerInfo{{
			PublicKey: pubAny,
			ModeInfo: &txtypes.ModeInfo{
				Sum: &txtypes.ModeInfo_Single_{
					Single: &txtypes.ModeInfo_Single{
						Mode: signing.SignMode_SIGN_MODE_DIRECT,
					},
				},
			},
			Sequence: seq,
		}},
		Fee: &txtypes.Fee{
			Amount:   nil, // CLOB: zero fee
			GasLimit: gasLimit,
		},
	}
	authInfoBytes, err := proto.Marshal(authInfo)
	if err != nil {
		return nil, fmt.Errorf("marshal AuthInfo: %w", err)
	}

	// TxPayload's TxBodyBytesB64 is a base64-encoded string on the wire
	// (see the comment on payload.TxPayload). Decode it before signing.
	bodyBytes, err := base64.StdEncoding.DecodeString(p.TxBodyBytesB64)
	if err != nil {
		return nil, fmt.Errorf("decode tx_body_bytes_b64: %w", err)
	}
	signBytes, err := payload.DirectSignBytes(bodyBytes, authInfoBytes, p.ChainID, accNum)
	if err != nil {
		return nil, fmt.Errorf("compute sign-bytes: %w", err)
	}
	sig, err := priv.Sign(signBytes)
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}

	txRaw := &txtypes.TxRaw{
		BodyBytes:     bodyBytes,
		AuthInfoBytes: authInfoBytes,
		Signatures:    [][]byte{sig},
	}
	txRawBytes, err := proto.Marshal(txRaw)
	if err != nil {
		return nil, fmt.Errorf("marshal TxRaw: %w", err)
	}

	return &payload.SignedTx{
		TxRawBytesB64: base64.StdEncoding.EncodeToString(txRawBytes),
		SignatureB64:  base64.StdEncoding.EncodeToString(sig),
		PubKeyB64:     base64.StdEncoding.EncodeToString(pub.Bytes()),
	}, nil
}
