// Copyright 2019 The go-ethereum Authors
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

// Contains a batch of utility type declarations used by the tests. As the node
// operates on unique types, a lot of them are needed to check various features.

package statediff

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

// Subscription struct holds our subscription channels
type Subscription struct {
	PayloadChan chan<- Payload
	QuitChan    chan<- bool
}

// Payload packages the data to send to statediff subscriptions
type Payload struct {
	StateDiffRlp []byte `json:"stateDiff"    gencodec:"required"`
}

// StateDiff is the final output structure from the builder
type StateDiff struct {
	BlockNumber     *big.Int      `json:"blockNumber"     gencodec:"required"`
	BlockHash       common.Hash   `json:"blockHash"       gencodec:"required"`
	UpdatedAccounts []AccountDiff `json:"updatedAccounts" gencodec:"required"`
}

// AccountDiff holds the data for a single state diff node
type AccountDiff struct {
	Key     []byte        `json:"key"         gencodec:"required"`
	Value   []byte        `json:"value"       gencodec:"required"`
	Storage []StorageDiff `json:"storage"     gencodec:"required"`
}

// StorageDiff holds the data for a single storage diff node
type StorageDiff struct {
	Key   []byte `json:"key"         gencodec:"required"`
	Value []byte `json:"value"       gencodec:"required"`
}
