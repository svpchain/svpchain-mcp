package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/svpchain/svpchain-mcp/lib/mcp/payload"
	"github.com/svpchain/svpchain-mcp/lib/mcp/signer"
)

// devsign is a thin one-shot CLI wrapper around lib/mcp/signer. Reads a
// TxPayload JSON from --in (or stdin), signs with --key-hex (or the
// DEVSIGN_KEY_HEX env), writes a SignedTx JSON to --out (or stdout).
// Kept for fullflow e2e parity and ad-hoc dev use; production callers
// should use cmd/mcp-signer/ over stdio MCP instead.

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
	signChallenge := flag.String("sign-challenge", "", "if set, sign this text as a self-service-auth challenge and emit base64 signature to stdout (TxPayload mode is skipped)")
	evmMode := flag.Bool("evm", false, "sign an EVMTxPayload (emit EVMSignedTx) instead of a Cosmos TxPayload — the sign_evm_tx path")
	flag.Parse()

	if *keyHex == "" {
		*keyHex = os.Getenv("DEVSIGN_KEY_HEX")
	}
	if *keyHex == "" {
		return fmt.Errorf("--key-hex (or DEVSIGN_KEY_HEX env) is required")
	}

	priv, err := signer.ParsePrivKey(*keyHex)
	if err != nil {
		return fmt.Errorf("parse key: %w", err)
	}

	// --sign-challenge mode: bypass TxPayload parsing and emit a base64
	// signature over the challenge text. Used by the e2e to drive
	// auth_challenge → auth_verify without standing up mcp-signer over
	// stdio.
	if *signChallenge != "" {
		sig, err := priv.Sign([]byte(*signChallenge))
		if err != nil {
			return fmt.Errorf("sign challenge: %w", err)
		}
		fmt.Println(base64.StdEncoding.EncodeToString(sig))
		return nil
	}

	pInput, err := readInput(*inPath)
	if err != nil {
		return fmt.Errorf("read input: %w", err)
	}

	// --evm mode: decode an EVMTxPayload and emit an EVMSignedTx (the
	// sign_evm_tx path). Mirrors the Cosmos TxPayload path below.
	if *evmMode {
		var ep payload.EVMTxPayload
		if err := json.Unmarshal(pInput, &ep); err != nil {
			return fmt.Errorf("decode EVMTxPayload: %w", err)
		}
		signed, err := signer.SignEVM(priv, &ep)
		if err != nil {
			return err
		}
		return writeOutput(*outPath, signed)
	}

	var p payload.TxPayload
	if err := json.Unmarshal(pInput, &p); err != nil {
		return fmt.Errorf("decode TxPayload: %w", err)
	}

	signed, err := signer.Sign(priv, &p)
	if err != nil {
		return err
	}
	return writeOutput(*outPath, signed)
}

// writeOutput marshals v as indented JSON to outPath (stdout when empty/"-").
func writeOutput(outPath string, v any) error {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal output: %w", err)
	}
	if outPath == "" || outPath == "-" {
		_, err = os.Stdout.Write(append(out, '\n'))
		return err
	}
	return os.WriteFile(outPath, append(out, '\n'), 0o600)
}

func readInput(path string) ([]byte, error) {
	if path == "" || path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}
