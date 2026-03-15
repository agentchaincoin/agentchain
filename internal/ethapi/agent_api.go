// Copyright 2024 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package ethapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rpc"
)

// AgentAPI provides a key-free RPC interface for AI agents.
// Agents can create wallets, send transactions, deploy contracts, and mine
// without ever handling private keys or passwords.
type AgentAPI struct {
	b         Backend
	nonceLock *AddrLocker
}

// NewAgentAPI creates a new agent API instance.
func NewAgentAPI(b Backend, nonceLock *AddrLocker) *AgentAPI {
	return &AgentAPI{b: b, nonceLock: nonceLock}
}

// CreateWallet creates a new wallet with an internally generated password.
// The private key and password never leave the node process.
// Returns only the new address.
func (s *AgentAPI) CreateWallet() (common.Address, error) {
	// Generate a cryptographically random password (32 bytes -> 64 hex chars)
	passwordBytes := make([]byte, 32)
	if _, err := rand.Read(passwordBytes); err != nil {
		return common.Address{}, fmt.Errorf("failed to generate random password: %v", err)
	}
	password := hex.EncodeToString(passwordBytes)

	// Get the keystore
	ks, err := fetchKeystore(s.b.AccountManager())
	if err != nil {
		return common.Address{}, err
	}

	// Create the account
	acc, err := ks.NewAccount(password)
	if err != nil {
		return common.Address{}, fmt.Errorf("failed to create account: %v", err)
	}

	// Permanently unlock the account (duration=0 means forever)
	if err := ks.Unlock(acc, password); err != nil {
		return common.Address{}, fmt.Errorf("failed to unlock account: %v", err)
	}

	log.Info("Agent wallet created", "address", acc.Address)
	return acc.Address, nil
}

// AgentWalletInfo holds wallet information returned by ListWallets.
type AgentWalletInfo struct {
	Address common.Address `json:"address"`
	Balance *hexutil.Big   `json:"balance"`
}

// ListWallets returns all accounts managed by the node's keystore with their balances.
func (s *AgentAPI) ListWallets(ctx context.Context) ([]AgentWalletInfo, error) {
	// Get the keystore
	ks, err := fetchKeystore(s.b.AccountManager())
	if err != nil {
		return nil, err
	}

	accs := ks.Accounts()
	result := make([]AgentWalletInfo, 0, len(accs))

	// Get latest state for balance lookups
	state, _, err := s.b.StateAndHeaderByNumberOrHash(ctx, rpc.BlockNumberOrHashWithNumber(rpc.LatestBlockNumber))
	if err != nil {
		return nil, err
	}

	for _, acc := range accs {
		balance := state.GetBalance(acc.Address)
		result = append(result, AgentWalletInfo{
			Address: acc.Address,
			Balance: (*hexutil.Big)(balance),
		})
	}

	return result, nil
}

// Send sends CRD from one agent wallet to another.
// The from account must be unlocked (agent-created wallets are always unlocked).
func (s *AgentAPI) Send(ctx context.Context, from common.Address, to common.Address, value hexutil.Big) (common.Hash, error) {
	// Lock the nonce to prevent concurrent nonce assignment
	s.nonceLock.LockAddr(from)
	defer s.nonceLock.UnlockAddr(from)

	// Build transaction args
	args := TransactionArgs{
		From:  &from,
		To:    &to,
		Value: &value,
	}

	// Auto-fill gas, gasPrice, nonce
	if err := args.setDefaults(ctx, s.b); err != nil {
		return common.Hash{}, err
	}

	// Build the transaction
	tx := args.toTransaction()

	// Sign with the wallet
	signed, err := s.signTx(from, tx)
	if err != nil {
		return common.Hash{}, err
	}

	// Submit
	return SubmitTransaction(ctx, s.b, signed)
}

// DeployContract deploys a smart contract from an agent wallet.
// The from account must be unlocked. Bytecode should be hex-encoded contract bytecode.
// Value is optional (for payable constructors).
func (s *AgentAPI) DeployContract(ctx context.Context, from common.Address, bytecode hexutil.Bytes, value *hexutil.Big) (common.Hash, error) {
	// Lock the nonce
	s.nonceLock.LockAddr(from)
	defer s.nonceLock.UnlockAddr(from)

	// Build transaction args (To=nil means contract creation)
	args := TransactionArgs{
		From:  &from,
		To:    nil,
		Data:  &bytecode,
		Value: value,
	}

	if args.Value == nil {
		args.Value = new(hexutil.Big)
	}

	// Auto-fill
	if err := args.setDefaults(ctx, s.b); err != nil {
		return common.Hash{}, err
	}

	tx := args.toTransaction()

	signed, err := s.signTx(from, tx)
	if err != nil {
		return common.Hash{}, err
	}

	return SubmitTransaction(ctx, s.b, signed)
}

// CallContract performs a read-only contract call (no transaction, no gas cost).
func (s *AgentAPI) CallContract(ctx context.Context, to common.Address, data hexutil.Bytes) (hexutil.Bytes, error) {
	args := TransactionArgs{
		To:   &to,
		Data: &data,
	}

	blockNrOrHash := rpc.BlockNumberOrHashWithNumber(rpc.LatestBlockNumber)
	result, err := DoCall(ctx, s.b, args, blockNrOrHash, nil, s.b.RPCEVMTimeout(), s.b.RPCGasCap())
	if err != nil {
		return nil, err
	}

	if len(result.Revert()) > 0 {
		return nil, newRevertError(result)
	}
	return result.Return(), result.Err
}

// GetBalance returns the balance of the given address in wei.
func (s *AgentAPI) GetBalance(ctx context.Context, address common.Address) (*hexutil.Big, error) {
	state, _, err := s.b.StateAndHeaderByNumberOrHash(ctx, rpc.BlockNumberOrHashWithNumber(rpc.LatestBlockNumber))
	if state == nil || err != nil {
		return nil, err
	}
	return (*hexutil.Big)(state.GetBalance(address)), state.Error()
}

// GetTransactionReceipt returns the receipt of a transaction by transaction hash.
func (s *AgentAPI) GetTransactionReceipt(ctx context.Context, hash common.Hash) (map[string]interface{}, error) {
	tx, blockHash, blockNumber, index, err := s.b.GetTransaction(ctx, hash)
	if err != nil {
		return nil, nil
	}
	if tx == nil {
		return nil, nil
	}

	receipts, err := s.b.GetReceipts(ctx, blockHash)
	if err != nil {
		return nil, err
	}
	if len(receipts) <= int(index) {
		return nil, nil
	}
	receipt := receipts[index]

	// Derive sender
	signer := types.MakeSigner(s.b.ChainConfig(), new(big.Int).SetUint64(blockNumber))
	from, _ := types.Sender(signer, tx)

	fields := map[string]interface{}{
		"blockHash":         blockHash,
		"blockNumber":       hexutil.Uint64(blockNumber),
		"transactionHash":   hash,
		"transactionIndex":  hexutil.Uint64(index),
		"from":              from,
		"to":                tx.To(),
		"gasUsed":           hexutil.Uint64(receipt.GasUsed),
		"cumulativeGasUsed": hexutil.Uint64(receipt.CumulativeGasUsed),
		"contractAddress":   nil,
		"logs":              receipt.Logs,
		"logsBloom":         receipt.Bloom,
		"type":              hexutil.Uint(tx.Type()),
		"effectiveGasPrice": (*hexutil.Big)(receipt.EffectiveGasPrice),
	}

	if len(receipt.PostState) > 0 {
		fields["root"] = hexutil.Bytes(receipt.PostState)
	} else {
		fields["status"] = hexutil.Uint(receipt.Status)
	}
	if receipt.Logs == nil {
		fields["logs"] = []*types.Log{}
	}
	if receipt.ContractAddress != (common.Address{}) {
		fields["contractAddress"] = receipt.ContractAddress
	}
	return fields, nil
}

// StartMining starts the miner with the given etherbase address and number of threads.
func (s *AgentAPI) StartMining(ctx context.Context, etherbase common.Address, threads *int) (bool, error) {
	s.b.SetEtherbase(etherbase)

	t := 1
	if threads != nil {
		t = *threads
	}
	if err := s.b.StartMining(t); err != nil {
		return false, err
	}
	return true, nil
}

// StopMining stops the miner.
func (s *AgentAPI) StopMining() (bool, error) {
	s.b.StopMining()
	return true, nil
}

// signTx signs a transaction with the wallet that holds the given address.
func (s *AgentAPI) signTx(addr common.Address, tx *types.Transaction) (*types.Transaction, error) {
	account := accounts.Account{Address: addr}
	wallet, err := s.b.AccountManager().Find(account)
	if err != nil {
		return nil, fmt.Errorf("unknown account %s: %v", addr.Hex(), err)
	}
	return wallet.SignTx(account, tx, s.b.ChainConfig().ChainID)
}

