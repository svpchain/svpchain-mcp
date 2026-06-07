package chain

import (
	"context"
	"fmt"
	"math/big"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

// EVMClient is the contract-agnostic surface the EVM tool family needs over
// svpchain's EVM JSON-RPC (eth_*). It is deliberately small — only the calls
// the build / broadcast / status path actually use — and is an interface so
// handlers and the assembler can be unit-tested against a mock.
//
// Reads (CallContract) and the chain-state calls (nonce/gas/fee/chainID) feed
// the EVMAssembler when building a tx; SendTransaction + TransactionReceipt
// drive broadcast_evm_tx / evm_tx_status. The signer (separate process) never
// touches this — all chain I/O stays on the remote.
type EVMClient interface {
	// PendingNonceAt returns the next nonce for account, counting txs already
	// in the mempool ("pending"), so back-to-back builds don't collide.
	PendingNonceAt(ctx context.Context, account common.Address) (uint64, error)
	// EstimateGas simulates the call and returns the gas it would consume.
	EstimateGas(ctx context.Context, msg ethereum.CallMsg) (uint64, error)
	// SuggestGasTipCap returns a suggested EIP-1559 priority fee (tip).
	SuggestGasTipCap(ctx context.Context) (*big.Int, error)
	// BaseFee returns the base fee of the latest block (nil if the chain is
	// pre-EIP-1559, which svpchain is not).
	BaseFee(ctx context.Context) (*big.Int, error)
	// ChainID returns the EVM chain id (svpchain default 262144).
	ChainID(ctx context.Context) (*big.Int, error)
	// CallContract executes a read-only eth_call against the latest block.
	CallContract(ctx context.Context, msg ethereum.CallMsg) ([]byte, error)
	// SendTransaction submits an already-signed tx (eth_sendRawTransaction
	// under the hood) and returns its hash.
	SendTransaction(ctx context.Context, tx *ethtypes.Transaction) (string, error)
	// TransactionReceipt looks up a mined tx's receipt by hash. Returns
	// (nil, ethereum.NotFound) when the tx is not yet included.
	TransactionReceipt(ctx context.Context, txHash common.Hash) (*ethtypes.Receipt, error)
}

type evmClient struct {
	inner *ethclient.Client
}

// NewEVMClient dials the EVM JSON-RPC at rpcURL (e.g. "http://127.0.0.1:8545").
func NewEVMClient(ctx context.Context, rpcURL string) (EVMClient, error) {
	c, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return nil, fmt.Errorf("ethclient.Dial %s: %w", rpcURL, err)
	}
	return &evmClient{inner: c}, nil
}

func (c *evmClient) PendingNonceAt(ctx context.Context, account common.Address) (uint64, error) {
	return c.inner.PendingNonceAt(ctx, account)
}

func (c *evmClient) EstimateGas(ctx context.Context, msg ethereum.CallMsg) (uint64, error) {
	return c.inner.EstimateGas(ctx, msg)
}

func (c *evmClient) SuggestGasTipCap(ctx context.Context) (*big.Int, error) {
	return c.inner.SuggestGasTipCap(ctx)
}

func (c *evmClient) BaseFee(ctx context.Context) (*big.Int, error) {
	head, err := c.inner.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("header by number: %w", err)
	}
	if head.BaseFee == nil {
		return nil, fmt.Errorf("latest block has no base fee (pre-EIP-1559 chain?)")
	}
	return head.BaseFee, nil
}

func (c *evmClient) ChainID(ctx context.Context) (*big.Int, error) {
	return c.inner.ChainID(ctx)
}

func (c *evmClient) CallContract(ctx context.Context, msg ethereum.CallMsg) ([]byte, error) {
	return c.inner.CallContract(ctx, msg, nil)
}

func (c *evmClient) SendTransaction(ctx context.Context, tx *ethtypes.Transaction) (string, error) {
	if err := c.inner.SendTransaction(ctx, tx); err != nil {
		return "", err
	}
	return tx.Hash().Hex(), nil
}

func (c *evmClient) TransactionReceipt(ctx context.Context, txHash common.Hash) (*ethtypes.Receipt, error) {
	return c.inner.TransactionReceipt(ctx, txHash)
}
