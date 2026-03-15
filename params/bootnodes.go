// Copyright 2015 The go-ethereum Authors
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

package params

import "github.com/ethereum/go-ethereum/common"

// MainnetBootnodes are the enode URLs of the P2P bootstrap nodes running on
// the main Ethereum network.
var MainnetBootnodes = []string{}

// AgentChainBootnodes are the enode URLs of the P2P bootstrap nodes for AgentChain.
var AgentChainBootnodes = []string{
	"enode://a72dd9715b58fd68bcfea9a8ece1fc7dfafe7803fc6eab825dbb3526f8093d4ac1bdc5833aa75a91fa2c9d26334e77e7770fe5973bd0a65f601ea8bad18028a0@165.232.86.29:30303",
}

// SepoliaBootnodes are the enode URLs of the P2P bootstrap nodes running on the
// Sepolia test network.
var SepoliaBootnodes = []string{}

// RinkebyBootnodes are the enode URLs of the P2P bootstrap nodes running on the
// Rinkeby test network.
var RinkebyBootnodes = []string{}

// GoerliBootnodes are the enode URLs of the P2P bootstrap nodes running on the
// Görli test network.
var GoerliBootnodes = []string{}

var V5Bootnodes = []string{}

const dnsPrefix = "enrtree://AKA3AM6LPBYEUDMVNU3BSVQJ5AD45Y7YPOHJLEF6W26QOE4VTUDPE@"

// KnownDNSNetwork returns the address of a public DNS-based node list for the given
// genesis hash and protocol. See https://github.com/ethereum/discv4-dns-lists for more
// information.
func KnownDNSNetwork(genesis common.Hash, protocol string) string {
	var net string
	switch genesis {
	case MainnetGenesisHash:
		net = "mainnet"
	case RinkebyGenesisHash:
		net = "rinkeby"
	case GoerliGenesisHash:
		net = "goerli"
	case SepoliaGenesisHash:
		net = "sepolia"
	default:
		return ""
	}
	return dnsPrefix + protocol + "." + net + ".ethdisco.net"
}
