package stages

import (
	"time"

	"github.com/c2h5oh/datasize"
	"github.com/gateway-fm/cdk-erigon-lib/common"
	"github.com/gateway-fm/cdk-erigon-lib/common/datadir"
	"github.com/gateway-fm/cdk-erigon-lib/kv"
	libstate "github.com/gateway-fm/cdk-erigon-lib/state"

	"math/big"

	"fmt"

	"github.com/0xPolygonHermez/zkevm-data-streamer/datastreamer"
	"github.com/ledgerwatch/erigon/chain"
	"github.com/ledgerwatch/erigon/common/math"
	"github.com/ledgerwatch/erigon/consensus"
	"github.com/ledgerwatch/erigon/core/rawdb"
	"github.com/ledgerwatch/erigon/core/state"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/core/vm"
	"github.com/ledgerwatch/erigon/eth/ethconfig"
	"github.com/ledgerwatch/erigon/eth/stagedsync"
	"github.com/ledgerwatch/erigon/eth/stagedsync/stages"
	"github.com/ledgerwatch/erigon/ethdb/prune"
	"github.com/ledgerwatch/erigon/turbo/services"
	"github.com/ledgerwatch/erigon/turbo/shards"
	"github.com/ledgerwatch/erigon/turbo/stages/headerdownload"
	"github.com/ledgerwatch/erigon/zk/datastream/server"
	"github.com/ledgerwatch/erigon/zk/hermez_db"
	verifier "github.com/ledgerwatch/erigon/zk/legacy_executor_verifier"
	"github.com/ledgerwatch/erigon/zk/tx"
	zktx "github.com/ledgerwatch/erigon/zk/tx"
	"github.com/ledgerwatch/erigon/zk/txpool"
	zktypes "github.com/ledgerwatch/erigon/zk/types"
	"github.com/ledgerwatch/erigon/zk/utils"
	"github.com/ledgerwatch/log/v3"
)

const (
	logInterval         = 20 * time.Second
	transactionGasLimit = 30000000
)

var (
	noop                 = state.NewNoopWriter()
	blockDifficulty      = new(big.Int).SetUint64(0)
	SpecialZeroIndexHash = common.HexToHash("0x27AE5BA08D7291C96C8CBDDCC148BF48A6D68C7974B94356F53754EF6171D757")
)

type HasChangeSetWriter interface {
	ChangeSetWriter() *state.ChangeSetWriter
}

type SequenceBlockCfg struct {
	db            kv.RwDB
	batchSize     datasize.ByteSize
	prune         prune.Mode
	changeSetHook stagedsync.ChangeSetHook
	chainConfig   *chain.Config
	engine        consensus.Engine
	zkVmConfig    *vm.ZkConfig
	badBlockHalt  bool
	stateStream   bool
	accumulator   *shards.Accumulator
	blockReader   services.FullBlockReader

	dirs             datadir.Dirs
	historyV3        bool
	syncCfg          ethconfig.Sync
	genesis          *types.Genesis
	agg              *libstate.AggregatorV3
	stream           *datastreamer.StreamServer
	datastreamServer *server.DataStreamServer
	zk               *ethconfig.Zk

	txPool   *txpool.TxPool
	txPoolDb kv.RwDB

	legacyVerifier *verifier.LegacyExecutorVerifier
	yieldSize      uint16
}

func StageSequenceBlocksCfg(
	db kv.RwDB,
	pm prune.Mode,
	batchSize datasize.ByteSize,
	changeSetHook stagedsync.ChangeSetHook,
	chainConfig *chain.Config,
	engine consensus.Engine,
	vmConfig *vm.ZkConfig,
	accumulator *shards.Accumulator,
	stateStream bool,
	badBlockHalt bool,

	historyV3 bool,
	dirs datadir.Dirs,
	blockReader services.FullBlockReader,
	genesis *types.Genesis,
	syncCfg ethconfig.Sync,
	agg *libstate.AggregatorV3,
	stream *datastreamer.StreamServer,
	zk *ethconfig.Zk,

	txPool *txpool.TxPool,
	txPoolDb kv.RwDB,
	legacyVerifier *verifier.LegacyExecutorVerifier,
	yieldSize uint16,
) SequenceBlockCfg {

	return SequenceBlockCfg{
		db:               db,
		prune:            pm,
		batchSize:        batchSize,
		changeSetHook:    changeSetHook,
		chainConfig:      chainConfig,
		engine:           engine,
		zkVmConfig:       vmConfig,
		dirs:             dirs,
		accumulator:      accumulator,
		stateStream:      stateStream,
		badBlockHalt:     badBlockHalt,
		blockReader:      blockReader,
		genesis:          genesis,
		historyV3:        historyV3,
		syncCfg:          syncCfg,
		agg:              agg,
		stream:           stream,
		datastreamServer: server.NewDataStreamServer(stream, chainConfig.ChainID.Uint64()),
		zk:               zk,
		txPool:           txPool,
		txPoolDb:         txPoolDb,
		legacyVerifier:   legacyVerifier,
		yieldSize:        yieldSize,
	}
}

func (sCfg *SequenceBlockCfg) toErigonExecuteBlockCfg() stagedsync.ExecuteBlockCfg {
	return stagedsync.StageExecuteBlocksCfg(
		sCfg.db,
		sCfg.prune,
		sCfg.batchSize,
		sCfg.changeSetHook,
		sCfg.chainConfig,
		sCfg.engine,
		&sCfg.zkVmConfig.Config,
		sCfg.accumulator,
		sCfg.stateStream,
		sCfg.badBlockHalt,
		sCfg.historyV3,
		sCfg.dirs,
		sCfg.blockReader,
		headerdownload.NewHeaderDownload(1, 1, sCfg.engine, sCfg.blockReader),
		sCfg.genesis,
		sCfg.syncCfg,
		sCfg.agg,
		sCfg.zk,
	)
}

type forkDb interface {
	GetAllForkHistory() ([]uint64, []uint64, error)
	GetLatestForkHistory() (uint64, uint64, error)
	GetForkId(batch uint64) (uint64, error)
	WriteForkIdBlockOnce(forkId, block uint64) error
	WriteForkId(batch, forkId uint64) error
}

func prepareForkId(lastBatch, executionAt uint64, hermezDb forkDb) (uint64, error) {
	var err error
	var latest uint64

	// get all history and find the fork appropriate for the batch we're processing now
	allForks, allBatches, err := hermezDb.GetAllForkHistory()
	if err != nil {
		return 0, err
	}

	nextBatch := lastBatch + 1

	// iterate over the batch boundaries and find the latest fork that applies
	for idx, batch := range allBatches {
		if nextBatch > batch {
			latest = allForks[idx]
		}
	}

	if latest == 0 {
		// not an error, need to wait for the block to finalize on the L1
		return 0, nil
	}

	// now we need to check the last batch to see if we need to update the fork id
	lastBatchFork, err := hermezDb.GetForkId(lastBatch)
	if err != nil {
		return 0, err
	}

	// write the fork height once for the next block at the point of fork upgrade
	if lastBatchFork < latest {
		log.Info("Upgrading fork id", "from", lastBatchFork, "to", latest, "batch", nextBatch)
		if err := hermezDb.WriteForkIdBlockOnce(latest, executionAt+1); err != nil {
			return latest, err
		}
	}

	return latest, nil
}

func prepareHeader(tx kv.RwTx, previousBlockNumber, deltaTimestamp, forcedTimestamp, forkId uint64, coinbase common.Address) (*types.Header, *types.Block, error) {
	parentBlock, err := rawdb.ReadBlockByNumber(tx, previousBlockNumber)
	if err != nil {
		return nil, nil, err
	}

	var newBlockTimestamp uint64

	if forcedTimestamp != math.MaxUint64 {
		newBlockTimestamp = forcedTimestamp
	} else {
		// in the case of normal execution when not in l1 recovery
		// we want to generate the timestamp based on the current time.  When in recovery
		// we will pass a real delta which we then need to apply to the previous block timestamp
		useTimestampOffsetFromParentBlock := deltaTimestamp != math.MaxUint64
		newBlockTimestamp = uint64(time.Now().Unix())
		if useTimestampOffsetFromParentBlock {
			newBlockTimestamp = parentBlock.Time() + deltaTimestamp
		}
	}

	return &types.Header{
		ParentHash: parentBlock.Hash(),
		Coinbase:   coinbase,
		Difficulty: blockDifficulty,
		Number:     new(big.Int).SetUint64(previousBlockNumber + 1),
		GasLimit:   utils.GetBlockGasLimitForFork(forkId),
		Time:       newBlockTimestamp,
	}, parentBlock, nil
}

func prepareL1AndInfoTreeRelatedStuff(sdb *stageDb, batchState *BatchState, proposedTimestamp uint64) (
	infoTreeIndexProgress uint64,
	l1TreeUpdate *zktypes.L1InfoTreeUpdate,
	l1TreeUpdateIndex uint64,
	l1BlockHash common.Hash,
	ger common.Hash,
	shouldWriteGerToContract bool,
	err error,
) {
	// if we are in a recovery state and recognise that a l1 info tree index has been reused
	// then we need to not include the GER and L1 block hash into the block info root calculation, so
	// we keep track of this here
	shouldWriteGerToContract = true

	if _, infoTreeIndexProgress, err = sdb.hermezDb.GetLatestBlockL1InfoTreeIndexProgress(); err != nil {
		return
	}

	if batchState.isL1Recovery() {
		l1TreeUpdateIndex = uint64(batchState.blockState.blockL1RecoveryData.L1InfoTreeIndex)
		if l1TreeUpdate, err = sdb.hermezDb.GetL1InfoTreeUpdate(l1TreeUpdateIndex); err != nil {
			return
		}
		if infoTreeIndexProgress >= l1TreeUpdateIndex {
			shouldWriteGerToContract = false
		}
	} else {
		if l1TreeUpdateIndex, l1TreeUpdate, err = calculateNextL1TreeUpdateToUse(infoTreeIndexProgress, sdb.hermezDb, proposedTimestamp); err != nil {
			return
		}
		if l1TreeUpdateIndex > 0 {
			infoTreeIndexProgress = l1TreeUpdateIndex
		}
	}

	// we only want GER and l1 block hash for indexes above 0 - 0 is a special case
	if l1TreeUpdate != nil && l1TreeUpdateIndex > 0 {
		l1BlockHash = l1TreeUpdate.ParentHash
		ger = l1TreeUpdate.GER
	}

	return
}

func prepareTickers(cfg *SequenceBlockCfg) (*time.Ticker, *time.Ticker, *time.Ticker) {
	batchTicker := time.NewTicker(cfg.zk.SequencerBatchSealTime)
	logTicker := time.NewTicker(10 * time.Second)
	blockTicker := time.NewTicker(cfg.zk.SequencerBlockSealTime)

	return batchTicker, logTicker, blockTicker
}

// will be called at the start of every new block created within a batch to figure out if there is a new GER
// we can use or not.  In the special case that this is the first block we just return 0 as we need to use the
// 0 index first before we can use 1+
func calculateNextL1TreeUpdateToUse(lastInfoIndex uint64, hermezDb *hermez_db.HermezDb, proposedTimestamp uint64) (uint64, *zktypes.L1InfoTreeUpdate, error) {
	// always default to 0 and only update this if the next available index has reached finality
	var nextL1Index uint64 = 0

	// check if the next index is there and if it has reached finality or not
	l1Info, err := hermezDb.GetL1InfoTreeUpdate(lastInfoIndex + 1)
	if err != nil {
		return 0, nil, err
	}

	// ensure that we are above the min timestamp for this index to use it
	if l1Info != nil && l1Info.Timestamp <= proposedTimestamp {
		nextL1Index = l1Info.Index
	}

	return nextL1Index, l1Info, nil
}

func updateSequencerProgress(tx kv.RwTx, newHeight uint64, newBatch uint64, unwinding bool) error {
	// now update stages that will be used later on in stageloop.go and other stages. As we're the sequencer
	// we won't have headers stage for example as we're already writing them here
	if err := stages.SaveStageProgress(tx, stages.Execution, newHeight); err != nil {
		return err
	}
	if err := stages.SaveStageProgress(tx, stages.Headers, newHeight); err != nil {
		return err
	}
	if err := stages.SaveStageProgress(tx, stages.HighestSeenBatchNumber, newBatch); err != nil {
		return err
	}

	if !unwinding {
		if err := stages.SaveStageProgress(tx, stages.IntermediateHashes, newHeight); err != nil {
			return err
		}

		if err := stages.SaveStageProgress(tx, stages.AccountHistoryIndex, newHeight); err != nil {
			return err
		}

		if err := stages.SaveStageProgress(tx, stages.StorageHistoryIndex, newHeight); err != nil {
			return err
		}
	}

	return nil
}

func tryHaltSequencer(batchContext *BatchContext, thisBatch uint64) {
	if batchContext.cfg.zk.SequencerHaltOnBatchNumber != 0 && batchContext.cfg.zk.SequencerHaltOnBatchNumber == thisBatch {
		for {
			log.Info(fmt.Sprintf("[%s] Halt sequencer on batch %d...", batchContext.s.LogPrefix(), thisBatch))
			time.Sleep(5 * time.Second) //nolint:gomnd
		}
	}
}

type batchChecker interface {
	GetL1InfoTreeUpdate(idx uint64) (*zktypes.L1InfoTreeUpdate, error)
}

func checkForBadBatch(
	batchNo uint64,
	hermezDb batchChecker,
	latestTimestamp uint64,
	highestAllowedInfoTreeIndex uint64,
	limitTimestamp uint64,
	decodedBlocks []zktx.DecodedBatchL2Data,
) (bool, error) {
	timestamp := latestTimestamp

	for _, decodedBlock := range decodedBlocks {
		timestamp += uint64(decodedBlock.DeltaTimestamp)

		// now check the limit timestamp we can't have used l1 info tree index from the future
		if timestamp > limitTimestamp {
			log.Error("batch went above the limit timestamp", "batch", batchNo, "timestamp", timestamp, "limit_timestamp", limitTimestamp)
			return true, nil
		}

		if decodedBlock.L1InfoTreeIndex > 0 {
			// first check if we have knowledge of this index or not
			l1Info, err := hermezDb.GetL1InfoTreeUpdate(uint64(decodedBlock.L1InfoTreeIndex))
			if err != nil {
				return false, err
			}
			if l1Info == nil {
				// can't use an index that doesn't exist, so we have a bad batch
				log.Error("batch used info tree index that doesn't exist", "batch", batchNo, "index", decodedBlock.L1InfoTreeIndex)
				return true, nil
			}

			// we have an invalid batch if the block timestamp is lower than the l1 info min timestamp value
			if timestamp < l1Info.Timestamp {
				log.Error("batch used info tree index with timestamp lower than allowed", "batch", batchNo, "index", decodedBlock.L1InfoTreeIndex, "timestamp", timestamp, "min_timestamp", l1Info.Timestamp)
				return true, nil
			}

			// now finally check that the index used is lower or equal to the highest allowed index
			if uint64(decodedBlock.L1InfoTreeIndex) > highestAllowedInfoTreeIndex {
				log.Error("batch used info tree index higher than the current info tree root allows", "batch", batchNo, "index", decodedBlock.L1InfoTreeIndex, "highest_allowed", highestAllowedInfoTreeIndex)
				return true, nil
			}
		}
	}

	return false, nil
}

// hard coded to match in with the smart contract
// https://github.com/0xPolygonHermez/zkevm-contracts/blob/73758334f8568b74e9493fcc530b442bd73325dc/contracts/PolygonZkEVM.sol#L119C63-L119C69
const LIMIT_120_KB = 120_000

type BlockDataChecker struct {
	limit   uint64 // limit amount of bytes
	counter uint64 // counter amount of bytes
}

func newBlockDataChecker() *BlockDataChecker {
	return &BlockDataChecker{
		limit:   LIMIT_120_KB,
		counter: 0,
	}
}

// adds bytes amounting to the block data and checks if the limit is reached
// if the limit is reached, the data is not added, so this can be reused again for next check
func (bdc *BlockDataChecker) AddBlockStartData() bool {
	blockStartBytesAmount := tx.START_BLOCK_BATCH_L2_DATA_SIZE // tx.GenerateStartBlockBatchL2Data(deltaTimestamp, l1InfoTreeIndex) returns 65 long byte array
	// add in the changeL2Block transaction
	if bdc.counter+blockStartBytesAmount > bdc.limit {
		return true
	}

	bdc.counter += blockStartBytesAmount

	return false
}

func (bdc *BlockDataChecker) AddTransactionData(txL2Data []byte) bool {
	encodedLen := uint64(len(txL2Data))
	if bdc.counter+encodedLen > bdc.limit {
		return true
	}

	bdc.counter += encodedLen

	return false
}
