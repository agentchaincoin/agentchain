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
	"errors"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
)

var errRandomXStopped = errors.New("randomx stopped")

// API exposes RandomX related methods for the RPC interface.
type API struct {
	rx *RandomX
}

// GetWork returns a work package for external miner.
//
// The work package consists of 4 strings:
//
//	result[0] - 32 bytes hex encoded current block header pow-hash
//	result[1] - 32 bytes hex encoded seed key used for RandomX
//	result[2] - 32 bytes hex encoded boundary condition ("target"), 2^256/difficulty
//	result[3] - hex encoded block number
func (api *API) GetWork() ([4]string, error) {
	if api.rx.remote == nil {
		return [4]string{}, errors.New("not supported")
	}

	var (
		workCh = make(chan [4]string, 1)
		errc   = make(chan error, 1)
	)
	select {
	case api.rx.remote.fetchWorkCh <- &sealWork{errc: errc, res: workCh}:
	case <-api.rx.remote.exitCh:
		return [4]string{}, errRandomXStopped
	}
	select {
	case work := <-workCh:
		return work, nil
	case err := <-errc:
		return [4]string{}, err
	}
}

// SubmitWork can be used by external miner to submit their POW solution.
// It returns an indication if the work was accepted.
func (api *API) SubmitWork(nonce types.BlockNonce, hash, digest common.Hash) bool {
	if api.rx.remote == nil {
		return false
	}

	var errc = make(chan error, 1)
	select {
	case api.rx.remote.submitWorkCh <- &mineResult{
		nonce:     nonce,
		mixDigest: digest,
		hash:      hash,
		errc:      errc,
	}:
	case <-api.rx.remote.exitCh:
		return false
	}
	err := <-errc
	return err == nil
}

// SubmitHashrate can be used for remote miners to submit their hash rate.
func (api *API) SubmitHashrate(rate hexutil.Uint64, id common.Hash) bool {
	if api.rx.remote == nil {
		return false
	}

	var done = make(chan struct{}, 1)
	select {
	case api.rx.remote.submitRateCh <- &hashrate{done: done, rate: uint64(rate), id: id}:
	case <-api.rx.remote.exitCh:
		return false
	}

	<-done
	return true
}

// GetHashrate returns the current hashrate for local CPU miner and remote miner.
func (api *API) GetHashrate() uint64 {
	return uint64(api.rx.Hashrate())
}
