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

// Package randomx implements the RandomX proof-of-work consensus engine,
// replacing Ethash for CPU-friendly mining suitable for AI agents on VPS.
package randomx

import (
	"encoding/binary"
	"math/big"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	gorandomx "github.com/core-coin/go-randomx"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/rpc"
	"golang.org/x/crypto/sha3"
)

var (
	// two256 is a big integer representing 2^256
	two256 = new(big.Int).Exp(big.NewInt(2), big.NewInt(256), big.NewInt(0))
)

const (
	// SeedEpochLength is the number of blocks per RandomX epoch (~3.4 hours at 6s).
	SeedEpochLength = 2048

	// SeedGracePeriod is the number of blocks at the start of each epoch
	// during which the previous epoch's key is also accepted.
	SeedGracePeriod = 64
)

// Mode defines the type and amount of PoW verification a RandomX engine makes.
type Mode uint

const (
	ModeNormal Mode = iota
	ModeTest
	ModeFake
	ModeFullFake
)

// Config are the configuration parameters of the RandomX engine.
type Config struct {
	PowMode       Mode
	Threads       int  // Mining threads (0 = all CPUs)
	UseJIT        bool // JIT compilation (default true)
	UseLargePages bool // Large memory pages

	// When set, notifications sent by the remote sealer will
	// be block header JSON objects instead of work package arrays.
	NotifyFull bool

	Log log.Logger `toml:"-"`
}

// rxCache holds a RandomX cache/dataset for a specific epoch, plus a pool of VMs.
type rxCache struct {
	epoch   uint64
	key     []byte
	cache   gorandomx.Cache
	dataset gorandomx.Dataset
	vms     []gorandomx.VM
	vmsMu   []sync.Mutex
}

// RandomX is a consensus engine based on proof-of-work implementing the RandomX
// algorithm for CPU-friendly mining.
type RandomX struct {
	config Config

	cacheMu      sync.RWMutex
	currentCache *rxCache // Current epoch cache
	futureCache  *rxCache // Pre-computed next epoch

	// Mining related fields
	rand     *rand.Rand    // Properly seeded random source for nonces
	threads  int           // Number of threads to mine on if mining
	update   chan struct{}  // Notification channel to update mining parameters
	hashrate metrics.Meter // Meter tracking the average hashrate
	remote   *remoteSealer

	// Simple hashrate tracking (metrics.Meter EWMA can round to 0 for slow PoW)
	hashCount atomic.Int64 // Total hashes since last snapshot
	hashSnap  atomic.Int64 // Hashes/sec at last snapshot
	hashTime  atomic.Int64 // Unix nano of last snapshot

	// The fields below are hooks for testing
	fakeFail  uint64        // Block number which fails PoW check even in fake mode
	fakeDelay time.Duration // Time delay to sleep for before returning from verify

	lock      sync.Mutex // Ensures thread safety for the in-memory caches and mining fields
	closeOnce sync.Once  // Ensures exit channel will not be closed twice.
}

// New creates a full RandomX PoW engine and starts a background thread for
// remote mining, also optionally notifying a batch of remote services of new work.
func New(config Config, notify []string, noverify bool) *RandomX {
	if config.Log == nil {
		config.Log = log.Root()
	}
	rx := &RandomX{
		config:   config,
		update:   make(chan struct{}),
		hashrate: metrics.NewMeterForced(),
	}
	rx.remote = startRemoteSealer(rx, notify, noverify)
	return rx
}

// NewTester creates a small sized RandomX PoW scheme useful only for testing.
func NewTester(notify []string, noverify bool) *RandomX {
	return New(Config{PowMode: ModeTest, Log: log.Root()}, notify, noverify)
}

// NewFaker creates a RandomX consensus engine with a fake PoW scheme that accepts
// all blocks' seal as valid, though they still have to conform to the consensus rules.
func NewFaker() *RandomX {
	return &RandomX{
		config: Config{
			PowMode: ModeFake,
			Log:     log.Root(),
		},
	}
}

// NewFakeFailer creates a RandomX consensus engine with a fake PoW scheme that
// accepts all blocks as valid apart from the single one specified.
func NewFakeFailer(fail uint64) *RandomX {
	return &RandomX{
		config: Config{
			PowMode: ModeFake,
			Log:     log.Root(),
		},
		fakeFail: fail,
	}
}

// NewFakeDelayer creates a RandomX consensus engine with a fake PoW scheme that
// accepts all blocks as valid, but delays verifications by some time.
func NewFakeDelayer(delay time.Duration) *RandomX {
	return &RandomX{
		config: Config{
			PowMode: ModeFake,
			Log:     log.Root(),
		},
		fakeDelay: delay,
	}
}

// NewFullFaker creates a RandomX consensus engine with a full fake scheme that
// accepts all blocks as valid, without checking any consensus rules whatsoever.
func NewFullFaker() *RandomX {
	return &RandomX{
		config: Config{
			PowMode: ModeFullFake,
			Log:     log.Root(),
		},
	}
}

// Close closes the exit channel to notify all backend threads exiting.
func (rx *RandomX) Close() error {
	rx.closeOnce.Do(func() {
		if rx.remote != nil {
			close(rx.remote.requestExit)
			<-rx.remote.exitCh
		}
		rx.cacheMu.Lock()
		defer rx.cacheMu.Unlock()
		if rx.currentCache != nil {
			destroyRxCache(rx.currentCache)
			rx.currentCache = nil
		}
		if rx.futureCache != nil {
			destroyRxCache(rx.futureCache)
			rx.futureCache = nil
		}
	})
	return nil
}

// SeedKey returns the RandomX seed key for the given block number.
// seedKey = keccak256("AgentChain-RandomX-v1" || le_uint64(epoch))
func SeedKey(blockNumber uint64) []byte {
	epoch := blockNumber / SeedEpochLength
	return seedKeyForEpoch(epoch)
}

func seedKeyForEpoch(epoch uint64) []byte {
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte("AgentChain-RandomX-v1"))
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], epoch)
	h.Write(buf[:])
	return h.Sum(nil)
}

// ensureCache returns a RandomX cache for the given block number.
// It lazily initializes caches and handles epoch transitions.
func (rx *RandomX) ensureCache(blockNumber uint64) *rxCache {
	epoch := blockNumber / SeedEpochLength

	rx.cacheMu.Lock()
	defer rx.cacheMu.Unlock()

	// Check if current cache is for the right epoch
	if rx.currentCache != nil && rx.currentCache.epoch == epoch {
		return rx.currentCache
	}

	// Check if future cache is what we need
	if rx.futureCache != nil && rx.futureCache.epoch == epoch {
		// Promote future cache to current
		old := rx.currentCache
		rx.currentCache = rx.futureCache
		rx.futureCache = nil
		if old != nil {
			go destroyRxCache(old)
		}
		// Pre-compute next epoch in background
		go rx.precomputeCache(epoch + 1)
		return rx.currentCache
	}

	// Need to create a new cache
	old := rx.currentCache
	rx.currentCache = rx.createRxCache(epoch)
	if old != nil {
		go destroyRxCache(old)
	}
	if rx.futureCache != nil {
		go destroyRxCache(rx.futureCache)
		rx.futureCache = nil
	}
	// Pre-compute next epoch in background
	go rx.precomputeCache(epoch + 1)
	return rx.currentCache
}

// ensureCacheForEpoch returns a cache specifically for the given epoch.
// Used during grace period when we need the previous epoch's cache.
func (rx *RandomX) ensureCacheForEpoch(epoch uint64) *rxCache {
	rx.cacheMu.Lock()
	defer rx.cacheMu.Unlock()

	if rx.currentCache != nil && rx.currentCache.epoch == epoch {
		return rx.currentCache
	}
	if rx.futureCache != nil && rx.futureCache.epoch == epoch {
		return rx.futureCache
	}

	// Create a temporary cache for this epoch
	return rx.createRxCache(epoch)
}

func (rx *RandomX) precomputeCache(epoch uint64) {
	rx.cacheMu.Lock()
	defer rx.cacheMu.Unlock()

	// Don't overwrite an existing future cache
	if rx.futureCache != nil {
		return
	}
	rx.config.Log.Info("Pre-computing RandomX cache for next epoch", "epoch", epoch)
	rx.futureCache = rx.createRxCache(epoch)
}

func (rx *RandomX) createRxCache(epoch uint64) *rxCache {
	key := seedKeyForEpoch(epoch)

	cache, err := gorandomx.AllocCache(gorandomx.GetFlags())
	if err != nil {
		rx.config.Log.Error("Failed to allocate RandomX cache", "err", err)
		return nil
	}
	gorandomx.InitCache(cache, key)

	dataset, err := gorandomx.AllocDataset(gorandomx.GetFlags())
	if err != nil {
		rx.config.Log.Error("Failed to allocate RandomX dataset", "err", err)
		gorandomx.ReleaseCache(cache)
		return nil
	}
	count := gorandomx.DatasetItemCount()
	gorandomx.InitDataset(dataset, cache, 0, count)

	numVMs := runtime.NumCPU()
	if numVMs < 1 {
		numVMs = 1
	}

	vms := make([]gorandomx.VM, numVMs)
	vmsMu := make([]sync.Mutex, numVMs)
	flags := gorandomx.GetFlags()
	for i := 0; i < numVMs; i++ {
		vm, err := gorandomx.CreateVM(cache, dataset, flags)
		if err != nil {
			// Fallback: retry without JIT if JIT-based VM creation fails
			rx.config.Log.Warn("RandomX VM creation failed with JIT, retrying without JIT", "err", err, "index", i)
			noJIT := flags &^ gorandomx.FlagJIT &^ gorandomx.FlagSecure
			vm, err = gorandomx.CreateVM(cache, dataset, noJIT)
		}
		if err != nil {
			rx.config.Log.Error("Failed to create RandomX VM", "err", err, "index", i)
			for j := 0; j < i; j++ {
				gorandomx.DestroyVM(vms[j])
			}
			gorandomx.ReleaseDataset(dataset)
			gorandomx.ReleaseCache(cache)
			return nil
		}
		vms[i] = vm
	}

	rx.config.Log.Info("RandomX cache initialized", "epoch", epoch, "vms", numVMs)

	return &rxCache{
		epoch:   epoch,
		key:     key,
		cache:   cache,
		dataset: dataset,
		vms:     vms,
		vmsMu:   vmsMu,
	}
}

func destroyRxCache(c *rxCache) {
	if c == nil {
		return
	}
	for _, vm := range c.vms {
		gorandomx.DestroyVM(vm)
	}
	if c.dataset != nil {
		gorandomx.ReleaseDataset(c.dataset)
	}
	gorandomx.ReleaseCache(c.cache)
}

// acquireVM returns a VM and its index from the pool. The caller must call
// releaseVM when done.
func (c *rxCache) acquireVM() (gorandomx.VM, int) {
	// Try to get a free VM without blocking
	for i := range c.vms {
		if c.vmsMu[i].TryLock() {
			return c.vms[i], i
		}
	}
	// All VMs busy, block on the first one
	c.vmsMu[0].Lock()
	return c.vms[0], 0
}

// releaseVM releases a VM back to the pool.
func (c *rxCache) releaseVM(index int) {
	c.vmsMu[index].Unlock()
}

// Threads returns the number of mining threads currently enabled.
func (rx *RandomX) Threads() int {
	rx.lock.Lock()
	defer rx.lock.Unlock()
	return rx.threads
}

// SetThreads updates the number of mining threads currently enabled.
func (rx *RandomX) SetThreads(threads int) {
	rx.lock.Lock()
	defer rx.lock.Unlock()

	rx.threads = threads
	select {
	case rx.update <- struct{}{}:
	default:
	}
}

// Hashrate implements PoW, returning the measured rate of the search invocations
// per second over the last minute.
func (rx *RandomX) Hashrate() float64 {
	// Use simple counter-based rate: hashCount / elapsed since last snapshot.
	// The EWMA meter rounds to 0 for slow PoW like RandomX with low difficulty.
	now := time.Now().UnixNano()
	count := rx.hashCount.Swap(0)
	prev := rx.hashTime.Swap(now)

	if prev > 0 && count > 0 {
		elapsed := float64(now-prev) / 1e9
		if elapsed > 0 {
			rate := float64(count) / elapsed
			rx.hashSnap.Store(int64(rate))
		}
	}

	// Also gather remote rates
	var remote uint64
	if rx.config.PowMode == ModeNormal || rx.config.PowMode == ModeTest {
		if rx.remote != nil {
			var res = make(chan uint64, 1)
			select {
			case rx.remote.fetchRateCh <- res:
				remote = <-res
			case <-rx.remote.exitCh:
			}
		}
	}

	return float64(rx.hashSnap.Load()) + float64(remote)
}

// APIs implements consensus.Engine, returning the user facing RPC APIs.
func (rx *RandomX) APIs(chain consensus.ChainHeaderReader) []rpc.API {
	return []rpc.API{
		{
			Namespace: "eth",
			Service:   &API{rx},
		},
		{
			Namespace: "randomx",
			Service:   &API{rx},
		},
	}
}
