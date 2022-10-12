package commands

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"

	"github.com/RoaringBitmap/roaring"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon/common"
	"github.com/ledgerwatch/erigon/common/dbutils"
	"github.com/ledgerwatch/erigon/core/rawdb"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/eth/filters"
	"github.com/ledgerwatch/erigon/ethdb/bitmapdb"
	"github.com/ledgerwatch/erigon/ethdb/cbor"
	"github.com/ledgerwatch/erigon/rpc"
	"github.com/ledgerwatch/erigon/turbo/rpchelper"
)

// GetLogsByHash implements erigon_getLogsByHash. Returns an array of arrays of logs generated by the transactions in the block given by the block's hash.
func (api *ErigonImpl) GetLogsByHash(ctx context.Context, hash common.Hash) ([][]*types.Log, error) {
	tx, err := api.db.BeginRo(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	chainConfig, err := api.chainConfig(tx)
	if err != nil {
		return nil, err
	}

	block, err := api.blockByHashWithSenders(tx, hash)
	if err != nil {
		return nil, err
	}
	if block == nil {
		return nil, nil
	}
	receipts, err := api.getReceipts(ctx, tx, chainConfig, block, block.Body().SendersFromTxs())
	if err != nil {
		return nil, fmt.Errorf("getReceipts error: %w", err)
	}

	logs := make([][]*types.Log, len(receipts))
	for i, receipt := range receipts {
		logs[i] = receipt.Logs
	}
	return logs, nil
}

// GetLogs implements eth_getLogs. Returns an array of logs matching a given filter object.
func (api *ErigonImpl) GetLogs(ctx context.Context, crit filters.FilterCriteria) (types.ErigonLogs, error) {
	var begin, end uint64
	erigonLogs := types.ErigonLogs{}

	tx, beginErr := api.db.BeginRo(ctx)
	if beginErr != nil {
		return erigonLogs, beginErr
	}
	defer tx.Rollback()

	if crit.BlockHash != nil {
		number := rawdb.ReadHeaderNumber(tx, *crit.BlockHash)
		if number == nil {
			return nil, fmt.Errorf("block not found: %x", *crit.BlockHash)
		}
		begin = *number
		end = *number
	} else {
		// Convert the RPC block numbers into internal representations
		latest, err := rpchelper.GetLatestBlockNumber(tx)
		if err != nil {
			return nil, err
		}

		begin = latest
		if crit.FromBlock != nil {
			if crit.FromBlock.Sign() >= 0 {
				begin = crit.FromBlock.Uint64()
			} else if !crit.FromBlock.IsInt64() || crit.FromBlock.Int64() != int64(rpc.LatestBlockNumber) {
				return nil, fmt.Errorf("negative value for FromBlock: %v", crit.FromBlock)
			}
		}
		end = latest
		if crit.ToBlock != nil {
			if crit.ToBlock.Sign() >= 0 {
				end = crit.ToBlock.Uint64()
			} else if !crit.ToBlock.IsInt64() || crit.ToBlock.Int64() != int64(rpc.LatestBlockNumber) {
				return nil, fmt.Errorf("negative value for ToBlock: %v", crit.ToBlock)
			}
		}
	}
	if end < begin {
		return nil, fmt.Errorf("end (%d) < begin (%d)", end, begin)
	}
	if end > roaring.MaxUint32 {
		return nil, fmt.Errorf("end (%d) > MaxUint32", end)
	}
	blockNumbers := bitmapdb.NewBitmap()
	defer bitmapdb.ReturnToPool(blockNumbers)
	blockNumbers.AddRange(begin, end+1) // [min,max)

	topicsBitmap, err := getTopicsBitmap(tx, crit.Topics, uint32(begin), uint32(end))
	if err != nil {
		return nil, err
	}
	if topicsBitmap != nil {
		blockNumbers.And(topicsBitmap)
	}

	rx := make([]*roaring.Bitmap, len(crit.Addresses))
	for idx, addr := range crit.Addresses {
		m, err := bitmapdb.Get(tx, kv.LogAddressIndex, addr[:], uint32(begin), uint32(end))
		if err != nil {
			return nil, err
		}
		rx[idx] = m
	}
	addrBitmap := roaring.FastOr(rx...)

	if len(rx) > 0 {
		blockNumbers.And(addrBitmap)
	}

	if blockNumbers.GetCardinality() == 0 {
		return erigonLogs, nil
	}

	iter := blockNumbers.Iterator()
	for iter.HasNext() {
		if err = ctx.Err(); err != nil {
			return nil, err
		}

		blockNumber := uint64(iter.Next())
		var logIndex uint
		var txIndex uint
		var blockLogs []*types.Log
		err := tx.ForPrefix(kv.Log, dbutils.EncodeBlockNumber(blockNumber), func(k, v []byte) error {
			var logs types.Logs
			if err := cbor.Unmarshal(&logs, bytes.NewReader(v)); err != nil {
				return fmt.Errorf("receipt unmarshal failed:  %w", err)
			}
			for _, log := range logs {
				log.Index = logIndex
				logIndex++
			}
			filtered := filterLogs(logs, crit.Addresses, crit.Topics)
			if len(filtered) == 0 {
				return nil
			}
			txIndex = uint(binary.BigEndian.Uint32(k[8:]))
			for _, log := range filtered {
				log.TxIndex = txIndex
			}
			blockLogs = append(blockLogs, filtered...)

			return nil
		})
		if err != nil {
			return erigonLogs, err
		}
		if len(blockLogs) == 0 {
			continue
		}

		header, err := api._blockReader.HeaderByNumber(ctx, tx, blockNumber)
		if err != nil {
			return nil, err
		}
		if header == nil {
			return nil, fmt.Errorf("block header not found: %d", blockNumber)
		}
		timestamp := header.Time

		blockHash, err := rawdb.ReadCanonicalHash(tx, blockNumber)
		if err != nil {
			return nil, err
		}

		body, err := api._blockReader.BodyWithTransactions(ctx, tx, blockHash, blockNumber)
		if err != nil {
			return nil, err
		}
		if body == nil {
			return nil, fmt.Errorf("block not found %d", blockNumber)
		}
		for _, log := range blockLogs {
			erigonLog := &types.ErigonLog{}
			erigonLog.BlockNumber = blockNumber
			erigonLog.BlockHash = blockHash
			erigonLog.TxHash = body.Transactions[log.TxIndex].Hash()
			erigonLog.Timestamp = timestamp
			erigonLog.Address = log.Address
			erigonLog.Topics = log.Topics
			erigonLog.Data = log.Data
			erigonLog.Index = log.Index
			erigonLog.Removed = log.Removed
			erigonLogs = append(erigonLogs, erigonLog)
		}
	}

	return erigonLogs, nil
}

// GetLogsByNumber implements erigon_getLogsByHash. Returns all the logs that appear in a block given the block's hash.
// func (api *ErigonImpl) GetLogsByNumber(ctx context.Context, number rpc.BlockNumber) ([][]*types.Log, error) {
// 	tx, err := api.db.Begin(ctx, false)
// 	if err != nil {
// 		return nil, err
// 	}
// 	defer tx.Rollback()

// 	number := rawdb.ReadHeaderNumber(tx, hash)
// 	if number == nil {
// 		return nil, fmt.Errorf("block not found: %x", hash)
// 	}

// 	receipts, err := getReceipts(ctx, tx, *number, hash)
// 	if err != nil {
// 		return nil, fmt.Errorf("getReceipts error: %w", err)
// 	}

// 	logs := make([][]*types.Log, len(receipts))
// 	for i, receipt := range receipts {
// 		logs[i] = receipt.Logs
// 	}
// 	return logs, nil
// }
