// Copyright 2024 The AgentChain Authors
// This file is part of the AgentChain library.
//
// The AgentChain library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The AgentChain library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the AgentChain library. If not, see <http://www.gnu.org/licenses/>.

package randomx

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"runtime"
	"time"

	mapset "github.com/deckarep/golang-set/v2"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/misc"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"golang.org/x/crypto/sha3"

	gorandomx "github.com/core-coin/go-randomx"
	"github.com/holiman/uint256"
)

// RandomX proof-of-work protocol constants.
var (
	maxUncles                     = 2       // Maximum number of uncles allowed in a single block
	allowedFutureBlockTimeSeconds = int64(9) // Max seconds from current time allowed for blocks (1.5x of 6s target)
)

// Various error messages to mark blocks invalid.
var (
	errOlderBlockTime    = errors.New("timestamp older than parent")
	errTooManyUncles     = errors.New("too many uncles")
	errDuplicateUncle    = errors.New("duplicate uncle")
	errUncleIsAncestor   = errors.New("uncle is ancestor")
	errDanglingUncle     = errors.New("uncle's parent is not ancestor")
	errInvalidDifficulty = errors.New("non-positive difficulty")
	errInvalidMixDigest  = errors.New("invalid mix digest")
	errInvalidPoW        = errors.New("invalid proof-of-work")
)

// Author implements consensus.Engine, returning the header's coinbase as the
// proof-of-work verified author of the block.
func (rx *RandomX) Author(header *types.Header) (common.Address, error) {
	return header.Coinbase, nil
}

// VerifyHeader checks whether a header conforms to the consensus rules of the
// RandomX engine.
func (rx *RandomX) VerifyHeader(chain consensus.ChainHeaderReader, header *types.Header, seal bool) error {
	if rx.config.PowMode == ModeFullFake {
		return nil
	}
	number := header.Number.Uint64()
	if chain.GetHeader(header.Hash(), number) != nil {
		return nil
	}
	parent := chain.GetHeader(header.ParentHash, number-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}
	return rx.verifyHeader(chain, header, parent, false, seal, time.Now().Unix())
}

// VerifyHeaders is similar to VerifyHeader, but verifies a batch of headers
// concurrently.
func (rx *RandomX) VerifyHeaders(chain consensus.ChainHeaderReader, headers []*types.Header, seals []bool) (chan<- struct{}, <-chan error) {
	if rx.config.PowMode == ModeFullFake || len(headers) == 0 {
		abort, results := make(chan struct{}), make(chan error, len(headers))
		for i := 0; i < len(headers); i++ {
			results <- nil
		}
		return abort, results
	}

	workers := runtime.GOMAXPROCS(0)
	// On Windows, RandomX CGo calls crash when multiple goroutines invoke
	// randomx_calculate_hash concurrently. Serialize verification.
	if runtime.GOOS == "windows" {
		workers = 1
	}
	if len(headers) < workers {
		workers = len(headers)
	}

	var (
		inputs  = make(chan int)
		done    = make(chan int, workers)
		errs    = make([]error, len(headers))
		abort   = make(chan struct{})
		unixNow = time.Now().Unix()
	)
	for i := 0; i < workers; i++ {
		go func() {
			for index := range inputs {
				errs[index] = rx.verifyHeaderWorker(chain, headers, seals, index, unixNow)
				done <- index
			}
		}()
	}

	errorsOut := make(chan error, len(headers))
	go func() {
		defer close(inputs)
		var (
			in, out = 0, 0
			checked = make([]bool, len(headers))
			inputs  = inputs
		)
		for {
			select {
			case inputs <- in:
				if in++; in == len(headers) {
					inputs = nil
				}
			case index := <-done:
				for checked[index] = true; checked[out]; out++ {
					errorsOut <- errs[out]
					if out == len(headers)-1 {
						return
					}
				}
			case <-abort:
				return
			}
		}
	}()
	return abort, errorsOut
}

func (rx *RandomX) verifyHeaderWorker(chain consensus.ChainHeaderReader, headers []*types.Header, seals []bool, index int, unixNow int64) error {
	var parent *types.Header
	if index == 0 {
		parent = chain.GetHeader(headers[0].ParentHash, headers[0].Number.Uint64()-1)
	} else if headers[index-1].Hash() == headers[index].ParentHash {
		parent = headers[index-1]
	}
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}
	return rx.verifyHeader(chain, headers[index], parent, false, seals[index], unixNow)
}

// VerifyUncles verifies that the given block's uncles conform to the consensus rules.
func (rx *RandomX) VerifyUncles(chain consensus.ChainReader, block *types.Block) error {
	if rx.config.PowMode == ModeFullFake {
		return nil
	}
	maxAllowed := maxUncles
	if chain.Config().IsReducedUncleReward(block.Number()) {
		maxAllowed = 1
	}
	if len(block.Uncles()) > maxAllowed {
		return errTooManyUncles
	}
	if len(block.Uncles()) == 0 {
		return nil
	}

	uncles, ancestors := mapset.NewSet[common.Hash](), make(map[common.Hash]*types.Header)

	number, parent := block.NumberU64()-1, block.ParentHash()
	for i := 0; i < 7; i++ {
		ancestorHeader := chain.GetHeader(parent, number)
		if ancestorHeader == nil {
			break
		}
		ancestors[parent] = ancestorHeader
		if ancestorHeader.UncleHash != types.EmptyUncleHash {
			ancestor := chain.GetBlock(parent, number)
			if ancestor == nil {
				break
			}
			for _, uncle := range ancestor.Uncles() {
				uncles.Add(uncle.Hash())
			}
		}
		parent, number = ancestorHeader.ParentHash, number-1
	}
	ancestors[block.Hash()] = block.Header()
	uncles.Add(block.Hash())

	for _, uncle := range block.Uncles() {
		hash := uncle.Hash()
		if uncles.Contains(hash) {
			return errDuplicateUncle
		}
		uncles.Add(hash)

		if ancestors[hash] != nil {
			return errUncleIsAncestor
		}
		if ancestors[uncle.ParentHash] == nil || uncle.ParentHash == block.ParentHash() {
			return errDanglingUncle
		}
		if err := rx.verifyHeader(chain, uncle, ancestors[uncle.ParentHash], true, true, time.Now().Unix()); err != nil {
			return err
		}
	}
	return nil
}

// verifyHeader checks whether a header conforms to the consensus rules.
func (rx *RandomX) verifyHeader(chain consensus.ChainHeaderReader, header, parent *types.Header, uncle bool, seal bool, unixNow int64) error {
	if uint64(len(header.Extra)) > params.MaximumExtraDataSize {
		return fmt.Errorf("extra-data too long: %d > %d", len(header.Extra), params.MaximumExtraDataSize)
	}
	if !uncle {
		if header.Time > uint64(unixNow+allowedFutureBlockTimeSeconds) {
			return consensus.ErrFutureBlock
		}
	}
	if header.Time <= parent.Time {
		return errOlderBlockTime
	}
	expected := rx.CalcDifficulty(chain, header.Time, parent)
	if expected.Cmp(header.Difficulty) != 0 {
		return fmt.Errorf("invalid difficulty: have %v, want %v", header.Difficulty, expected)
	}
	if header.GasLimit > params.MaxGasLimit {
		return fmt.Errorf("invalid gasLimit: have %v, max %v", header.GasLimit, params.MaxGasLimit)
	}
	if header.GasUsed > header.GasLimit {
		return fmt.Errorf("invalid gasUsed: have %d, gasLimit %d", header.GasUsed, header.GasLimit)
	}
	if !chain.Config().IsLondon(header.Number) {
		if header.BaseFee != nil {
			return fmt.Errorf("invalid baseFee before fork: have %d, expected 'nil'", header.BaseFee)
		}
		if err := misc.VerifyGaslimit(parent.GasLimit, header.GasLimit); err != nil {
			return err
		}
	} else if err := misc.VerifyEip1559Header(chain.Config(), parent, header); err != nil {
		return err
	}
	if chain.Config().IsGasLimitBounds(header.Number) {
		if err := misc.VerifyGaslimitBounds(header.GasLimit); err != nil {
			return err
		}
	}
	if diff := new(big.Int).Sub(header.Number, parent.Number); diff.Cmp(big.NewInt(1)) != 0 {
		return consensus.ErrInvalidNumber
	}
	if chain.Config().IsShanghai(header.Time) {
		return fmt.Errorf("randomx does not support shanghai fork")
	}
	if chain.Config().IsCancun(header.Time) {
		return fmt.Errorf("randomx does not support cancun fork")
	}
	if seal {
		if err := rx.verifySeal(header); err != nil {
			return err
		}
	}
	if err := misc.VerifyDAOHeaderExtraData(chain.Config(), header); err != nil {
		return err
	}
	return nil
}

// CalcDifficulty is the difficulty adjustment algorithm.
func (rx *RandomX) CalcDifficulty(chain consensus.ChainHeaderReader, time uint64, parent *types.Header) *big.Int {
	return CalcDifficulty(chain.Config(), time, parent)
}

// CalcDifficulty returns the difficulty that a new block should have.
func CalcDifficulty(config *params.ChainConfig, time uint64, parent *types.Header) *big.Int {
	if config.ChainID != nil && config.ChainID.Cmp(params.AgentChainChainID) == 0 {
		return CalcDifficultyAgentChain(time, parent)
	}
	// Fallback for non-AgentChain (shouldn't happen but safe)
	return CalcDifficultyAgentChain(time, parent)
}

// AgentChain difficulty parameters
const (
	agentChainBlockTarget   = 6  // 6-second block time target
	agentChainMinDifficulty = 256 // Must be >= difficultyBoundDivisor for adjustment to work
	difficultyBoundDivisor  = 8  // Right-shifts for division by 256 (faster adjustment for new chain)
)

// CalcDifficultyAgentChain is the difficulty adjustment algorithm for AgentChain.
// Based on Byzantium formula but with divisor 6 (targeting 6-second blocks)
// and no difficulty bomb.
func CalcDifficultyAgentChain(time uint64, parent *types.Header) *big.Int {
	x := (time - parent.Time) / agentChainBlockTarget
	c := uint64(1)
	if parent.UncleHash != types.EmptyUncleHash {
		c = 2
	}
	xNeg := x >= c
	if xNeg {
		x = x - c
	} else {
		x = c - x
	}
	if x > 99 {
		x = 99
	}

	y := new(uint256.Int)
	y.SetFromBig(parent.Difficulty)
	pDiffClone := y.Clone()
	z := new(uint256.Int).SetUint64(x)
	y.Rsh(y, difficultyBoundDivisor) // y: p_diff / 2048
	z.Mul(y, z)

	if xNeg {
		y.Sub(pDiffClone, z)
	} else {
		y.Add(pDiffClone, z)
	}
	if y.LtUint64(agentChainMinDifficulty) {
		y.SetUint64(agentChainMinDifficulty)
	}
	return y.ToBig()
}

// verifySeal checks whether a block satisfies the PoW difficulty requirements
// using RandomX.
func (rx *RandomX) verifySeal(header *types.Header) error {
	if rx.config.PowMode == ModeFake || rx.config.PowMode == ModeFullFake {
		time.Sleep(rx.fakeDelay)
		if rx.fakeFail == header.Number.Uint64() {
			return errInvalidPoW
		}
		return nil
	}
	if header.Difficulty.Sign() <= 0 {
		return errInvalidDifficulty
	}

	number := header.Number.Uint64()
	epoch := number / SeedEpochLength
	blockInEpoch := number % SeedEpochLength

	// Build the input: SealHash(32 bytes) || nonce(8 bytes LE)
	sealHash := rx.SealHash(header)
	input := make([]byte, 40)
	copy(input[:32], sealHash.Bytes())
	binary.LittleEndian.PutUint64(input[32:], header.Nonce.Uint64())

	// Get cache and compute hash
	cache := rx.ensureCache(number)
	if cache == nil {
		return errors.New("failed to initialize RandomX cache")
	}

	vm, vmIdx := cache.acquireVM()
	result := gorandomx.CalculateHash(vm, input)
	cache.releaseVM(vmIdx)

	// Verify MixDigest matches the result
	if !bytes.Equal(header.MixDigest[:], result) {
		// If we're in the grace period, try with previous epoch key
		if epoch > 0 && blockInEpoch < SeedGracePeriod {
			prevCache := rx.ensureCacheForEpoch(epoch - 1)
			if prevCache != nil {
				vm2, vmIdx2 := prevCache.acquireVM()
				result2 := gorandomx.CalculateHash(vm2, input)
				prevCache.releaseVM(vmIdx2)

				if bytes.Equal(header.MixDigest[:], result2) {
					// Check PoW with previous epoch result
					target := new(big.Int).Div(two256, header.Difficulty)
					if new(big.Int).SetBytes(result2).Cmp(target) > 0 {
						return errInvalidPoW
					}
					return nil
				}
			}
		}
		return errInvalidMixDigest
	}

	// Verify result < 2^256/difficulty
	target := new(big.Int).Div(two256, header.Difficulty)
	if new(big.Int).SetBytes(result).Cmp(target) > 0 {
		return errInvalidPoW
	}
	return nil
}

// Prepare implements consensus.Engine, initializing the difficulty field of a
// header to conform to the protocol.
func (rx *RandomX) Prepare(chain consensus.ChainHeaderReader, header *types.Header) error {
	parent := chain.GetHeader(header.ParentHash, header.Number.Uint64()-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}
	header.Difficulty = rx.CalcDifficulty(chain, header.Time, parent)
	return nil
}

// Finalize implements consensus.Engine, accumulating the block and uncle rewards.
func (rx *RandomX) Finalize(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB, txs []*types.Transaction, uncles []*types.Header, withdrawals []*types.Withdrawal) {
	accumulateRewards(chain.Config(), state, header, uncles)
}

// FinalizeAndAssemble implements consensus.Engine, accumulating the block and
// uncle rewards, setting the final state and assembling the block.
func (rx *RandomX) FinalizeAndAssemble(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB, txs []*types.Transaction, uncles []*types.Header, receipts []*types.Receipt, withdrawals []*types.Withdrawal) (*types.Block, error) {
	if len(withdrawals) > 0 {
		return nil, errors.New("randomx does not support withdrawals")
	}
	rx.Finalize(chain, header, state, txs, uncles, nil)
	header.Root = state.IntermediateRoot(chain.Config().IsEIP158(header.Number))
	return types.NewBlock(header, txs, uncles, receipts, trie.NewStackTrie(nil)), nil
}

// SealHash returns the hash of a block prior to it being sealed.
func (rx *RandomX) SealHash(header *types.Header) (hash common.Hash) {
	hasher := sha3.NewLegacyKeccak256()

	enc := []interface{}{
		header.ParentHash,
		header.UncleHash,
		header.Coinbase,
		header.Root,
		header.TxHash,
		header.ReceiptHash,
		header.Bloom,
		header.Difficulty,
		header.Number,
		header.GasLimit,
		header.GasUsed,
		header.Time,
		header.Extra,
	}
	if header.BaseFee != nil {
		enc = append(enc, header.BaseFee)
	}
	if header.WithdrawalsHash != nil {
		panic("withdrawal hash set on randomx")
	}
	rlp.Encode(hasher, enc)
	hasher.Sum(hash[:0])
	return hash
}

// Some constants for reward calculations.
var (
	big8  = big.NewInt(8)
	big32 = big.NewInt(32)
)

// AgentChain inflation constants
var (
	agentChainEpochLength   = big.NewInt(5_256_000) // ~1 year at 6s blocks
	agentChainInitialReward = big.NewInt(2e+18)     // 2 CRD (Credits) in wei
	agentChainInflationNum  = big.NewInt(102)        // numerator for 2% increase
	agentChainInflationDen  = big.NewInt(100)        // denominator for 2% increase
)

// calcAgentChainBlockReward returns the block reward for AgentChain.
// Each epoch (5,256,000 blocks ~= 1 year), reward increases by 2%.
func calcAgentChainBlockReward(blockNumber *big.Int) *big.Int {
	epoch := new(big.Int).Div(blockNumber, agentChainEpochLength)
	reward := new(big.Int).Set(agentChainInitialReward)
	for i := int64(0); i < epoch.Int64(); i++ {
		reward.Mul(reward, agentChainInflationNum)
		reward.Div(reward, agentChainInflationDen)
	}
	return reward
}

// accumulateRewards credits the coinbase with block and uncle rewards.
func accumulateRewards(config *params.ChainConfig, stateDB *state.StateDB, header *types.Header, uncles []*types.Header) {
	var blockReward *big.Int
	if config.ChainID != nil && config.ChainID.Cmp(params.AgentChainChainID) == 0 {
		blockReward = calcAgentChainBlockReward(header.Number)
	} else {
		blockReward = new(big.Int).Set(big.NewInt(2e+18)) // default 2 ETH
	}

	reward := new(big.Int).Set(blockReward)
	r := new(big.Int)

	reducedUncles := config.IsReducedUncleReward(header.Number)
	for _, uncle := range uncles {
		if reducedUncles {
			r.Div(blockReward, big32)
			stateDB.AddBalance(uncle.Coinbase, r)
		} else {
			r.Add(uncle.Number, big8)
			r.Sub(r, header.Number)
			r.Mul(r, blockReward)
			r.Div(r, big8)
			stateDB.AddBalance(uncle.Coinbase, r)
		}
		r.Div(blockReward, big32)
		reward.Add(reward, r)
	}
	stateDB.AddBalance(header.Coinbase, reward)
}

// Exported difficulty functions for compatibility
var FrontierDifficultyCalculator = CalcDifficultyAgentChain
