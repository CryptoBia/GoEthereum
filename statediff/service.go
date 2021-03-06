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

package statediff

import (
	"bytes"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
)

const chainEventChanSize = 20000

type blockChain interface {
	SubscribeChainEvent(ch chan<- core.ChainEvent) event.Subscription
	GetBlockByHash(hash common.Hash) *types.Block
	AddToStateDiffProcessedCollection(hash common.Hash)
	GetReceiptsByHash(hash common.Hash) types.Receipts
}

// IService is the state-diffing service interface
type IService interface {
	// APIs(), Protocols(), Start() and Stop()
	node.Service
	// Main event loop for processing state diffs
	Loop(chainEventCh chan core.ChainEvent)
	// Method to subscribe to receive state diff processing output
	Subscribe(id rpc.ID, sub chan<- Payload, quitChan chan<- bool)
	// Method to unsubscribe from state diff processing
	Unsubscribe(id rpc.ID) error
}

// Service is the underlying struct for the state diffing service
type Service struct {
	// Used to sync access to the Subscriptions
	sync.Mutex
	// Used to build the state diff objects
	Builder Builder
	// Used to subscribe to chain events (blocks)
	BlockChain blockChain
	// Used to signal shutdown of the service
	QuitChan chan bool
	// A mapping of rpc.IDs to their subscription channels
	Subscriptions map[rpc.ID]Subscription
	// Cache the last block so that we can avoid having to lookup the next block's parent
	lastBlock *types.Block
	// Whether or not the block data is streamed alongside the state diff data in the subscription payload
	StreamBlock bool
	// Whether or not we have any subscribers; only if we do, do we processes state diffs
	subscribers int32
}

// NewStateDiffService creates a new statediff.Service
func NewStateDiffService(db ethdb.Database, blockChain *core.BlockChain, config Config) (*Service, error) {
	return &Service{
		Mutex:         sync.Mutex{},
		BlockChain:    blockChain,
		Builder:       NewBuilder(db, blockChain, config),
		QuitChan:      make(chan bool),
		Subscriptions: make(map[rpc.ID]Subscription),
		StreamBlock:   config.StreamBlock,
	}, nil
}

// Protocols exports the services p2p protocols, this service has none
func (sds *Service) Protocols() []p2p.Protocol {
	return []p2p.Protocol{}
}

// APIs returns the RPC descriptors the statediff.Service offers
func (sds *Service) APIs() []rpc.API {
	return []rpc.API{
		{
			Namespace: APIName,
			Version:   APIVersion,
			Service:   NewPublicStateDiffAPI(sds),
			Public:    true,
		},
	}
}

// Loop is the main processing method
func (sds *Service) Loop(chainEventCh chan core.ChainEvent) {
	chainEventSub := sds.BlockChain.SubscribeChainEvent(chainEventCh)
	defer chainEventSub.Unsubscribe()
	errCh := chainEventSub.Err()
	for {
		select {
		//Notify chain event channel of events
		case chainEvent := <-chainEventCh:
			log.Debug("Event received from chainEventCh", "event", chainEvent)
			// if we don't have any subscribers, do not process a statediff
			if atomic.LoadInt32(&sds.subscribers) == 0 {
				log.Debug("Currently no subscribers to the statediffing service; processing is halted")
				continue
			}
			currentBlock := chainEvent.Block
			parentHash := currentBlock.ParentHash()
			var parentBlock *types.Block
			if sds.lastBlock != nil && bytes.Equal(sds.lastBlock.Hash().Bytes(), currentBlock.ParentHash().Bytes()) {
				parentBlock = sds.lastBlock
			} else {
				parentBlock = sds.BlockChain.GetBlockByHash(parentHash)
			}
			sds.lastBlock = currentBlock
			if parentBlock == nil {
				log.Error(fmt.Sprintf("Parent block is nil, skipping this block (%d)", currentBlock.Number()))
				continue
			}
			if err := sds.processStateDiff(currentBlock, parentBlock); err != nil {
				log.Error(fmt.Sprintf("Error building statediff for block %d; error: ", currentBlock.Number()) + err.Error())
			}
		case err := <-errCh:
			log.Warn("Error from chain event subscription, breaking loop", "error", err)
			sds.close()
			return
		case <-sds.QuitChan:
			log.Info("Quitting the statediffing process")
			sds.close()
			return
		}
	}
}

// processStateDiff method builds the state diff payload from the current and parent block before sending it to listening subscriptions
func (sds *Service) processStateDiff(currentBlock, parentBlock *types.Block) error {
	stateDiff, err := sds.Builder.BuildStateDiff(parentBlock.Root(), currentBlock.Root(), currentBlock.Number(), currentBlock.Hash())
	if err != nil {
		return err
	}
	stateDiffRlp, err := rlp.EncodeToBytes(stateDiff)
	if err != nil {
		return err
	}
	payload := Payload{
		StateDiffRlp: stateDiffRlp,
	}
	if sds.StreamBlock {
		blockBuff := new(bytes.Buffer)
		if err = currentBlock.EncodeRLP(blockBuff); err != nil {
			return err
		}
		payload.BlockRlp = blockBuff.Bytes()
		receiptBuff := new(bytes.Buffer)
		receipts := sds.BlockChain.GetReceiptsByHash(currentBlock.Hash())
		if err = rlp.Encode(receiptBuff, receipts); err != nil {
			return err
		}
		payload.ReceiptsRlp = receiptBuff.Bytes()
	}

	isEmpty, err := isEmptyPayload(payload, currentBlock)
	if err != nil {
		log.Warn("Error checking if payload is empty")
	}

	//Send a payload to subscribers only if isn't empty
	if !isEmpty {
		sds.send(payload)
	}

	return nil
}

func isEmptyPayload(payload Payload, block *types.Block) (bool, error) {
	emptyStateDiffRlp, err := getEmptyStateDiffRlpForBlock(block)
	if err != nil {
		return false, err
	}

	return reflect.DeepEqual(payload.StateDiffRlp, emptyStateDiffRlp), nil
}

func getEmptyStateDiffRlpForBlock(block *types.Block) ([]byte, error) {
	stateDiffWithoutUpdatedAccounts := StateDiff{
		BlockNumber: block.Number(),
		BlockHash:   block.Hash(),
	}

	return rlp.EncodeToBytes(stateDiffWithoutUpdatedAccounts)
}

// Subscribe is used by the API to subscribe to the service loop
func (sds *Service) Subscribe(id rpc.ID, sub chan<- Payload, quitChan chan<- bool) {
	log.Info("Subscribing to the statediff service")
	if atomic.CompareAndSwapInt32(&sds.subscribers, 0, 1) {
		log.Info("State diffing subscription received; beginning statediff processing")
	}
	sds.Lock()
	sds.Subscriptions[id] = Subscription{
		PayloadChan: sub,
		QuitChan:    quitChan,
	}
	sds.Unlock()
}

// Unsubscribe is used to unsubscribe from the service loop
func (sds *Service) Unsubscribe(id rpc.ID) error {
	log.Info("Unsubscribing from the statediff service")
	sds.Lock()
	_, ok := sds.Subscriptions[id]
	if !ok {
		return fmt.Errorf("cannot unsubscribe; subscription for id %s does not exist", id)
	}
	delete(sds.Subscriptions, id)
	if len(sds.Subscriptions) == 0 {
		if atomic.CompareAndSwapInt32(&sds.subscribers, 1, 0) {
			log.Info("No more subscriptions; halting statediff processing")
		}
	}
	sds.Unlock()
	return nil
}

// Start is used to begin the service
func (sds *Service) Start(*p2p.Server) error {
	log.Info("Starting statediff service")

	chainEventCh := make(chan core.ChainEvent, chainEventChanSize)
	go sds.Loop(chainEventCh)

	return nil
}

// Stop is used to close down the service
func (sds *Service) Stop() error {
	log.Info("Stopping statediff service")
	close(sds.QuitChan)
	return nil
}

// send is used to fan out and serve the payloads to all subscriptions
func (sds *Service) send(payload Payload) {
	sds.Lock()
	for id, sub := range sds.Subscriptions {
		select {
		case sub.PayloadChan <- payload:
			log.Info(fmt.Sprintf("sending state diff payload to subscription %s", id))
		default:
			log.Info(fmt.Sprintf("unable to send payload to subscription %s; channel has no receiver", id))
			// in this case, try to close the bad subscription and remove it
			select {
			case sub.QuitChan <- true:
				log.Info(fmt.Sprintf("closing subscription %s", id))
			default:
				log.Info(fmt.Sprintf("unable to close subscription %s; channel has no receiver", id))
			}
			delete(sds.Subscriptions, id)
		}
	}
	// If after removing all bad subscriptions we have none left, halt processing
	if len(sds.Subscriptions) == 0 {
		if atomic.CompareAndSwapInt32(&sds.subscribers, 1, 0) {
			log.Info("No more subscriptions; halting statediff processing")
		}
	}
	sds.Unlock()
}

// close is used to close all listening subscriptions
func (sds *Service) close() {
	sds.Lock()
	for id, sub := range sds.Subscriptions {
		select {
		case sub.QuitChan <- true:
			log.Info(fmt.Sprintf("closing subscription %s", id))
		default:
			log.Info(fmt.Sprintf("unable to close subscription %s; channel has no receiver", id))
		}
		delete(sds.Subscriptions, id)
	}
	sds.Unlock()
}
