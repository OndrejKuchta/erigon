package evmlytics

import (
	"context"
	"fmt"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/ledgerwatch/erigon-lib/chain"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/gointerfaces/txpool"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/kv/kvcache"
	libstate "github.com/ledgerwatch/erigon-lib/state"
	"github.com/ledgerwatch/erigon/consensus"
	"github.com/ledgerwatch/erigon/core/rawdb"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/ethdb/prune"
	"github.com/ledgerwatch/erigon/turbo/rpchelper"
	"github.com/ledgerwatch/erigon/turbo/services"
	"github.com/ledgerwatch/log/v3"
	"go.uber.org/atomic"
)

type BaseAPI struct {
	stateCache   kvcache.Cache                         // thread-safe
	blocksLRU    *lru.Cache[common.Hash, *types.Block] // thread-safe
	filters      *rpchelper.Filters
	_chainConfig atomic.Pointer[chain.Config]
	_genesis     atomic.Pointer[types.Block]
	_historyV3   atomic.Pointer[bool]
	_pruneMode   atomic.Pointer[prune.Mode]

	_blockReader services.FullBlockReader
	_txnReader   services.TxnReader
	_agg         *libstate.AggregatorV3
	_engine      consensus.EngineReader

	evmCallTimeout time.Duration
}

type EvmlyticsAPI interface {
	StartReadingBlocks(db kv.RoDB, blockReader services.FullBlockReader)
}

type EvmlyticsAPIImpl struct {
	*BaseAPI
	db kv.RoDB
}

func StartReadingBlocks(ctx context.Context, db kv.RoDB, borDb kv.RoDB,
	eth rpchelper.ApiBackend, txPool txpool.TxpoolClient, mining txpool.MiningClient,
	stateCache kvcache.Cache, blockReader services.FullBlockReader,
	ff *rpchelper.Filters, agg *libstate.AggregatorV3, err error) (map[string]interface{}, error) {

	// Copy from otterscan block details - GetBlockDetails function
	tx, err := db.BeginRo(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	currentHeader := rawdb.ReadCurrentHeader(tx)
	//currentHeaderTime := currentHeader.Time
	//highestNumber := currentHeader.Number.Uint64()

	log.Info(currentHeader.Number.String())
	//highestBlockNum := currentHeader.Number.Uint64()

	// Set block to read manually
	// blockNum := highestBlockNum
	// blockNum = 15348627

	// Variant 2 - Blockreader

	for blockNum := uint64(15348627); blockNum < 15348637; blockNum++ {

		// fmt.Printf("%d\n", uint64(blockNum))

		hash, hashErr := rawdb.ReadCanonicalHash(tx, blockNum)
		if hashErr != nil {
			return nil, hashErr
		}

		block, senders, err := blockReader.BlockWithSenders(ctx, tx, hash, blockNum)
		if err != nil {
			return nil, err
			// Print error for this blog
		}
		if block == nil { // don't save nil's to DB
			return nil, nil
		}

		go storeBlockToDB(block, blockNum, senders)

	}

	// Start the routine that reads all ETH blocks from 0 till latest Block
	go readAllBlocks()

	return nil, nil

}

func storeBlockToDB(block *types.Block, blockNum uint64, senders []common.Address) {
	fmt.Printf("%d\n", uint64(blockNum))
	//fmt.Printf("%d\n", senders)

	fmt.Printf("%d\n", block.Transactions().Len())

}

func readAllBlocks() {

	log.Info("Starting Block Reader")
	log.Info("Starting Block Reader2")

}
