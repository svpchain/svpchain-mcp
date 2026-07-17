package signer

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/cosmos/cosmos-sdk/types/tx/signing"
	"github.com/cosmos/evm/crypto/ethsecp256k1"
	"github.com/cosmos/gogoproto/proto"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	ethcrypto "github.com/ethereum/go-ethereum/crypto"

	appconfig "github.com/dydxprotocol/v4-chain/protocol/app/config"
	"github.com/svpchain/svpchain-mcp/lib/mcp/payload"
)

func init() {
	// Set the svp bech32 prefix so any sdk.AccAddress stringification
	// matches the chain. Importing this package is sufficient — no caller
	// needs its own blank import of app/config.
	appconfig.SetAddressPrefixes()
}

// ParsePrivKey decodes a 32-byte eth_secp256k1 private key from a hex
// string. A leading "0x" is tolerated; surrounding whitespace is trimmed.
func ParsePrivKey(s string) (*ethsecp256k1.PrivKey, error) {
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

// DeriveAddress returns the bech32 string address derived from priv's
// public key, with the svp prefix in place.
func DeriveAddress(priv *ethsecp256k1.PrivKey) string {
	return sdk.AccAddress(priv.PubKey().Address()).String()
}

// Sign turns p into a SignedTx by building an AuthInfo with the signer's
// pubkey, computing the SIGN_MODE_DIRECT sign-bytes via
// payload.DirectSignBytes (shared with the remote MCP server so both
// sides agree on the byte layout), signing with priv, and proto-marshaling
// a TxRaw.
//
// Cross-checks:
//   - p.Version must equal payload.CurrentVersion.
//   - If p.SignerAddress is non-empty, it must equal the key-derived
//     address; an empty p.SignerAddress is tolerated for ad-hoc demos
//     (the caller is responsible for the address mismatch downstream).
func Sign(priv *ethsecp256k1.PrivKey, p *payload.TxPayload) (*payload.SignedTx, error) {
	if p.Version != payload.CurrentVersion {
		return nil, fmt.Errorf("unsupported TxPayload version %d (want %d)", p.Version, payload.CurrentVersion)
	}

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

// SignEVM turns an EVMTxPayload into a signed, RLP-encoded EIP-1559 Ethereum
// transaction (EVMSignedTx). It is the EVM analog of Sign: the production
// signer MCP (svpchain-signer-mcp) exposes this as the sign_evm_tx tool; this
// in-tree copy backs scripts/devsign and the e2e/unit tests so the full
// build→sign→broadcast flow runs without the external signer.
//
// Unlike the Cosmos path, an Ethereum tx is self-contained — there is no
// AuthInfo/pubkey to attach; the same eth_secp256k1 key (re-derived as an
// ECDSA key over the same secp256k1 curve) signs the RLP payload directly.
//
// Cross-check: the key's 0x address must equal p.SignerAddress (when set), so
// the signer can't be tricked into signing a tx for another account.
func SignEVM(priv *ethsecp256k1.PrivKey, p *payload.EVMTxPayload) (*payload.EVMSignedTx, error) {
	if p.Version != payload.CurrentVersion {
		return nil, fmt.Errorf("unsupported EVMTxPayload version %d (want %d)", p.Version, payload.CurrentVersion)
	}

	ecdsaKey, err := ethcrypto.ToECDSA(priv.Key)
	if err != nil {
		return nil, fmt.Errorf("convert key to ECDSA: %w", err)
	}
	from := ethcrypto.PubkeyToAddress(ecdsaKey.PublicKey)
	if p.SignerAddress != "" && from != ethcommon.HexToAddress(p.SignerAddress) {
		return nil, fmt.Errorf("key-derived address %s does not match payload.signer_address %s",
			from.Hex(), p.SignerAddress)
	}

	chainID, ok := new(big.Int).SetString(p.EVMChainID, 10)
	if !ok {
		return nil, fmt.Errorf("parse evm_chain_id %q", p.EVMChainID)
	}
	nonce, err := strconv.ParseUint(p.Nonce, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse nonce %q: %w", p.Nonce, err)
	}
	gasLimit, err := strconv.ParseUint(p.Gas, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse gas %q: %w", p.Gas, err)
	}
	maxFee, ok := new(big.Int).SetString(p.MaxFeePerGas, 10)
	if !ok {
		return nil, fmt.Errorf("parse max_fee_per_gas %q", p.MaxFeePerGas)
	}
	tip, ok := new(big.Int).SetString(p.MaxPriorityFeePerGas, 10)
	if !ok {
		return nil, fmt.Errorf("parse max_priority_fee_per_gas %q", p.MaxPriorityFeePerGas)
	}
	value := new(big.Int) // empty value == 0
	if p.Value != "" {
		v, ok := new(big.Int).SetString(p.Value, 10)
		if !ok {
			return nil, fmt.Errorf("parse value %q", p.Value)
		}
		value = v
	}
	var data []byte
	if p.Data != "" {
		data, err = hexutil.Decode(p.Data)
		if err != nil {
			return nil, fmt.Errorf("decode data: %w", err)
		}
	}
	to := ethcommon.HexToAddress(p.To)

	tx := ethtypes.NewTx(&ethtypes.DynamicFeeTx{
		ChainID:   chainID,
		Nonce:     nonce,
		GasTipCap: tip,
		GasFeeCap: maxFee,
		Gas:       gasLimit,
		To:        &to,
		Value:     value,
		Data:      data,
	})
	signed, err := ethtypes.SignTx(tx, ethtypes.LatestSignerForChainID(chainID), ecdsaKey)
	if err != nil {
		return nil, fmt.Errorf("sign evm tx: %w", err)
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("marshal signed evm tx: %w", err)
	}
	return &payload.EVMSignedTx{RawTxHex: hexutil.Encode(raw)}, nil
}
