// TODO:

/*

0, Use clickhouse Native interface

https://github.com/ClickHouse/clickhouse-go#native-interface
Use the columnar insert: https://github.com/ClickHouse/clickhouse-go/blob/main/examples/clickhouse_api/columnar_insert.go

ISSUES:
Decimals 38 to big Int
https://github.com/ClickHouse/clickhouse-go/blob/main/examples/clickhouse_api/big_int.go

Why are data not stored (0 rows) created after batch.send()

2, Try to run this function in paralel goroutines
3, Update Go to 18.4 to use the clickhouse-go version 2.3

Implement getLogs to get all logs and traces as well



*/

package evmlytics

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
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
	ff *rpchelper.Filters, agg *libstate.AggregatorV3) (map[string]interface{}, error) {

	// Init and connect to Clickhouse
	var (
		conn, err = clickhouse.Open(&clickhouse.Options{
			Addr: []string{"127.0.0.1:9000"},
			Auth: clickhouse.Auth{
				Database: "ethereum",
				Username: "default",
				Password: "110023",
			},
			Debug:           true,
			DialTimeout:     time.Second,
			MaxOpenConns:    10,
			MaxIdleConns:    5,
			ConnMaxLifetime: time.Hour,
		})
	)

	conn.Ping(ctx)

	defer func() {
		// conn.Exec(ctx, "DROP TABLE ethereum.blocks")
	}()

	// Init the Erigon DB
	tx, err := db.BeginRo(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	currentHeader := rawdb.ReadCurrentHeader(tx)
	log.Info(currentHeader.Number.String())

	highestBlockNum := currentHeader.Number.Uint64()
	fmt.Printf("This is the highest block number %d\n", uint64(highestBlockNum))

	// Set block to read manually
	// blockNum = 15348627

	// Go through all the Blocks from 0 to latest
	var batchSize = uint64(1000)
	highestBlockNum = uint64(15348627)

	for blockNum := uint64(15338627); blockNum < highestBlockNum; blockNum += batchSize {
		// Start and end of the current batch
		endOfCurrentBatch := blockNum + batchSize
		if endOfCurrentBatch > highestBlockNum {
			endOfCurrentBatch = highestBlockNum
		}

		fmt.Printf("Processing batch, start: %d end: %d\n", uint64(blockNum), uint64(endOfCurrentBatch))

		batch, err := conn.PrepareBatch(context.Background(), "INSERT INTO ethereum.blocks")
		if err != nil {
			return nil, err
		}

		// batch.Flush()

		// Create always a batch of blocks and send it to clickhouse

		var (
			numbers           []uint32
			hashes            []string
			parentHashes      []string
			nonces            []string
			sha3UnclesList    []string
			logsBlooms        []string
			transactionsRoots []string
			stateRoots        []string
			receiptsRoots     []string
			miners            []string
			difficulties      []*big.Int
			totalDifficulties []*big.Int
			sizes             []uint32
			extraDataList     []string
			gasLimits         []uint32
			gasUsages         []uint32
			timestamps        []time.Time
			transactionCounts []uint16
			baseFeePerGasList []uint64
		)

		// ----- Internal FOR loop -----

		for blockCount := uint64(blockNum); blockCount < endOfCurrentBatch; blockCount++ {
			// Read one block and append it

			hash, hashErr := rawdb.ReadCanonicalHash(tx, blockCount)
			if hashErr != nil {
				return nil, hashErr
			}

			block, _, err := blockReader.BlockWithSenders(ctx, tx, hash, blockCount)
			if err == nil && block != nil {
				// Store block data to arrays
				numbers = append(numbers, uint32(block.Number().Uint64()))
				fmt.Printf("Processing block number %d trans.len: %d numbers.len: %d\n", block.Number(), block.Transactions().Len(), len(numbers))

				hashes = append(hashes, "")
				parentHashes = append(parentHashes, "")
				nonces = append(nonces, "")
				sha3UnclesList = append(sha3UnclesList, "")
				logsBlooms = append(logsBlooms, "")
				transactionsRoots = append(transactionsRoots, "")
				stateRoots = append(stateRoots, "")
				receiptsRoots = append(receiptsRoots, "")
				miners = append(miners, "")

				difficulties = append(difficulties, big.NewInt(0))
				totalDifficulties = append(totalDifficulties, big.NewInt(0))
				sizes = append(sizes, 0)
				extraDataList = append(extraDataList, "")
				gasLimits = append(gasLimits, 0)
				gasUsages = append(gasUsages, 0)
				timestamps = append(timestamps, time.Now())
				transactionCounts = append(transactionCounts, 0)
				baseFeePerGasList = append(baseFeePerGasList, 0)

			}
		}
		// ----- END -----

		if err := batch.Column(0).Append(numbers); err != nil {
			return nil, err
		}

		/*
			if err := batch.Column(0).Append(numbers); err != nil {
				return nil, err
			}

			if err := batch.Column(1).Append(hashes); err != nil {
				return nil, err
			}

			if err := batch.Column(2).Append(parentHashes); err != nil {
				return nil, err
			}

			if err := batch.Column(3).Append(nonces); err != nil {
				return nil, err
			}

			if err := batch.Column(4).Append(sha3UnclesList); err != nil {
				return nil, err
			}

			if err := batch.Column(5).Append(logsBlooms); err != nil {
				return nil, err
			}

			if err := batch.Column(6).Append(transactionsRoots); err != nil {
				return nil, err
			}

			if err := batch.Column(7).Append(miners); err != nil {
				return nil, err
			}

			if err := batch.Column(8).Append(difficulties); err != nil {
				return nil, err
			}

			if err := batch.Column(9).Append(totalDifficulties); err != nil {
				return nil, err
			}

			if err := batch.Column(10).Append(sizes); err != nil {
				return nil, err
			}

			if err := batch.Column(11).Append(extraDataList); err != nil {
				return nil, err
			}

			if err := batch.Column(12).Append(gasLimits); err != nil {
				return nil, err
			}

			if err := batch.Column(13).Append(gasUsages); err != nil {
				return nil, err
			}

			if err := batch.Column(14).Append(timestamps); err != nil {
				return nil, err
			}

			if err := batch.Column(15).Append(transactionCounts); err != nil {
				return nil, err
			}

			if err := batch.Column(16).Append(baseFeePerGasList); err != nil {
				return nil, err
			}
		*/

		// Send the batch
		if err = batch.Send(); err != nil {
			return nil, err
		}

	}

	return nil, nil

}

/*

func storeBlockToDB(block *types.Block, blockNum uint64, senders []common.Address) {
	//fmt.Printf("%d\n", uint64(blockNum))
	//fmt.Printf("%d\n", senders)
	//fmt.Printf("%d\n", block.Transactions().Len())
}

func insertColumns() error {

	var (
		ctx       = context.Background()
		conn, err = clickhouse.Open(&clickhouse.Options{
			Addr: []string{"127.0.0.1:9000"},
			Auth: clickhouse.Auth{
				Database: "ethereum",
				Username: "default",
				Password: "110023",
			},
			//Debug:           true,
			DialTimeout:     time.Second,
			MaxOpenConns:    10,
			MaxIdleConns:    5,
			ConnMaxLifetime: time.Hour,
		})
	)

	defer func() {
		conn.Exec(ctx, "DROP TABLE ethereum.blocks")
	}()

	// conn.Exec(ctx, `DROP TABLE IF EXISTS ethereum.blocks`)


		if err = conn.Exec(ctx, `
			CREATE TABLE example (
				  Col1 UInt64
				, Col2 String
				, Col3 Array(UInt8)
				, Col4 DateTime
			) ENGINE = Memory
		`); err != nil {
			return err
		}


	batch, err := conn.PrepareBatch(context.Background(), "INSERT INTO example")
	if err != nil {
		return err
	}
	var (
		numbers           []uint32
		hashes            []string
		parentHashes      []string
		nonces            []string
		sha3UnclesList    []string
		logsBlooms        []string
		transactionsRoots []string
		stateRoots        []string
		receiptsRoots     []string
		miners            []string
		difficulties      []*big.Int
		totalDifficulties []*big.Int
		sizes             []uint32
		extraDataList     []string
		gasLimits         []uint32
		gasUsages         []uint32
		timestamps        []time.Time
		transactionCounts []uint16
		baseFeePerGasList []uint64
	)
	for i := 0; i < 1_000; i++ {


			col1 = append(col1, uint64(i))
			col2 = append(col2, "Golang SQL database driver")
			col3 = append(col3, []uint8{1, 2, 3, 4, 5, 6, 7, 8, 9})
			col4 = append(col4, time.Now())


	}
	if err := batch.Column(0).Append(col1); err != nil {
		return err
	}
	if err := batch.Column(1).Append(col2); err != nil {
		return err
	}
	if err := batch.Column(2).Append(col3); err != nil {
		return err
	}
	if err := batch.Column(3).Append(col4); err != nil {
		return err
	}
	return batch.Send()
}


func insertColumnsNative() error {

	ctx := context.Background()
	c, err := ch.Dial(ctx, ch.Options{Address: "localhost:9000", Password: "110023"})
	if err != nil {
		panic(err)
	}
	var (
		numbers int
		data    proto.ColUInt64
	)

	if err := c.Do(ctx, ch.Query{
		Body: "SELECT number FROM ethereum.blocks LIMIT 1000",
		Result: proto.Results{
			{Name: "number", Data: &data},
		},
		// OnResult will be called on next received data block.
		OnResult: func(ctx context.Context, b proto.Block) error {
			numbers += len(data)
			return nil
		},
	}); err != nil {
		log.Info(err.Error())
		panic(err)
	}
	fmt.Println("numbers:", numbers)
	return nil
}
*/
