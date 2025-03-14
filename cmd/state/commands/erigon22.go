package commands

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	libcommon "github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/kv"
	kv2 "github.com/ledgerwatch/erigon-lib/kv/mdbx"
	libstate "github.com/ledgerwatch/erigon-lib/state"
	"github.com/ledgerwatch/erigon/cmd/sentry/sentry"
	"github.com/ledgerwatch/erigon/common"
	"github.com/ledgerwatch/erigon/consensus"
	"github.com/ledgerwatch/erigon/consensus/misc"
	"github.com/ledgerwatch/erigon/core"
	"github.com/ledgerwatch/erigon/core/state"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/core/vm"
	"github.com/ledgerwatch/erigon/eth/ethconfig"
	"github.com/ledgerwatch/erigon/eth/stagedsync"
	"github.com/ledgerwatch/erigon/eth/stagedsync/stages"
	datadir2 "github.com/ledgerwatch/erigon/node/nodecfg/datadir"
	"github.com/ledgerwatch/erigon/p2p"
	"github.com/ledgerwatch/erigon/params"
	"github.com/ledgerwatch/erigon/turbo/services"
	"github.com/ledgerwatch/erigon/turbo/snapshotsync"
	stages2 "github.com/ledgerwatch/erigon/turbo/stages"
	"github.com/ledgerwatch/log/v3"
	"github.com/spf13/cobra"
	"golang.org/x/sync/semaphore"
)

var (
	reset bool
)

func init() {
	erigon22Cmd.Flags().BoolVar(&reset, "reset", false, "Resets the state database and static files")
	withDataDir(erigon22Cmd)
	rootCmd.AddCommand(erigon22Cmd)
}

var erigon22Cmd = &cobra.Command{
	Use:   "erigon22",
	Short: "Exerimental command to re-execute blocks from beginning using erigon2 histoty (ugrade 2)",
	RunE: func(cmd *cobra.Command, args []string) error {
		logger := log.New()
		return Erigon22(genesis, logger)
	},
}

type Worker22 struct {
	lock         sync.Locker
	db           kv.RoDB
	tx           kv.Tx
	wg           *sync.WaitGroup
	rs           *state.State22
	blockReader  services.FullBlockReader
	allSnapshots *snapshotsync.RoSnapshots
	stateWriter  *state.StateWriter22
	stateReader  *state.StateReader22
	getHeader    func(hash common.Hash, number uint64) *types.Header
	ctx          context.Context
	engine       consensus.Engine
	txNums       []uint64
	chainConfig  *params.ChainConfig
	logger       log.Logger
	genesis      *core.Genesis
	resultCh     chan state.TxTask
}

func NewWorker22(lock sync.Locker, db kv.RoDB, wg *sync.WaitGroup, rs *state.State22,
	blockReader services.FullBlockReader, allSnapshots *snapshotsync.RoSnapshots,
	txNums []uint64, chainConfig *params.ChainConfig, logger log.Logger, genesis *core.Genesis,
	resultCh chan state.TxTask,
) *Worker22 {
	return &Worker22{
		lock:         lock,
		db:           db,
		wg:           wg,
		rs:           rs,
		blockReader:  blockReader,
		allSnapshots: allSnapshots,
		ctx:          context.Background(),
		stateWriter:  state.NewStateWriter22(rs),
		stateReader:  state.NewStateReader22(rs),
		txNums:       txNums,
		chainConfig:  chainConfig,
		logger:       logger,
		genesis:      genesis,
		resultCh:     resultCh,
	}
}

func (rw *Worker22) ResetTx() {
	if rw.tx != nil {
		rw.tx.Rollback()
		rw.tx = nil
	}
}

func (rw *Worker22) run() {
	defer rw.wg.Done()
	rw.getHeader = func(hash common.Hash, number uint64) *types.Header {
		h, err := rw.blockReader.Header(rw.ctx, nil, hash, number)
		if err != nil {
			panic(err)
		}
		return h
	}
	rw.engine = initConsensusEngine(rw.chainConfig, rw.logger, rw.allSnapshots)
	for txTask, ok := rw.rs.Schedule(); ok; txTask, ok = rw.rs.Schedule() {
		rw.runTxTask(&txTask)
		rw.resultCh <- txTask // Needs to have outside of the lock
	}
}

func (rw *Worker22) runTxTask(txTask *state.TxTask) {
	rw.lock.Lock()
	defer rw.lock.Unlock()
	if rw.tx == nil {
		var err error
		if rw.tx, err = rw.db.BeginRo(rw.ctx); err != nil {
			panic(err)
		}
		rw.stateReader.SetTx(rw.tx)
	}
	txTask.Error = nil
	rw.stateReader.SetTxNum(txTask.TxNum)
	rw.stateWriter.SetTxNum(txTask.TxNum)
	rw.stateReader.ResetReadSet()
	rw.stateWriter.ResetWriteSet()
	ibs := state.New(rw.stateReader)
	daoForkTx := rw.chainConfig.DAOForkSupport && rw.chainConfig.DAOForkBlock != nil && rw.chainConfig.DAOForkBlock.Uint64() == txTask.BlockNum && txTask.TxIndex == -1
	var err error
	if txTask.BlockNum == 0 && txTask.TxIndex == -1 {
		//fmt.Printf("txNum=%d, blockNum=%d, Genesis\n", txTask.TxNum, txTask.BlockNum)
		// Genesis block
		_, ibs, err = rw.genesis.ToBlock()
		if err != nil {
			panic(err)
		}
	} else if daoForkTx {
		//fmt.Printf("txNum=%d, blockNum=%d, DAO fork\n", txTask.TxNum, txTask.BlockNum)
		misc.ApplyDAOHardFork(ibs)
		ibs.SoftFinalise()
	} else if txTask.TxIndex == -1 {
		// Block initialisation
	} else if txTask.Final {
		if txTask.BlockNum > 0 {
			//fmt.Printf("txNum=%d, blockNum=%d, finalisation of the block\n", txTask.TxNum, txTask.BlockNum)
			// End of block transaction in a block
			if _, _, err := rw.engine.Finalize(rw.chainConfig, txTask.Header, ibs, txTask.Block.Transactions(), txTask.Block.Uncles(), nil /* receipts */, nil, nil, nil); err != nil {
				panic(fmt.Errorf("finalize of block %d failed: %w", txTask.BlockNum, err))
			}
		}
	} else {
		//fmt.Printf("txNum=%d, blockNum=%d, txIndex=%d\n", txTask.TxNum, txTask.BlockNum, txTask.TxIndex)
		txHash := txTask.Tx.Hash()
		gp := new(core.GasPool).AddGas(txTask.Tx.GetGas())
		vmConfig := vm.Config{NoReceipts: true, SkipAnalysis: core.SkipAnalysis(rw.chainConfig, txTask.BlockNum)}
		contractHasTEVM := func(contractHash common.Hash) (bool, error) { return false, nil }
		ibs.Prepare(txHash, txTask.BlockHash, txTask.TxIndex)
		getHashFn := core.GetHashFn(txTask.Header, rw.getHeader)
		blockContext := core.NewEVMBlockContext(txTask.Header, getHashFn, rw.engine, nil /* author */, contractHasTEVM)
		msg, err := txTask.Tx.AsMessage(*types.MakeSigner(rw.chainConfig, txTask.BlockNum), txTask.Header.BaseFee, txTask.Rules)
		if err != nil {
			panic(err)
		}
		txContext := core.NewEVMTxContext(msg)
		vmenv := vm.NewEVM(blockContext, txContext, ibs, rw.chainConfig, vmConfig)
		if _, err = core.ApplyMessage(vmenv, msg, gp, true /* refunds */, false /* gasBailout */); err != nil {
			txTask.Error = err
			//fmt.Printf("error=%v\n", err)
		}
		// Update the state with pending changes
		ibs.SoftFinalise()
	}
	// Prepare read set, write set and balanceIncrease set and send for serialisation
	if txTask.Error == nil {
		txTask.BalanceIncreaseSet = ibs.BalanceIncreaseSet()
		//for addr, bal := range txTask.BalanceIncreaseSet {
		//	fmt.Printf("[%x]=>[%d]\n", addr, &bal)
		//}
		if err = ibs.MakeWriteSet(txTask.Rules, rw.stateWriter); err != nil {
			panic(err)
		}
		txTask.ReadLists = rw.stateReader.ReadSet()
		txTask.WriteLists = rw.stateWriter.WriteSet()
		txTask.AccountPrevs, txTask.AccountDels, txTask.StoragePrevs, txTask.CodePrevs = rw.stateWriter.PrevAndDels()
		size := (20 + 32) * len(txTask.BalanceIncreaseSet)
		for _, list := range txTask.ReadLists {
			for _, b := range list.Keys {
				size += len(b)
			}
			for _, b := range list.Vals {
				size += len(b)
			}
		}
		for _, list := range txTask.WriteLists {
			for _, b := range list.Keys {
				size += len(b)
			}
			for _, b := range list.Vals {
				size += len(b)
			}
		}
		txTask.ResultsSize = int64(size)
	}
}

func processResultQueue(rws *state.TxTaskQueue, outputTxNum *uint64, rs *state.State22, agg *libstate.Aggregator22, applyTx kv.Tx,
	triggerCount *uint64, outputBlockNum *uint64, repeatCount *uint64, resultsSize *int64) {
	for rws.Len() > 0 && (*rws)[0].TxNum == *outputTxNum {
		txTask := heap.Pop(rws).(state.TxTask)
		atomic.AddInt64(resultsSize, -txTask.ResultsSize)
		if txTask.Error == nil && rs.ReadsValid(txTask.ReadLists) {
			if err := rs.Apply(txTask.Rules.IsSpuriousDragon, applyTx, txTask, agg); err != nil {
				panic(err)
			}
			*triggerCount += rs.CommitTxNum(txTask.Sender, txTask.TxNum)
			*outputTxNum++
			*outputBlockNum = txTask.BlockNum
			//fmt.Printf("Applied %d block %d txIndex %d\n", txTask.TxNum, txTask.BlockNum, txTask.TxIndex)
		} else {
			rs.AddWork(txTask)
			*repeatCount++
			//fmt.Printf("Rolled back %d block %d txIndex %d\n", txTask.TxNum, txTask.BlockNum, txTask.TxIndex)
		}
	}
}

func Erigon22(genesis *core.Genesis, logger log.Logger) error {
	sigs := make(chan os.Signal, 1)
	interruptCh := make(chan bool, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigs
		interruptCh <- true
	}()
	var err error
	ctx := context.Background()
	reconDbPath := path.Join(datadir, "db22")
	if reset {
		if _, err = os.Stat(reconDbPath); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		} else if err = os.RemoveAll(reconDbPath); err != nil {
			return err
		}
	}
	limiter := semaphore.NewWeighted(int64(runtime.NumCPU() + 1))
	db, err := kv2.NewMDBX(logger).Path(reconDbPath).RoTxsLimiter(limiter).Open()
	if err != nil {
		return err
	}
	startTime := time.Now()
	var blockReader services.FullBlockReader
	var allSnapshots *snapshotsync.RoSnapshots
	allSnapshots = snapshotsync.NewRoSnapshots(ethconfig.NewSnapCfg(true, false, true), path.Join(datadir, "snapshots"))
	defer allSnapshots.Close()
	if err := allSnapshots.ReopenFolder(); err != nil {
		return fmt.Errorf("reopen snapshot segments: %w", err)
	}
	blockReader = snapshotsync.NewBlockReaderWithSnapshots(allSnapshots)
	// Compute mapping blockNum -> last TxNum in that block
	maxBlockNum := allSnapshots.BlocksAvailable() + 1
	txNums := make([]uint64, maxBlockNum)
	if err = allSnapshots.Bodies.View(func(bs []*snapshotsync.BodySegment) error {
		for _, b := range bs {
			if err = b.Iterate(func(blockNum, baseTxNum, txAmount uint64) {
				txNums[blockNum] = baseTxNum + txAmount
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("build txNum => blockNum mapping: %w", err)
	}
	workerCount := runtime.NumCPU()
	workCh := make(chan state.TxTask, 128)

	engine := initConsensusEngine(chainConfig, logger, allSnapshots)
	sentryControlServer, err := sentry.NewMultiClient(
		db,
		"",
		chainConfig,
		common.Hash{},
		engine,
		1,
		nil,
		ethconfig.Defaults.Sync,
		blockReader,
		false,
	)
	if err != nil {
		return err
	}
	cfg := ethconfig.Defaults
	cfg.DeprecatedTxPool.Disable = true
	cfg.Dirs = datadir2.New(datadir)
	cfg.Snapshot = allSnapshots.Cfg()
	stagedSync, err := stages2.NewStagedSync(context.Background(), logger, db, p2p.Config{}, cfg, sentryControlServer, datadir, &stagedsync.Notifications{}, nil, allSnapshots, nil, nil)
	if err != nil {
		return err
	}
	rwTx, err := db.BeginRw(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if rwTx != nil {
			rwTx.Rollback()
		}
	}()
	execStage, err := stagedSync.StageState(stages.Execution, rwTx, db)
	if err != nil {
		return err
	}
	if !reset {
		block = execStage.BlockNumber + 1
	}
	rwTx.Rollback()

	rs := state.NewState22()
	aggDir := path.Join(datadir, "agg22")
	if reset {
		if _, err = os.Stat(aggDir); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		} else if err = os.RemoveAll(aggDir); err != nil {
			return err
		}
		if err = os.MkdirAll(aggDir, 0755); err != nil {
			return err
		}
	}
	agg, err := libstate.NewAggregator22(aggDir, AggregationStep)
	if err != nil {
		return err
	}
	defer agg.Close()
	var lock sync.RWMutex
	reconWorkers := make([]*Worker22, workerCount)
	var wg sync.WaitGroup
	resultCh := make(chan state.TxTask, 128)
	for i := 0; i < workerCount; i++ {
		reconWorkers[i] = NewWorker22(lock.RLocker(), db, &wg, rs, blockReader, allSnapshots, txNums, chainConfig, logger, genesis, resultCh)
	}
	defer func() {
		for i := 0; i < workerCount; i++ {
			reconWorkers[i].ResetTx()
		}
	}()
	wg.Add(workerCount)
	for i := 0; i < workerCount; i++ {
		go reconWorkers[i].run()
	}
	commitThreshold := uint64(1024 * 1024 * 1024)
	resultsThreshold := int64(1024 * 1024 * 1024)
	count := uint64(0)
	repeatCount := uint64(0)
	triggerCount := uint64(0)
	prevCount := uint64(0)
	prevRepeatCount := uint64(0)
	//prevTriggerCount := uint64(0)
	resultsSize := int64(0)
	prevTime := time.Now()
	logEvery := time.NewTicker(logInterval)
	defer logEvery.Stop()
	var rws state.TxTaskQueue
	var rwsLock sync.Mutex
	rwsReceiveCond := sync.NewCond(&rwsLock)
	heap.Init(&rws)
	var outputTxNum uint64
	if block > 0 {
		outputTxNum = txNums[block-1]
	}
	var inputBlockNum, outputBlockNum uint64
	var prevOutputBlockNum uint64 = block
	// Go-routine gathering results from the workers
	var maxTxNum uint64 = txNums[len(txNums)-1]
	go func() {
		var applyTx kv.RwTx
		defer func() {
			if applyTx != nil {
				applyTx.Rollback()
			}
		}()
		if applyTx, err = db.BeginRw(ctx); err != nil {
			panic(err)
		}
		agg.SetTx(applyTx)
		defer rs.Finish()
		var waiting, applying time.Duration
		waitStart := time.Now()
		var waitEnd time.Time
		for outputTxNum < atomic.LoadUint64(&maxTxNum) {
			select {
			case txTask := <-resultCh:
				waitEnd = time.Now()
				waiting += (waitEnd.Sub(waitStart))
				//fmt.Printf("Saved %d block %d txIndex %d\n", txTask.TxNum, txTask.BlockNum, txTask.TxIndex)
				func() {
					rwsLock.Lock()
					defer rwsLock.Unlock()
					atomic.AddInt64(&resultsSize, txTask.ResultsSize)
					heap.Push(&rws, txTask)
					processResultQueue(&rws, &outputTxNum, rs, agg, applyTx, &triggerCount, &outputBlockNum, &repeatCount, &resultsSize)
					rwsReceiveCond.Signal()
				}()
				waitStart = time.Now()
				applying += waitStart.Sub(waitEnd)
			case <-logEvery.C:
				var m runtime.MemStats
				libcommon.ReadMemStats(&m)
				sizeEstimate := rs.SizeEstimate()
				count = rs.DoneCount()
				currentTime := time.Now()
				interval := currentTime.Sub(prevTime)
				speedTx := float64(count-prevCount) / (float64(interval) / float64(time.Second))
				speedBlock := float64(outputBlockNum-prevOutputBlockNum) / (float64(interval) / float64(time.Second))
				var repeatRatio float64
				if count > prevCount {
					repeatRatio = 100.0 * float64(repeatCount-prevRepeatCount) / float64(count-prevCount)
				}
				log.Info("Transaction replay",
					//"workers", workerCount,
					"at block", outputBlockNum,
					"input block", atomic.LoadUint64(&inputBlockNum),
					"blk/s", fmt.Sprintf("%.1f", speedBlock),
					"tx/s", fmt.Sprintf("%.1f", speedTx),
					"waiting", waiting,
					"applying", applying,
					//"repeats", repeatCount-prevRepeatCount,
					//"triggered", triggerCount-prevTriggerCount,
					"result queue", rws.Len(),
					"results size", libcommon.ByteCount(uint64(atomic.LoadInt64(&resultsSize))),
					"repeat ratio", fmt.Sprintf("%.2f%%", repeatRatio),
					"buffer", libcommon.ByteCount(sizeEstimate),
					"alloc", libcommon.ByteCount(m.Alloc), "sys", libcommon.ByteCount(m.Sys),
				)
				prevTime = currentTime
				prevCount = count
				prevOutputBlockNum = outputBlockNum
				prevRepeatCount = repeatCount
				//prevTriggerCount = triggerCount
				if sizeEstimate >= commitThreshold {
					commitStart := time.Now()
					log.Info("Committing...")
					err := func() error {
						rwsLock.Lock()
						defer rwsLock.Unlock()
						// Drain results (and process) channel because read sets do not carry over
						for {
							var drained bool
							for !drained {
								select {
								case txTask := <-resultCh:
									atomic.AddInt64(&resultsSize, txTask.ResultsSize)
									heap.Push(&rws, txTask)
								default:
									drained = true
								}
							}
							processResultQueue(&rws, &outputTxNum, rs, agg, applyTx, &triggerCount, &outputBlockNum, &repeatCount, &resultsSize)
							if rws.Len() == 0 {
								break
							}
						}
						rwsReceiveCond.Signal()
						lock.Lock() // This is to prevent workers from starting work on any new txTask
						defer lock.Unlock()
						// Drain results channel because read sets do not carry over
						var drained bool
						for !drained {
							select {
							case txTask := <-resultCh:
								rs.AddWork(txTask)
							default:
								drained = true
							}
						}
						// Drain results queue as well
						for rws.Len() > 0 {
							txTask := heap.Pop(&rws).(state.TxTask)
							atomic.AddInt64(&resultsSize, -txTask.ResultsSize)
							rs.AddWork(txTask)
						}
						if err = applyTx.Commit(); err != nil {
							return err
						}
						for i := 0; i < workerCount; i++ {
							reconWorkers[i].ResetTx()
						}
						rwTx, err = db.BeginRw(ctx)
						if err != nil {
							return err
						}
						if err = rs.Flush(rwTx); err != nil {
							return err
						}
						if err = rwTx.Commit(); err != nil {
							return err
						}
						if applyTx, err = db.BeginRw(ctx); err != nil {
							return err
						}
						agg.SetTx(applyTx)
						return nil
					}()
					if err != nil {
						panic(err)
					}
					log.Info("Committed", "time", time.Since(commitStart))
				}
				waiting = 0
				applying = 0
			}
		}
		if err = applyTx.Commit(); err != nil {
			panic(err)
		}
	}()
	var inputTxNum uint64
	if block > 0 {
		inputTxNum = txNums[block-1]
	}
	var header *types.Header
	var blockNum uint64
loop:
	for blockNum = block; blockNum < maxBlockNum; blockNum++ {
		atomic.StoreUint64(&inputBlockNum, blockNum)
		rules := chainConfig.Rules(blockNum)
		if header, err = blockReader.HeaderByNumber(ctx, nil, blockNum); err != nil {
			return err
		}
		blockHash := header.Hash()
		b, _, err := blockReader.BlockWithSenders(ctx, nil, blockHash, blockNum)
		if err != nil {
			return err
		}
		txs := b.Transactions()
		for txIndex := -1; txIndex <= len(txs); txIndex++ {
			// Do not oversend, wait for the result heap to go under certain size
			func() {
				rwsLock.Lock()
				defer rwsLock.Unlock()
				for rws.Len() > 128 || atomic.LoadInt64(&resultsSize) >= resultsThreshold || rs.SizeEstimate() >= commitThreshold {
					rwsReceiveCond.Wait()
				}
			}()
			txTask := state.TxTask{
				Header:    header,
				BlockNum:  blockNum,
				Rules:     rules,
				Block:     b,
				TxNum:     inputTxNum,
				TxIndex:   txIndex,
				BlockHash: blockHash,
				Final:     txIndex == len(txs),
			}
			if txIndex >= 0 && txIndex < len(txs) {
				txTask.Tx = txs[txIndex]
				if sender, ok := txs[txIndex].GetSender(); ok {
					txTask.Sender = &sender
				}
				if ok := rs.RegisterSender(txTask); ok {
					rs.AddWork(txTask)
				}
			} else {
				rs.AddWork(txTask)
			}
			inputTxNum++
		}
		// Check for interrupts
		select {
		case <-interruptCh:
			log.Info(fmt.Sprintf("interrupted, please wait for cleanup, next run will start with block %d", blockNum+1))
			atomic.StoreUint64(&maxTxNum, inputTxNum)
			break loop
		default:
		}
	}
	close(workCh)
	wg.Wait()
	for i := 0; i < workerCount; i++ {
		reconWorkers[i].ResetTx()
	}
	rwTx, err = db.BeginRw(ctx)
	if err != nil {
		return err
	}
	if err = rs.Flush(rwTx); err != nil {
		return err
	}
	if err = execStage.Update(rwTx, blockNum); err != nil {
		return err
	}
	if err = rwTx.Commit(); err != nil {
		return err
	}
	if rwTx, err = db.BeginRw(ctx); err != nil {
		return err
	}
	log.Info("Transaction replay complete", "duration", time.Since(startTime))
	log.Info("Computing hashed state")
	tmpDir := filepath.Join(datadir, "tmp")
	if err = rwTx.ClearBucket(kv.HashedAccounts); err != nil {
		return err
	}
	if err = rwTx.ClearBucket(kv.HashedStorage); err != nil {
		return err
	}
	if err = rwTx.ClearBucket(kv.ContractCode); err != nil {
		return err
	}
	if err = stagedsync.PromoteHashedStateCleanly("recon", rwTx, stagedsync.StageHashStateCfg(db, tmpDir), ctx); err != nil {
		return err
	}
	if err = rwTx.Commit(); err != nil {
		return err
	}
	if rwTx, err = db.BeginRw(ctx); err != nil {
		return err
	}
	var rootHash common.Hash
	if rootHash, err = stagedsync.RegenerateIntermediateHashes("recon", rwTx, stagedsync.StageTrieCfg(db, false /* checkRoot */, false /* saveHashesToDB */, false /* badBlockHalt */, tmpDir, blockReader, nil /* HeaderDownload */), common.Hash{}, make(chan struct{}, 1)); err != nil {
		return err
	}
	if err = rwTx.Commit(); err != nil {
		return err
	}
	if rootHash != header.Root {
		log.Error("Incorrect root hash", "expected", fmt.Sprintf("%x", header.Root))
	}
	return nil
}
