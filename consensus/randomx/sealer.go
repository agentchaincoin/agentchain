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
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"math/rand"
	"net/http"
	"runtime"
	"sync"
	"time"

	gorandomx "github.com/core-coin/go-randomx"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/types"
)

const (
	// staleThreshold is the maximum depth of the acceptable stale but valid solution.
	staleThreshold = 7
)

var (
	errNoMiningWork      = errors.New("no mining work available yet")
	errInvalidSealResult = errors.New("invalid or stale proof-of-work solution")
)

// Seal implements consensus.Engine, attempting to find a nonce that satisfies
// the block's difficulty requirements using RandomX.
func (rx *RandomX) Seal(chain consensus.ChainHeaderReader, block *types.Block, results chan<- *types.Block, stop <-chan struct{}) error {
	if rx.config.PowMode == ModeFake || rx.config.PowMode == ModeFullFake {
		header := block.Header()
		header.Nonce, header.MixDigest = types.BlockNonce{}, common.Hash{}
		select {
		case results <- block.WithSeal(header):
		default:
			rx.config.Log.Warn("Sealing result is not read by miner", "mode", "fake", "sealhash", rx.SealHash(block.Header()))
		}
		return nil
	}

	abort := make(chan struct{})

	rx.lock.Lock()
	threads := rx.threads
	if rx.rand == nil {
		seed, err := crand.Int(crand.Reader, big.NewInt(math.MaxInt64))
		if err != nil {
			rx.lock.Unlock()
			return err
		}
		rx.rand = rand.New(rand.NewSource(seed.Int64()))
	}
	rx.lock.Unlock()
	if threads == 0 {
		threads = runtime.NumCPU()
	}
	if threads < 0 {
		threads = 0
	}

	// Push new work to remote sealer
	if rx.remote != nil {
		rx.remote.workCh <- &sealTask{block: block, results: results}
	}
	var (
		pend   sync.WaitGroup
		locals = make(chan *types.Block)
	)
	for i := 0; i < threads; i++ {
		pend.Add(1)
		go func(id int, nonce uint64) {
			defer pend.Done()
			rx.mine(block, id, nonce, abort, locals)
		}(i, uint64(rx.rand.Int63()))
	}

	go func() {
		var result *types.Block
		select {
		case <-stop:
			close(abort)
		case result = <-locals:
			select {
			case results <- result:
			default:
				rx.config.Log.Warn("Sealing result is not read by miner", "mode", "local", "sealhash", rx.SealHash(block.Header()))
			}
			close(abort)
		case <-rx.update:
			close(abort)
			if err := rx.Seal(chain, block, results, stop); err != nil {
				rx.config.Log.Error("Failed to restart sealing after update", "err", err)
			}
		}
		pend.Wait()
	}()
	return nil
}

// mine is the actual proof-of-work miner that searches for a nonce using RandomX.
func (rx *RandomX) mine(block *types.Block, id int, seed uint64, abort chan struct{}, found chan *types.Block) {
	var (
		header = block.Header()
		hash   = rx.SealHash(header).Bytes()
		target = new(big.Int).Div(two256, header.Difficulty)
		number = header.Number.Uint64()
	)

	// Get a RandomX cache and dedicated VM for this mining thread
	cache := rx.ensureCache(number)
	if cache == nil {
		rx.config.Log.Error("Failed to get RandomX cache for mining", "block", number)
		return
	}

	// Acquire a dedicated VM for this thread
	vm, vmIdx := cache.acquireVM()
	defer cache.releaseVM(vmIdx)

	var (
		attempts  = int64(0)
		nonce     = seed
		powBuffer = new(big.Int)
	)
	logger := rx.config.Log.New("miner", id)
	logger.Trace("Started RandomX search for new nonces", "seed", seed)
search:
	for {
		select {
		case <-abort:
			logger.Trace("RandomX nonce search aborted", "attempts", nonce-seed)
			rx.hashrate.Mark(attempts)
			break search

		default:
			attempts++
			rx.hashCount.Add(1) // Simple counter for reliable hashrate
			// Report hashrate every 2^12 iterations (RandomX is slower than ethash)
			if (attempts % (1 << 12)) == 0 {
				rx.hashrate.Mark(attempts)
				attempts = 0
			}

			// Build input: SealHash(32 bytes) || nonce(8 bytes LE)
			input := make([]byte, 40)
			copy(input[:32], hash)
			binary.LittleEndian.PutUint64(input[32:], nonce)

			// Compute RandomX hash
			result := gorandomx.CalculateHash(vm, input)

			if powBuffer.SetBytes(result).Cmp(target) <= 0 {
				// Correct nonce found
				header = types.CopyHeader(header)
				header.Nonce = types.EncodeNonce(nonce)
				header.MixDigest = common.BytesToHash(result)

				select {
				case found <- block.WithSeal(header):
					logger.Trace("RandomX nonce found and reported", "attempts", nonce-seed, "nonce", nonce)
				case <-abort:
					logger.Trace("RandomX nonce found but discarded", "attempts", nonce-seed, "nonce", nonce)
				}
				break search
			}
			nonce++
		}
	}
}

// This is the timeout for HTTP requests to notify external miners.
const remoteSealerTimeout = 1 * time.Second

type remoteSealer struct {
	works        map[common.Hash]*types.Block
	rates        map[common.Hash]hashrate
	currentBlock *types.Block
	currentWork  [4]string
	notifyCtx    context.Context
	cancelNotify context.CancelFunc
	reqWG        sync.WaitGroup

	rx           *RandomX
	noverify     bool
	notifyURLs   []string
	results      chan<- *types.Block
	workCh       chan *sealTask
	fetchWorkCh  chan *sealWork
	submitWorkCh chan *mineResult
	fetchRateCh  chan chan uint64
	submitRateCh chan *hashrate
	requestExit  chan struct{}
	exitCh       chan struct{}
}

type sealTask struct {
	block   *types.Block
	results chan<- *types.Block
}

type mineResult struct {
	nonce     types.BlockNonce
	mixDigest common.Hash
	hash      common.Hash

	errc chan error
}

type hashrate struct {
	id   common.Hash
	ping time.Time
	rate uint64

	done chan struct{}
}

type sealWork struct {
	errc chan error
	res  chan [4]string
}

func startRemoteSealer(rx *RandomX, urls []string, noverify bool) *remoteSealer {
	ctx, cancel := context.WithCancel(context.Background())
	s := &remoteSealer{
		rx:           rx,
		noverify:     noverify,
		notifyURLs:   urls,
		notifyCtx:    ctx,
		cancelNotify: cancel,
		works:        make(map[common.Hash]*types.Block),
		rates:        make(map[common.Hash]hashrate),
		workCh:       make(chan *sealTask),
		fetchWorkCh:  make(chan *sealWork),
		submitWorkCh: make(chan *mineResult),
		fetchRateCh:  make(chan chan uint64),
		submitRateCh: make(chan *hashrate),
		requestExit:  make(chan struct{}),
		exitCh:       make(chan struct{}),
	}
	go s.loop()
	return s
}

func (s *remoteSealer) loop() {
	defer func() {
		s.rx.config.Log.Trace("RandomX remote sealer is exiting")
		s.cancelNotify()
		s.reqWG.Wait()
		close(s.exitCh)
	}()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case work := <-s.workCh:
			s.results = work.results
			s.makeWork(work.block)
			s.notifyWork()

		case work := <-s.fetchWorkCh:
			if s.currentBlock == nil {
				work.errc <- errNoMiningWork
			} else {
				work.res <- s.currentWork
			}

		case result := <-s.submitWorkCh:
			if s.submitWork(result.nonce, result.mixDigest, result.hash) {
				result.errc <- nil
			} else {
				result.errc <- errInvalidSealResult
			}

		case result := <-s.submitRateCh:
			s.rates[result.id] = hashrate{rate: result.rate, ping: time.Now()}
			close(result.done)

		case req := <-s.fetchRateCh:
			var total uint64
			for _, rate := range s.rates {
				total += rate.rate
			}
			req <- total

		case <-ticker.C:
			for id, rate := range s.rates {
				if time.Since(rate.ping) > 10*time.Second {
					delete(s.rates, id)
				}
			}
			if s.currentBlock != nil {
				for hash, block := range s.works {
					if block.NumberU64()+staleThreshold <= s.currentBlock.NumberU64() {
						delete(s.works, hash)
					}
				}
			}

		case <-s.requestExit:
			return
		}
	}
}

// makeWork creates a work package for external miner.
//
// The work package consists of 4 strings:
//
//	result[0], 32 bytes hex encoded current block header pow-hash
//	result[1], 32 bytes hex encoded seed key used for RandomX
//	result[2], 32 bytes hex encoded boundary condition ("target"), 2^256/difficulty
//	result[3], hex encoded block number
func (s *remoteSealer) makeWork(block *types.Block) {
	hash := s.rx.SealHash(block.Header())
	s.currentWork[0] = hash.Hex()
	s.currentWork[1] = common.BytesToHash(SeedKey(block.NumberU64())).Hex()
	s.currentWork[2] = common.BytesToHash(new(big.Int).Div(two256, block.Difficulty()).Bytes()).Hex()
	s.currentWork[3] = hexutil.EncodeBig(block.Number())

	s.currentBlock = block
	s.works[hash] = block
}

func (s *remoteSealer) notifyWork() {
	work := s.currentWork

	var blob []byte
	if s.rx.config.NotifyFull {
		blob, _ = json.Marshal(s.currentBlock.Header())
	} else {
		blob, _ = json.Marshal(work)
	}

	s.reqWG.Add(len(s.notifyURLs))
	for _, url := range s.notifyURLs {
		go s.sendNotification(s.notifyCtx, url, blob, work)
	}
}

func (s *remoteSealer) sendNotification(ctx context.Context, url string, json []byte, work [4]string) {
	defer s.reqWG.Done()

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(json))
	if err != nil {
		s.rx.config.Log.Warn("Can't create remote miner notification", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, remoteSealerTimeout)
	defer cancel()
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.rx.config.Log.Warn("Failed to notify remote miner", "err", err)
	} else {
		s.rx.config.Log.Trace("Notified remote miner", "miner", url, "hash", work[0], "target", work[2])
		resp.Body.Close()
	}
}

func (s *remoteSealer) submitWork(nonce types.BlockNonce, mixDigest common.Hash, sealhash common.Hash) bool {
	if s.currentBlock == nil {
		s.rx.config.Log.Error("Pending work without block", "sealhash", sealhash)
		return false
	}
	block := s.works[sealhash]
	if block == nil {
		s.rx.config.Log.Warn("Work submitted but none pending", "sealhash", sealhash, "curnumber", s.currentBlock.NumberU64())
		return false
	}
	header := block.Header()
	header.Nonce = nonce
	header.MixDigest = mixDigest

	start := time.Now()
	if !s.noverify {
		if err := s.rx.verifySeal(header); err != nil {
			s.rx.config.Log.Warn("Invalid proof-of-work submitted", "sealhash", sealhash, "elapsed", common.PrettyDuration(time.Since(start)), "err", err)
			return false
		}
	}
	if s.results == nil {
		s.rx.config.Log.Warn("RandomX result channel is empty, submitted mining result is rejected")
		return false
	}
	s.rx.config.Log.Trace("Verified correct proof-of-work", "sealhash", sealhash, "elapsed", common.PrettyDuration(time.Since(start)))

	solution := block.WithSeal(header)

	if solution.NumberU64()+staleThreshold > s.currentBlock.NumberU64() {
		select {
		case s.results <- solution:
			s.rx.config.Log.Debug("Work submitted is acceptable", "number", solution.NumberU64(), "sealhash", sealhash, "hash", solution.Hash())
			return true
		default:
			s.rx.config.Log.Warn("Sealing result is not read by miner", "mode", "remote", "sealhash", sealhash)
			return false
		}
	}
	s.rx.config.Log.Warn("Work submitted is too old", "number", solution.NumberU64(), "sealhash", sealhash, "hash", solution.Hash())
	return false
}
