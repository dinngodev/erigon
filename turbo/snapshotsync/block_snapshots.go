package snapshotsync

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/holiman/uint256"
	common2 "github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/cmp"
	"github.com/ledgerwatch/erigon-lib/common/dbg"
	"github.com/ledgerwatch/erigon-lib/compress"
	"github.com/ledgerwatch/erigon-lib/etl"
	proto_downloader "github.com/ledgerwatch/erigon-lib/gointerfaces/downloader"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/recsplit"
	types2 "github.com/ledgerwatch/erigon-lib/types"
	"github.com/ledgerwatch/erigon/cmd/downloader/downloadergrpc"
	"github.com/ledgerwatch/erigon/cmd/hack/tool"
	"github.com/ledgerwatch/erigon/common"
	"github.com/ledgerwatch/erigon/common/dbutils"
	"github.com/ledgerwatch/erigon/core/rawdb"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/crypto"
	"github.com/ledgerwatch/erigon/eth/ethconfig"
	"github.com/ledgerwatch/erigon/eth/stagedsync/stages"
	"github.com/ledgerwatch/erigon/params"
	"github.com/ledgerwatch/erigon/rlp"
	"github.com/ledgerwatch/erigon/turbo/snapshotsync/snap"
	"github.com/ledgerwatch/erigon/turbo/snapshotsync/snapcfg"
	"github.com/ledgerwatch/log/v3"
	"go.uber.org/atomic"
	"golang.org/x/exp/slices"
	"golang.org/x/sync/semaphore"
)

type DownloadRequest struct {
	ranges      *Range
	path        string
	torrentHash string
}

type HeaderSegment struct {
	seg           *compress.Decompressor // value: first_byte_of_header_hash + header_rlp
	idxHeaderHash *recsplit.Index        // header_hash       -> headers_segment_offset
	ranges        Range
}

type BodySegment struct {
	seg           *compress.Decompressor // value: rlp(types.BodyForStorage)
	idxBodyNumber *recsplit.Index        // block_num_u64     -> bodies_segment_offset
	ranges        Range
}

type TxnSegment struct {
	Seg                 *compress.Decompressor // value: first_byte_of_transaction_hash + sender_address + transaction_rlp
	IdxTxnHash          *recsplit.Index        // transaction_hash  -> transactions_segment_offset
	IdxTxnHash2BlockNum *recsplit.Index        // transaction_hash  -> block_number
	ranges              Range
}

func (sn *HeaderSegment) closeIdx() {
	if sn.idxHeaderHash != nil {
		sn.idxHeaderHash.Close()
		sn.idxHeaderHash = nil
	}
}
func (sn *HeaderSegment) closeSeg() {
	if sn.seg != nil {
		sn.seg.Close()
		sn.seg = nil
	}
}
func (sn *HeaderSegment) close() {
	sn.closeSeg()
	sn.closeIdx()
}
func (sn *HeaderSegment) reopenSeg(dir string) (err error) {
	sn.closeSeg()
	fileName := snap.SegmentFileName(sn.ranges.from, sn.ranges.to, snap.Headers)
	sn.seg, err = compress.NewDecompressor(path.Join(dir, fileName))
	if err != nil {
		return fmt.Errorf("%w, fileName: %s", err, fileName)
	}
	return nil
}
func (sn *HeaderSegment) reopenIdxIfNeed(dir string, optimistic bool) (err error) {
	if sn.idxHeaderHash != nil {
		return nil
	}
	err = sn.reopenIdx(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			if optimistic {
				log.Warn("[snapshots] open index", "err", err)
			} else {
				return err
			}
		}
	}
	return nil
}
func (sn *HeaderSegment) reopenIdx(dir string) (err error) {
	sn.closeIdx()
	fileName := snap.IdxFileName(sn.ranges.from, sn.ranges.to, snap.Headers.String())
	sn.idxHeaderHash, err = recsplit.OpenIndex(path.Join(dir, fileName))
	if err != nil {
		return fmt.Errorf("%w, fileName: %s", err, fileName)
	}
	return nil
}

func (sn *BodySegment) closeSeg() {
	if sn.seg != nil {
		sn.seg.Close()
		sn.seg = nil
	}
}
func (sn *BodySegment) closeIdx() {
	if sn.idxBodyNumber != nil {
		sn.idxBodyNumber.Close()
		sn.idxBodyNumber = nil
	}
}
func (sn *BodySegment) close() {
	sn.closeSeg()
	sn.closeIdx()
}

func (sn *BodySegment) reopenSeg(dir string) (err error) {
	sn.closeSeg()
	fileName := snap.SegmentFileName(sn.ranges.from, sn.ranges.to, snap.Bodies)
	sn.seg, err = compress.NewDecompressor(path.Join(dir, fileName))
	if err != nil {
		return fmt.Errorf("%w, fileName: %s", err, fileName)
	}
	return nil
}
func (sn *BodySegment) reopenIdxIfNeed(dir string, optimistic bool) (err error) {
	if sn.idxBodyNumber != nil {
		return nil
	}
	err = sn.reopenIdx(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			if optimistic {
				log.Warn("[snapshots] open index", "err", err)
			} else {
				return err
			}
		}
	}
	return nil
}

func (sn *BodySegment) reopenIdx(dir string) (err error) {
	sn.closeIdx()
	fileName := snap.IdxFileName(sn.ranges.from, sn.ranges.to, snap.Bodies.String())
	sn.idxBodyNumber, err = recsplit.OpenIndex(path.Join(dir, fileName))
	if err != nil {
		return fmt.Errorf("%w, fileName: %s", err, fileName)
	}
	return nil
}

func (sn *BodySegment) Iterate(f func(blockNum, baseTxNum, txAmout uint64)) error {
	var buf []byte
	g := sn.seg.MakeGetter()
	blockNum := sn.idxBodyNumber.BaseDataID()
	var b types.BodyForStorage
	for g.HasNext() {
		buf, _ = g.Next(buf[:0])
		if err := rlp.DecodeBytes(buf, &b); err != nil {
			return err
		}
		f(blockNum, b.BaseTxId, uint64(b.TxAmount))
		blockNum++
	}
	return nil
}

func (sn *TxnSegment) closeIdx() {
	if sn.IdxTxnHash != nil {
		sn.IdxTxnHash.Close()
		sn.IdxTxnHash = nil
	}
	if sn.IdxTxnHash2BlockNum != nil {
		sn.IdxTxnHash2BlockNum.Close()
		sn.IdxTxnHash2BlockNum = nil
	}
}
func (sn *TxnSegment) closeSeg() {
	if sn.Seg != nil {
		sn.Seg.Close()
		sn.Seg = nil
	}
}
func (sn *TxnSegment) close() {
	sn.closeSeg()
	sn.closeIdx()
}
func (sn *TxnSegment) reopenSeg(dir string) (err error) {
	sn.closeSeg()
	fileName := snap.SegmentFileName(sn.ranges.from, sn.ranges.to, snap.Transactions)
	sn.Seg, err = compress.NewDecompressor(path.Join(dir, fileName))
	if err != nil {
		return fmt.Errorf("%w, fileName: %s", err, fileName)
	}
	return nil
}
func (sn *TxnSegment) reopenIdx(dir string) (err error) {
	sn.closeIdx()
	fileName := snap.IdxFileName(sn.ranges.from, sn.ranges.to, snap.Transactions.String())
	sn.IdxTxnHash, err = recsplit.OpenIndex(path.Join(dir, fileName))
	if err != nil {
		return fmt.Errorf("%w, fileName: %s", err, fileName)
	}

	fileName = snap.IdxFileName(sn.ranges.from, sn.ranges.to, snap.Transactions2Block.String())
	sn.IdxTxnHash2BlockNum, err = recsplit.OpenIndex(path.Join(dir, fileName))
	if err != nil {
		return fmt.Errorf("%w, fileName: %s", err, fileName)
	}
	return nil
}

func (sn *TxnSegment) reopenIdxIfNeed(dir string, optimistic bool) (err error) {
	if sn.IdxTxnHash != nil && sn.IdxTxnHash2BlockNum != nil {
		return nil
	}
	err = sn.reopenIdx(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			if optimistic {
				log.Warn("[snapshots] open index", "err", err)
			} else {
				return err
			}
		}
	}
	return nil
}

type headerSegments struct {
	lock     sync.RWMutex
	segments []*HeaderSegment
}

func (s *headerSegments) View(f func(segments []*HeaderSegment) error) error {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return f(s.segments)
}
func (s *headerSegments) ViewSegment(blockNum uint64, f func(sn *HeaderSegment) error) (found bool, err error) {
	s.lock.RLock()
	defer s.lock.RUnlock()
	for _, seg := range s.segments {
		if !(blockNum >= seg.ranges.from && blockNum < seg.ranges.to) {
			continue
		}
		return true, f(seg)
	}
	return false, nil
}

type bodySegments struct {
	lock     sync.RWMutex
	segments []*BodySegment
}

func (s *bodySegments) View(f func([]*BodySegment) error) error {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return f(s.segments)
}
func (s *bodySegments) ViewSegment(blockNum uint64, f func(*BodySegment) error) (found bool, err error) {
	s.lock.RLock()
	defer s.lock.RUnlock()
	for _, seg := range s.segments {
		if !(blockNum >= seg.ranges.from && blockNum < seg.ranges.to) {
			continue
		}
		return true, f(seg)
	}
	return false, nil
}

type txnSegments struct {
	lock     sync.RWMutex
	segments []*TxnSegment
}

func (s *txnSegments) View(f func([]*TxnSegment) error) error {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return f(s.segments)
}
func (s *txnSegments) ViewSegment(blockNum uint64, f func(*TxnSegment) error) (found bool, err error) {
	s.lock.RLock()
	defer s.lock.RUnlock()
	for _, seg := range s.segments {
		if !(blockNum >= seg.ranges.from && blockNum < seg.ranges.to) {
			continue
		}
		return true, f(seg)
	}
	return false, nil
}

type RoSnapshots struct {
	indicesReady  atomic.Bool
	segmentsReady atomic.Bool

	Headers *headerSegments
	Bodies  *bodySegments
	Txs     *txnSegments

	dir         string
	segmentsMax atomic.Uint64 // all types of .seg files are available - up to this number
	idxMax      atomic.Uint64 // all types of .idx files are available - up to this number
	cfg         ethconfig.Snapshot
}

// NewRoSnapshots - opens all snapshots. But to simplify everything:
//  - it opens snapshots only on App start and immutable after
//  - all snapshots of given blocks range must exist - to make this blocks range available
//  - gaps are not allowed
//  - segment have [from:to) semantic
func NewRoSnapshots(cfg ethconfig.Snapshot, snapDir string) *RoSnapshots {
	return &RoSnapshots{dir: snapDir, cfg: cfg, Headers: &headerSegments{}, Bodies: &bodySegments{}, Txs: &txnSegments{}}
}

func (s *RoSnapshots) Cfg() ethconfig.Snapshot { return s.cfg }
func (s *RoSnapshots) Dir() string             { return s.dir }
func (s *RoSnapshots) SegmentsReady() bool     { return s.segmentsReady.Load() }
func (s *RoSnapshots) IndicesReady() bool      { return s.indicesReady.Load() }
func (s *RoSnapshots) IndicesMax() uint64      { return s.idxMax.Load() }
func (s *RoSnapshots) SegmentsMax() uint64     { return s.segmentsMax.Load() }
func (s *RoSnapshots) BlocksAvailable() uint64 { return cmp.Min(s.segmentsMax.Load(), s.idxMax.Load()) }
func (s *RoSnapshots) LogStat() {
	var m runtime.MemStats
	common2.ReadMemStats(&m)
	log.Info("[Snapshots] Stat",
		"blocks", fmt.Sprintf("%dk", (s.BlocksAvailable()+1)/1000),
		"indices", fmt.Sprintf("%dk", (s.IndicesMax()+1)/1000),
		"alloc", common2.ByteCount(m.Alloc), "sys", common2.ByteCount(m.Sys))
}

func (s *RoSnapshots) EnsureExpectedBlocksAreAvailable(cfg *snapcfg.Cfg) error {
	if s.BlocksAvailable() < cfg.ExpectBlocks {
		return fmt.Errorf("app must wait until all expected snapshots are available. Expected: %d, Available: %d", cfg.ExpectBlocks, s.BlocksAvailable())
	}
	return nil
}

func (s *RoSnapshots) idxAvailability() uint64 {
	var headers, bodies, txs uint64
	for _, seg := range s.Headers.segments {
		if seg.idxHeaderHash == nil {
			break
		}
		headers = seg.ranges.to - 1
	}
	for _, seg := range s.Bodies.segments {
		if seg.idxBodyNumber == nil {
			break
		}
		bodies = seg.ranges.to - 1
	}
	for _, seg := range s.Txs.segments {
		if seg.IdxTxnHash == nil || seg.IdxTxnHash2BlockNum == nil {
			break
		}
		txs = seg.ranges.to - 1
	}
	return cmp.Min(headers, cmp.Min(bodies, txs))
}

// OptimisticReopenWithDB - optimistically open snapshots (ignoring error), useful at App startup because:
// - user must be able: delete any snapshot file and Erigon will self-heal by re-downloading
// - RPC return Nil for historical blocks if snapshots are not open
func (s *RoSnapshots) OptimisticReopenWithDB(db kv.RoDB) {
	_ = db.View(context.Background(), func(tx kv.Tx) error {
		snList, err := rawdb.ReadSnapshots(tx)
		if err != nil {
			return err
		}
		return s.ReopenList(snList, true)
	})
}

func (s *RoSnapshots) Files() (list []string) {
	s.Headers.lock.RLock()
	defer s.Headers.lock.RUnlock()
	s.Bodies.lock.RLock()
	defer s.Bodies.lock.RUnlock()
	s.Txs.lock.RLock()
	defer s.Txs.lock.RUnlock()
	max := s.BlocksAvailable()
	for _, seg := range s.Bodies.segments {
		if seg.ranges.from > max {
			continue
		}
		_, fName := filepath.Split(seg.seg.FilePath())
		list = append(list, fName)
	}
	for _, seg := range s.Headers.segments {
		if seg.ranges.from > max {
			continue
		}
		_, fName := filepath.Split(seg.seg.FilePath())
		list = append(list, fName)
	}
	for _, seg := range s.Txs.segments {
		if seg.ranges.from > max {
			continue
		}
		_, fName := filepath.Split(seg.Seg.FilePath())
		list = append(list, fName)
	}
	return list
}

// ReopenList stops on optimistic=false, continue opening files on optimistic=true
func (s *RoSnapshots) ReopenList(fileNames []string, optimistic bool) error {
	s.Headers.lock.Lock()
	defer s.Headers.lock.Unlock()
	s.Bodies.lock.Lock()
	defer s.Bodies.lock.Unlock()
	s.Txs.lock.Lock()
	defer s.Txs.lock.Unlock()

	s.closeWhatNotInList(fileNames)
	var segmentsMax uint64
	var segmentsMaxSet bool
Loop:
	for _, fName := range fileNames {
		f, err := snap.ParseFileName(s.dir, fName)
		if err != nil {
			log.Warn("invalid segment name", "err", err, "name", fName)
			continue
		}

		switch f.T {
		case snap.Headers:
			for _, sn := range s.Headers.segments {
				_, name := filepath.Split(sn.seg.FilePath())
				if fName == name {
					if err := sn.reopenIdxIfNeed(s.dir, optimistic); err != nil {
						return err
					}
					continue Loop
				}
			}

			sn := &HeaderSegment{ranges: Range{f.From, f.To}}
			if err := sn.reopenSeg(s.dir); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					if optimistic {
						continue Loop
					} else {
						break Loop
					}
				}
				if optimistic {
					log.Warn("[snapshots] open segment", "err", err)
					continue Loop
				} else {
					return err
				}
			}

			// it's possible to iterate over .seg file even if you don't have index
			// then make segment available even if index open may fail
			s.Headers.segments = append(s.Headers.segments, sn)
			if err := sn.reopenIdxIfNeed(s.dir, optimistic); err != nil {
				return err
			}
		case snap.Bodies:
			for _, sn := range s.Bodies.segments {
				_, name := filepath.Split(sn.seg.FilePath())
				if fName == name {
					if err := sn.reopenIdxIfNeed(s.dir, optimistic); err != nil {
						return err
					}
					continue Loop
				}
			}

			sn := &BodySegment{ranges: Range{f.From, f.To}}
			if err := sn.reopenSeg(s.dir); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					if optimistic {
						continue Loop
					} else {
						break Loop
					}
				}
				if optimistic {
					log.Warn("[snapshots] open segment", "err", err)
					continue Loop
				} else {
					return err
				}
			}
			s.Bodies.segments = append(s.Bodies.segments, sn)
			if err := sn.reopenIdxIfNeed(s.dir, optimistic); err != nil {
				return err
			}
		case snap.Transactions:
			for _, sn := range s.Txs.segments {
				_, name := filepath.Split(sn.Seg.FilePath())
				if fName == name {
					if err := sn.reopenIdxIfNeed(s.dir, optimistic); err != nil {
						return err
					}
					continue Loop
				}
			}

			sn := &TxnSegment{ranges: Range{f.From, f.To}}
			if err := sn.reopenSeg(s.dir); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					if optimistic {
						continue Loop
					} else {
						break Loop
					}
				}
				if optimistic {
					log.Warn("[snapshots] open segment", "err", err)
					continue Loop
				} else {
					return err
				}
			}
			s.Txs.segments = append(s.Txs.segments, sn)
			if err := sn.reopenIdxIfNeed(s.dir, optimistic); err != nil {
				return err
			}
		}

		if f.To > 0 {
			segmentsMax = f.To - 1
		} else {
			segmentsMax = 0
		}
		segmentsMaxSet = true
	}
	if segmentsMaxSet {
		s.segmentsMax.Store(segmentsMax)
	}
	s.segmentsReady.Store(true)
	s.idxMax.Store(s.idxAvailability())
	s.indicesReady.Store(true)

	return nil
}

func (s *RoSnapshots) Ranges() (ranges []Range) {
	_ = s.Headers.View(func(segments []*HeaderSegment) error {
		for _, sn := range segments {
			ranges = append(ranges, sn.ranges)
		}
		return nil
	})
	return ranges
}

func (s *RoSnapshots) ReopenFolder() error {
	files, _, err := Segments(s.dir)
	if err != nil {
		return err
	}
	var list []string
	for _, f := range files {
		_, fName := filepath.Split(f.Path)
		list = append(list, fName)
	}
	return s.ReopenList(list, false)
}
func (s *RoSnapshots) ReopenWithDB(db kv.RoDB) error {
	if err := db.View(context.Background(), func(tx kv.Tx) error {
		snList, err := rawdb.ReadSnapshots(tx)
		if err != nil {
			return err
		}
		return s.ReopenList(snList, true)
	}); err != nil {
		return err
	}
	return nil
}

func (s *RoSnapshots) Close() {
	s.Headers.lock.Lock()
	defer s.Headers.lock.Unlock()
	s.Bodies.lock.Lock()
	defer s.Bodies.lock.Unlock()
	s.Txs.lock.Lock()
	defer s.Txs.lock.Unlock()
	s.closeWhatNotInList(nil)
}

func (s *RoSnapshots) closeWhatNotInList(l []string) {
Loop1:
	for i, sn := range s.Headers.segments {
		_, name := filepath.Split(sn.seg.FilePath())
		for _, fName := range l {
			if fName == name {
				continue Loop1
			}
		}
		sn.close()
		s.Headers.segments[i] = nil
	}
Loop2:
	for i, sn := range s.Bodies.segments {
		_, name := filepath.Split(sn.seg.FilePath())
		for _, fName := range l {
			if fName == name {
				continue Loop2
			}
		}
		sn.close()
		s.Bodies.segments[i] = nil
	}
Loop3:
	for i, sn := range s.Txs.segments {
		_, name := filepath.Split(sn.Seg.FilePath())
		for _, fName := range l {
			if fName == name {
				continue Loop3
			}
		}
		sn.close()
		s.Txs.segments[i] = nil
	}
	var i int
	for i = 0; i < len(s.Headers.segments) && s.Headers.segments[i] != nil; i++ {
	}
	tail := s.Headers.segments[i:]
	s.Headers.segments = s.Headers.segments[:i]
	for i = 0; i < len(tail); i++ {
		if tail[i] != nil {
			tail[i].close()
			tail[i] = nil
		}
	}

	for i = 0; i < len(s.Bodies.segments) && s.Bodies.segments[i] != nil; i++ {
	}
	tailB := s.Bodies.segments[i:]
	s.Bodies.segments = s.Bodies.segments[:i]
	for i = 0; i < len(tailB); i++ {
		if tailB[i] != nil {
			tailB[i].close()
			tailB[i] = nil
		}
	}

	for i = 0; i < len(s.Txs.segments) && s.Txs.segments[i] != nil; i++ {
	}
	tailC := s.Txs.segments[i:]
	s.Txs.segments = s.Txs.segments[:i]
	for i = 0; i < len(tailC); i++ {
		if tailC[i] != nil {
			tailC[i].close()
			tailC[i] = nil
		}
	}
}

func (s *RoSnapshots) PrintDebug() {
	s.Headers.lock.RLock()
	defer s.Headers.lock.RUnlock()
	s.Bodies.lock.RLock()
	defer s.Bodies.lock.RUnlock()
	s.Txs.lock.RLock()
	defer s.Txs.lock.RUnlock()
	fmt.Printf("sn: %d, %d\n", s.segmentsMax.Load(), s.idxMax.Load())
	fmt.Println("    == Snapshots, Header")
	for _, sn := range s.Headers.segments {
		fmt.Printf("%d,  %t\n", sn.ranges.from, sn.idxHeaderHash == nil)
	}
	fmt.Println("    == Snapshots, Body")
	for _, sn := range s.Bodies.segments {
		fmt.Printf("%d,  %t\n", sn.ranges.from, sn.idxBodyNumber == nil)
	}
	fmt.Println("    == Snapshots, Txs")
	for _, sn := range s.Txs.segments {
		fmt.Printf("%d,  %t, %t\n", sn.ranges.from, sn.IdxTxnHash == nil, sn.IdxTxnHash2BlockNum == nil)
	}
}
func (s *RoSnapshots) ViewHeaders(blockNum uint64, f func(sn *HeaderSegment) error) (found bool, err error) {
	if !s.indicesReady.Load() || blockNum > s.BlocksAvailable() {
		return false, nil
	}
	return s.Headers.ViewSegment(blockNum, f)
}
func (s *RoSnapshots) ViewBodies(blockNum uint64, f func(sn *BodySegment) error) (found bool, err error) {
	if !s.indicesReady.Load() || blockNum > s.BlocksAvailable() {
		return false, nil
	}
	return s.Bodies.ViewSegment(blockNum, f)
}
func (s *RoSnapshots) ViewTxs(blockNum uint64, f func(sn *TxnSegment) error) (found bool, err error) {
	if !s.indicesReady.Load() || blockNum > s.BlocksAvailable() {
		return false, nil
	}
	return s.Txs.ViewSegment(blockNum, f)
}

func buildIdx(ctx context.Context, sn snap.FileInfo, chainID uint256.Int, tmpDir string, lvl log.Lvl) error {
	switch sn.T {
	case snap.Headers:
		if err := HeadersIdx(ctx, sn.Path, sn.From, tmpDir, lvl); err != nil {
			return err
		}
	case snap.Bodies:
		if err := BodiesIdx(ctx, sn.Path, sn.From, tmpDir, lvl); err != nil {
			return err
		}
	case snap.Transactions:
		dir, _ := filepath.Split(sn.Path)
		if err := TransactionsIdx(ctx, chainID, sn.From, sn.To, dir, tmpDir, lvl); err != nil {
			return err
		}
	}
	return nil
}

func BuildMissedIndices(ctx context.Context, dir string, chainID uint256.Int, tmpDir string, workers int, lvl log.Lvl) error {
	//log.Log(lvl, "[snapshots] Build indices", "from", min)
	logEvery := time.NewTicker(20 * time.Second)
	defer logEvery.Stop()
	segments, _, err := Segments(dir)
	if err != nil {
		return err
	}
	errs := make(chan error, 1024)
	wg := &sync.WaitGroup{}
	sem := semaphore.NewWeighted(int64(workers))
	for _, t := range snap.AllSnapshotTypes {
		for _, sn := range segments {
			if sn.T != t {
				continue
			}
			if hasIdxFile(&sn) {
				continue
			}
			wg.Add(1)
			if err := sem.Acquire(ctx, 1); err != nil {
				return err
			}
			go func(sn snap.FileInfo) {
				defer sem.Release(1)
				defer wg.Done()

				log.Log(log.LvlInfo, "[snapshots] BuildMissedIndices", "from", sn.From, "to", sn.To)
				if err := buildIdx(ctx, sn, chainID, tmpDir, lvl); err != nil {
					errs <- err
				}

				select {
				case <-ctx.Done():
					errs <- ctx.Err()
					return
				case <-logEvery.C:
					var m runtime.MemStats
					if lvl >= log.LvlInfo {
						common2.ReadMemStats(&m)
					}
					log.Log(lvl, "[snapshots] Indexing", "type", t.String(), "blockNum", sn.To, "alloc", common2.ByteCount(m.Alloc), "sys", common2.ByteCount(m.Sys))
				default:
				}
			}(sn)
		}
	}
	go func() {
		wg.Wait()
		close(errs)
	}()
	for err := range errs {
		if err != nil {
			return err
		}
	}

	return nil
}

func noGaps(in []snap.FileInfo) (out []snap.FileInfo, missingSnapshots []Range) {
	var prevTo uint64
	for _, f := range in {
		if f.To <= prevTo {
			continue
		}
		if f.From != prevTo { // no gaps
			missingSnapshots = append(missingSnapshots, Range{prevTo, f.From})
			continue
		}
		prevTo = f.To
		out = append(out, f)
	}
	return out, missingSnapshots
}

func allTypeOfSegmentsMustExist(dir string, in []snap.FileInfo) (res []snap.FileInfo) {
MainLoop:
	for _, f := range in {
		if f.From == f.To {
			continue
		}
		for _, t := range snap.AllSnapshotTypes {
			p := filepath.Join(dir, snap.SegmentFileName(f.From, f.To, t))
			if !common.FileExist(p) {
				continue MainLoop
			}
		}
		res = append(res, f)
	}
	return res
}

// noOverlaps - keep largest ranges and avoid overlap
func noOverlaps(in []snap.FileInfo) (res []snap.FileInfo) {
	for i := range in {
		f := in[i]
		if f.From == f.To {
			continue
		}

		for j := i + 1; j < len(in); j++ { // if there is file with larger range - use it instead
			f2 := in[j]
			if f.From == f.To {
				continue
			}
			if f2.From > f.From {
				break
			}
			f = f2
			i++
		}

		res = append(res, f)
	}
	return res
}

func SegmentsList(dir string) (res []string, err error) {
	files, _, err := Segments(dir)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		_, fName := filepath.Split(f.Path)
		res = append(res, fName)
	}
	return res, nil
}

// EnforceSnapshotsInvariant if DB has record - then file exists, if file exists - DB has record.
// it also does notify about changes after db commit
func EnforceSnapshotsInvariant(db kv.RwDB, dir string, allSnapshots *RoSnapshots, notifier DBEventNotifier) (snList []string, err error) {
	snListInFolder, err := SegmentsList(dir)
	if err != nil {
		return nil, err
	}
	if err = db.Update(context.Background(), func(tx kv.RwTx) error {
		snList, err = rawdb.EnforceSnapshotsInvariant(tx, snListInFolder)
		if err != nil {
			return err
		}
		return err
	}); err != nil {
		return snList, err
	}
	if allSnapshots != nil {
		if err := allSnapshots.ReopenList(snList, false); err != nil {
			return snList, err
		}
	}
	if notifier != nil {
		notifier.OnNewSnapshot()
	}
	return snList, nil
}

func Segments(dir string) (res []snap.FileInfo, missingSnapshots []Range, err error) {
	list, err := snap.Segments(dir)
	if err != nil {
		return nil, missingSnapshots, err
	}
	{
		var l []snap.FileInfo
		var m []Range
		for _, f := range list {
			if f.T != snap.Headers {
				continue
			}
			l = append(l, f)
		}
		l, m = noGaps(noOverlaps(allTypeOfSegmentsMustExist(dir, l)))
		res = append(res, l...)
		missingSnapshots = append(missingSnapshots, m...)
	}
	{
		var l []snap.FileInfo
		for _, f := range list {
			if f.T != snap.Bodies {
				continue
			}
			l = append(l, f)
		}
		l, _ = noGaps(noOverlaps(allTypeOfSegmentsMustExist(dir, l)))
		res = append(res, l...)
	}
	{
		var l []snap.FileInfo
		for _, f := range list {
			if f.T != snap.Transactions {
				continue
			}
			l = append(l, f)
		}
		l, _ = noGaps(noOverlaps(allTypeOfSegmentsMustExist(dir, l)))
		res = append(res, l...)
	}

	return res, missingSnapshots, nil
}

func chooseSegmentEnd(from, to, blocksPerFile uint64) uint64 {
	next := (from/blocksPerFile + 1) * blocksPerFile
	to = cmp.Min(next, to)
	return to - (to % snap.MIN_SEGMENT_SIZE) // round down to the nearest 1k
}

type BlockRetire struct {
	working atomic.Bool
	wg      *sync.WaitGroup
	result  *BlockRetireResult

	workers   int
	tmpDir    string
	snapshots *RoSnapshots
	db        kv.RoDB

	downloader proto_downloader.DownloaderClient
	notifier   DBEventNotifier
}

type BlockRetireResult struct {
	BlockFrom, BlockTo uint64
	Err                error
}

func NewBlockRetire(workers int, tmpDir string, snapshots *RoSnapshots, db kv.RoDB, downloader proto_downloader.DownloaderClient, notifier DBEventNotifier) *BlockRetire {
	return &BlockRetire{workers: workers, tmpDir: tmpDir, snapshots: snapshots, wg: &sync.WaitGroup{}, db: db, downloader: downloader, notifier: notifier}
}
func (br *BlockRetire) Snapshots() *RoSnapshots { return br.snapshots }
func (br *BlockRetire) Working() bool           { return br.working.Load() }
func (br *BlockRetire) Wait()                   { br.wg.Wait() }
func (br *BlockRetire) Result() *BlockRetireResult {
	r := br.result
	br.result = nil
	return r
}
func CanRetire(curBlockNum uint64, snapshots *RoSnapshots) (blockFrom, blockTo uint64, can bool) {
	blockFrom = snapshots.BlocksAvailable() + 1
	return canRetire(blockFrom, curBlockNum-params.FullImmutabilityThreshold)
}
func canRetire(from, to uint64) (blockFrom, blockTo uint64, can bool) {
	blockFrom = (from / 1_000) * 1_000
	roundedTo1K := (to / 1_000) * 1_000
	var maxJump uint64 = 1_000
	if blockFrom%500_000 == 0 {
		maxJump = 500_000
	} else if blockFrom%100_000 == 0 {
		maxJump = 100_000
	} else if blockFrom%10_000 == 0 {
		maxJump = 10_000
	}
	//roundedTo1K := (to / 1_000) * 1_000
	jump := cmp.Min(maxJump, roundedTo1K-blockFrom)
	switch { // only next segment sizes are allowed
	case jump >= 500_000:
		blockTo = blockFrom + 500_000
	case jump >= 100_000:
		blockTo = blockFrom + 100_000
	case jump >= 10_000:
		blockTo = blockFrom + 10_000
	case jump >= 1_000:
		blockTo = blockFrom + 1_000
	default:
		blockTo = blockFrom
	}
	return blockFrom, blockTo, blockTo-blockFrom >= 1_000
}
func CanDeleteTo(curBlockNum uint64, snapshots *RoSnapshots) (blockTo uint64) {
	if curBlockNum+999 < params.FullImmutabilityThreshold {
		// To prevent overflow of uint64 below
		return snapshots.BlocksAvailable() + 1
	}
	hardLimit := (curBlockNum/1_000)*1_000 - params.FullImmutabilityThreshold
	return cmp.Min(hardLimit, snapshots.BlocksAvailable()+1)
}
func (br *BlockRetire) RetireBlocks(ctx context.Context, blockFrom, blockTo uint64, lvl log.Lvl) error {
	chainConfig := tool.ChainConfigFromDB(br.db)
	chainID, _ := uint256.FromBig(chainConfig.ChainID)
	return retireBlocks(ctx, blockFrom, blockTo, *chainID, br.tmpDir, br.snapshots, br.db, br.workers, br.downloader, lvl, br.notifier)
}

func (br *BlockRetire) PruneAncientBlocks(tx kv.RwTx) error {
	if !br.snapshots.cfg.KeepBlocks {
		return nil
	}
	currentProgress, err := stages.GetStageProgress(tx, stages.Senders)
	if err != nil {
		return err
	}
	canDeleteTo := CanDeleteTo(currentProgress, br.snapshots)
	if _, _, err := rawdb.DeleteAncientBlocks(tx, canDeleteTo, 100); err != nil {
		return nil
	}
	if err := rawdb.PruneTable(tx, kv.Senders, canDeleteTo, context.Background(), 100); err != nil {
		return err
	}
	return nil
}

func (br *BlockRetire) RetireBlocksInBackground(ctx context.Context, forwardProgress uint64, lvl log.Lvl) {
	if br.working.Load() {
		// go-routine is still working
		return
	}
	if br.result != nil {
		// Prevent invocation for the same range twice, result needs to be cleared in the Result() function
		return
	}

	br.wg.Add(1)
	go func() {
		br.working.Store(true)
		defer br.working.Store(false)
		defer br.wg.Done()

		blockFrom, blockTo, ok := CanRetire(forwardProgress, br.Snapshots())
		if !ok {
			return
		}

		err := br.RetireBlocks(ctx, blockFrom, blockTo, lvl)
		br.result = &BlockRetireResult{
			BlockFrom: blockFrom,
			BlockTo:   blockTo,
			Err:       err,
		}
	}()
}

type DBEventNotifier interface {
	OnNewSnapshot()
}

func retireBlocks(ctx context.Context, blockFrom, blockTo uint64, chainID uint256.Int, tmpDir string, snapshots *RoSnapshots, db kv.RoDB, workers int, downloader proto_downloader.DownloaderClient, lvl log.Lvl, notifier DBEventNotifier) error {
	log.Log(lvl, "[snapshots] Retire Blocks", "range", fmt.Sprintf("%dk-%dk", blockFrom/1000, blockTo/1000))
	// in future we will do it in background
	if err := DumpBlocks(ctx, blockFrom, blockTo, snap.DEFAULT_SEGMENT_SIZE, tmpDir, snapshots.Dir(), db, workers, lvl); err != nil {
		return fmt.Errorf("DumpBlocks: %w", err)
	}
	if err := snapshots.ReopenFolder(); err != nil {
		return fmt.Errorf("reopen: %w", err)
	}
	snapshots.LogStat()
	if notifier != nil { // notify about new snapshots of any size
		notifier.OnNewSnapshot()
	}
	merger := NewMerger(tmpDir, workers, lvl, chainID, notifier)
	rangesToMerge := merger.FindMergeRanges(snapshots.Ranges())
	if len(rangesToMerge) == 0 {
		return nil
	}
	err := merger.Merge(ctx, snapshots, rangesToMerge, snapshots.Dir(), true)
	if err != nil {
		return err
	}
	if err := snapshots.ReopenFolder(); err != nil {
		return fmt.Errorf("reopen: %w", err)
	}
	snapshots.LogStat()
	if notifier != nil { // notify about new snapshots of any size
		notifier.OnNewSnapshot()
	}

	var downloadRequest []DownloadRequest
	for _, r := range rangesToMerge {
		downloadRequest = append(downloadRequest, NewDownloadRequest(&r, "", ""))
	}

	return RequestSnapshotsDownload(ctx, downloadRequest, downloader)
}

func DumpBlocks(ctx context.Context, blockFrom, blockTo, blocksPerFile uint64, tmpDir, snapDir string, chainDB kv.RoDB, workers int, lvl log.Lvl) error {
	if blocksPerFile == 0 {
		return nil
	}
	chainConfig := tool.ChainConfigFromDB(chainDB)
	chainID, _ := uint256.FromBig(chainConfig.ChainID)
	for i := blockFrom; i < blockTo; i = chooseSegmentEnd(i, blockTo, blocksPerFile) {
		if err := dumpBlocksRange(ctx, i, chooseSegmentEnd(i, blockTo, blocksPerFile), tmpDir, snapDir, chainDB, *chainID, workers, lvl); err != nil {
			return err
		}
	}
	return nil
}

func dumpBlocksRange(ctx context.Context, blockFrom, blockTo uint64, tmpDir, snapDir string, chainDB kv.RoDB, chainID uint256.Int, workers int, lvl log.Lvl) error {
	f, _ := snap.ParseFileName(snapDir, snap.SegmentFileName(blockFrom, blockTo, snap.Headers))
	if err := DumpHeaders(ctx, chainDB, f.Path, tmpDir, blockFrom, blockTo, workers, lvl); err != nil {
		return fmt.Errorf("DumpHeaders: %w", err)
	}
	if err := buildIdx(ctx, f, chainID, tmpDir, lvl); err != nil {
		return err
	}

	f, _ = snap.ParseFileName(snapDir, snap.SegmentFileName(blockFrom, blockTo, snap.Bodies))
	if err := DumpBodies(ctx, chainDB, f.Path, tmpDir, blockFrom, blockTo, workers, lvl); err != nil {
		return fmt.Errorf("DumpBodies: %w", err)
	}
	if err := buildIdx(ctx, f, chainID, tmpDir, lvl); err != nil {
		return err
	}

	f, _ = snap.ParseFileName(snapDir, snap.SegmentFileName(blockFrom, blockTo, snap.Transactions))
	if _, err := DumpTxs(ctx, chainDB, f.Path, tmpDir, blockFrom, blockTo, workers, lvl); err != nil {
		return fmt.Errorf("DumpTxs: %w", err)
	}
	if err := buildIdx(ctx, f, chainID, tmpDir, lvl); err != nil {
		return err
	}

	return nil
}

func hasIdxFile(sn *snap.FileInfo) bool {
	dir, _ := filepath.Split(sn.Path)
	fName := snap.IdxFileName(sn.From, sn.To, sn.T.String())
	switch sn.T {
	case snap.Headers:
		idx, err := recsplit.OpenIndex(path.Join(dir, fName))
		if err != nil {
			return false
		}
		_ = idx.Close()
	case snap.Bodies:
		idx, err := recsplit.OpenIndex(path.Join(dir, fName))
		if err != nil {
			return false
		}
		_ = idx.Close()
	case snap.Transactions:
		idx, err := recsplit.OpenIndex(path.Join(dir, fName))
		if err != nil {
			return false
		}
		_ = idx.Close()

		fName = snap.IdxFileName(sn.From, sn.To, snap.Transactions2Block.String())
		idx, err = recsplit.OpenIndex(path.Join(dir, fName))
		if err != nil {
			return false
		}
		_ = idx.Close()
	}
	return true
}

// DumpTxs - [from, to)
// Format: hash[0]_1byte + sender_address_2bytes + txnRlp
func DumpTxs(ctx context.Context, db kv.RoDB, segmentFile, tmpDir string, blockFrom, blockTo uint64, workers int, lvl log.Lvl) (firstTxID uint64, err error) {
	logEvery := time.NewTicker(20 * time.Second)
	defer logEvery.Stop()
	warmupCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	chainConfig := tool.ChainConfigFromDB(db)
	chainID, _ := uint256.FromBig(chainConfig.ChainID)

	f, err := compress.NewCompressor(ctx, "Snapshot Txs", segmentFile, tmpDir, compress.MinPatternScore, workers, lvl)
	if err != nil {
		return 0, fmt.Errorf("NewCompressor: %w, %s", err, segmentFile)
	}
	defer f.Close()

	var prevTxID uint64
	numBuf := make([]byte, binary.MaxVarintLen64)
	parseCtx := types2.NewTxParseContext(*chainID)
	parseCtx.WithSender(false)
	slot := types2.TxSlot{}
	var sender [20]byte
	parse := func(v, valueBuf []byte, senders []common.Address, j int) ([]byte, error) {
		if _, err := parseCtx.ParseTransaction(v, 0, &slot, sender[:], false /* hasEnvelope */, nil); err != nil {
			return valueBuf, err
		}
		if len(senders) > 0 {
			sender = senders[j]
		}

		valueBuf = valueBuf[:0]
		valueBuf = append(valueBuf, slot.IDHash[:1]...)
		valueBuf = append(valueBuf, sender[:]...)
		valueBuf = append(valueBuf, v...)
		return valueBuf, nil
	}
	valueBuf := make([]byte, 16*4096)
	addSystemTx := func(tx kv.Tx, txId uint64) error {
		binary.BigEndian.PutUint64(numBuf, txId)
		tv, err := tx.GetOne(kv.EthTx, numBuf[:8])
		if err != nil {
			return err
		}
		if tv == nil {
			if err := f.AddWord(nil); err != nil {
				return fmt.Errorf("AddWord1: %d", err)
			}
			return nil
		}

		parseCtx.WithSender(false)
		valueBuf, err = parse(tv, valueBuf, nil, 0)
		if err != nil {
			return err
		}
		if err := f.AddWord(valueBuf); err != nil {
			return fmt.Errorf("AddWord2: %d", err)
		}
		return nil
	}

	firstIDSaved := false

	doWarmup, warmupTxs, warmupSenders := blockTo-blockFrom >= 100_000 && workers > 4, atomic.NewBool(false), atomic.NewBool(false)
	from := dbutils.EncodeBlockNumber(blockFrom)
	var lastBody types.BodyForStorage
	if err := kv.BigChunks(db, kv.HeaderCanonical, from, func(tx kv.Tx, k, v []byte) (bool, error) {
		blockNum := binary.BigEndian.Uint64(k)
		if blockNum >= blockTo { // [from, to)
			return false, nil
		}

		h := common.BytesToHash(v)
		dataRLP := rawdb.ReadStorageBodyRLP(tx, h, blockNum)
		if dataRLP == nil {
			return false, fmt.Errorf("body not found: %d, %x", blockNum, h)
		}
		var body types.BodyForStorage
		if e := rlp.DecodeBytes(dataRLP, &body); e != nil {
			return false, e
		}
		lastBody = body
		if body.TxAmount == 0 {
			return true, nil
		}
		if doWarmup && !warmupSenders.Load() && blockNum%1_000 == 0 {
			kv.ReadAhead(warmupCtx, db, warmupSenders, kv.Senders, dbutils.EncodeBlockNumber(blockNum), 10_000)
		}
		if doWarmup && !warmupTxs.Load() && blockNum%1_000 == 0 {
			kv.ReadAhead(warmupCtx, db, warmupTxs, kv.EthTx, dbutils.EncodeBlockNumber(body.BaseTxId), 100*10_000)
		}
		senders, err := rawdb.ReadSenders(tx, h, blockNum)
		if err != nil {
			return false, err
		}

		if !firstIDSaved {
			firstIDSaved = true
			firstTxID = body.BaseTxId
		}
		j := 0

		if err := addSystemTx(tx, body.BaseTxId); err != nil {
			return false, err
		}
		if prevTxID > 0 {
			prevTxID++
		} else {
			prevTxID = body.BaseTxId
		}
		binary.BigEndian.PutUint64(numBuf, body.BaseTxId+1)
		if err := tx.ForAmount(kv.EthTx, numBuf[:8], body.TxAmount-2, func(tk, tv []byte) error {
			id := binary.BigEndian.Uint64(tk)
			if prevTxID != 0 && id != prevTxID+1 {
				panic(fmt.Sprintf("no gaps in tx ids are allowed: block %d does jump from %d to %d", blockNum, prevTxID, id))
			}
			prevTxID = id
			parseCtx.WithSender(len(senders) == 0)
			valueBuf, err = parse(tv, valueBuf, senders, j)
			if err != nil {
				return fmt.Errorf("%w, block: %d", err, blockNum)
			}
			if err := f.AddWord(valueBuf); err != nil {
				return err
			}
			j++

			return nil
		}); err != nil {
			return false, fmt.Errorf("ForAmount: %w", err)
		}

		if err := addSystemTx(tx, body.BaseTxId+uint64(body.TxAmount)-1); err != nil {
			return false, err
		}
		prevTxID++

		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-logEvery.C:
			var m runtime.MemStats
			if lvl >= log.LvlInfo {
				common2.ReadMemStats(&m)
			}
			log.Log(lvl, "[snapshots] Dumping txs", "block num", blockNum,
				"alloc", common2.ByteCount(m.Alloc), "sys", common2.ByteCount(m.Sys),
			)
		default:
		}
		return true, nil
	}); err != nil {
		return 0, fmt.Errorf("BigChunks: %w", err)
	}

	expectedCount := lastBody.BaseTxId + uint64(lastBody.TxAmount) - firstTxID
	if expectedCount != uint64(f.Count()) {
		return 0, fmt.Errorf("incorrect tx count: %d, expected from db: %d", f.Count(), expectedCount)
	}
	snapDir, _ := filepath.Split(segmentFile)
	_, expectedCount, err = expectedTxsAmount(snapDir, blockFrom, blockTo)
	if err != nil {
		return 0, err
	}
	if expectedCount != uint64(f.Count()) {
		return 0, fmt.Errorf("incorrect tx count: %d, expected from snapshots: %d", f.Count(), expectedCount)
	}

	if err := f.Compress(); err != nil {
		return 0, fmt.Errorf("compress: %w", err)
	}

	_, fileName := filepath.Split(segmentFile)
	ext := filepath.Ext(fileName)
	log.Log(lvl, "[snapshots] Compression", "ratio", f.Ratio.String(), "file", fileName[:len(fileName)-len(ext)])

	return firstTxID, nil
}

// DumpHeaders - [from, to)
func DumpHeaders(ctx context.Context, db kv.RoDB, segmentFilePath, tmpDir string, blockFrom, blockTo uint64, workers int, lvl log.Lvl) error {
	logEvery := time.NewTicker(20 * time.Second)
	defer logEvery.Stop()

	f, err := compress.NewCompressor(ctx, "Snapshot Headers", segmentFilePath, tmpDir, compress.MinPatternScore, workers, lvl)
	if err != nil {
		return err
	}
	defer f.Close()

	key := make([]byte, 8+32)
	from := dbutils.EncodeBlockNumber(blockFrom)
	if err := kv.BigChunks(db, kv.HeaderCanonical, from, func(tx kv.Tx, k, v []byte) (bool, error) {
		blockNum := binary.BigEndian.Uint64(k)
		if blockNum >= blockTo {
			return false, nil
		}
		copy(key, k)
		copy(key[8:], v)
		dataRLP, err := tx.GetOne(kv.Headers, key)
		if err != nil {
			return false, err
		}
		if dataRLP == nil {
			return false, fmt.Errorf("header missed in db: block_num=%d,  hash=%x", blockNum, v)
		}
		h := types.Header{}
		if err := rlp.DecodeBytes(dataRLP, &h); err != nil {
			return false, err
		}

		value := make([]byte, len(dataRLP)+1) // first_byte_of_header_hash + header_rlp
		value[0] = h.Hash()[0]
		copy(value[1:], dataRLP)
		if err := f.AddWord(value); err != nil {
			return false, err
		}

		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-logEvery.C:
			var m runtime.MemStats
			if lvl >= log.LvlInfo {
				common2.ReadMemStats(&m)
			}
			log.Log(lvl, "[snapshots] Dumping headers", "block num", blockNum,
				"alloc", common2.ByteCount(m.Alloc), "sys", common2.ByteCount(m.Sys),
			)
		default:
		}
		return true, nil
	}); err != nil {
		return err
	}
	if err := f.Compress(); err != nil {
		return fmt.Errorf("compress: %w", err)
	}
	return nil
}

// DumpBodies - [from, to)
func DumpBodies(ctx context.Context, db kv.RoDB, segmentFilePath, tmpDir string, blockFrom, blockTo uint64, workers int, lvl log.Lvl) error {
	logEvery := time.NewTicker(20 * time.Second)
	defer logEvery.Stop()

	f, err := compress.NewCompressor(ctx, "Snapshot Bodies", segmentFilePath, tmpDir, compress.MinPatternScore, workers, lvl)
	if err != nil {
		return err
	}
	defer f.Close()

	key := make([]byte, 8+32)
	from := dbutils.EncodeBlockNumber(blockFrom)
	if err := kv.BigChunks(db, kv.HeaderCanonical, from, func(tx kv.Tx, k, v []byte) (bool, error) {
		blockNum := binary.BigEndian.Uint64(k)
		if blockNum >= blockTo {
			return false, nil
		}
		copy(key, k)
		copy(key[8:], v)
		dataRLP, err := tx.GetOne(kv.BlockBody, key)
		if err != nil {
			return false, err
		}
		if dataRLP == nil {
			log.Warn("header missed", "block_num", blockNum, "hash", fmt.Sprintf("%x", v))
			return true, nil
		}

		if err := f.AddWord(dataRLP); err != nil {
			return false, err
		}

		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-logEvery.C:
			var m runtime.MemStats
			if lvl >= log.LvlInfo {
				common2.ReadMemStats(&m)
			}
			log.Log(lvl, "[snapshots] Wrote into file", "block num", blockNum,
				"alloc", common2.ByteCount(m.Alloc), "sys", common2.ByteCount(m.Sys),
			)
		default:
		}
		return true, nil
	}); err != nil {
		return err
	}
	if err := f.Compress(); err != nil {
		return fmt.Errorf("compress: %w", err)
	}

	return nil
}

var EmptyTxHash = common.Hash{}

func expectedTxsAmount(snapDir string, blockFrom, blockTo uint64) (firstTxID, expectedCount uint64, err error) {
	bodySegmentPath := filepath.Join(snapDir, snap.SegmentFileName(blockFrom, blockTo, snap.Bodies))
	bodiesSegment, err := compress.NewDecompressor(bodySegmentPath)
	if err != nil {
		return
	}
	defer bodiesSegment.Close()

	gg := bodiesSegment.MakeGetter()
	buf, _ := gg.Next(nil)
	firstBody := &types.BodyForStorage{}
	if err = rlp.DecodeBytes(buf, firstBody); err != nil {
		return
	}
	firstTxID = firstBody.BaseTxId

	lastBody := new(types.BodyForStorage)
	i := uint64(0)
	for gg.HasNext() {
		i++
		if i == blockTo-blockFrom-1 {
			buf, _ = gg.Next(buf[:0])
			if err = rlp.DecodeBytes(buf, lastBody); err != nil {
				return
			}
			if gg.HasNext() {
				panic(1)
			}
		} else {
			gg.Skip()
		}
	}

	expectedCount = lastBody.BaseTxId + uint64(lastBody.TxAmount) - firstBody.BaseTxId
	return
}

func TransactionsIdx(ctx context.Context, chainID uint256.Int, blockFrom, blockTo uint64, snapDir string, tmpDir string, lvl log.Lvl) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("TransactionsIdx: at=%d-%d, %v, %s", blockFrom, blockTo, rec, dbg.Stack())
		}
	}()
	firstBlockNum := blockFrom
	firstTxID, expectedCount, err := expectedTxsAmount(snapDir, blockFrom, blockTo)
	if err != nil {
		return err
	}
	bodySegmentPath := filepath.Join(snapDir, snap.SegmentFileName(blockFrom, blockTo, snap.Bodies))
	bodiesSegment, err := compress.NewDecompressor(bodySegmentPath)
	if err != nil {
		return
	}
	defer bodiesSegment.Close()

	segmentFilePath := filepath.Join(snapDir, snap.SegmentFileName(blockFrom, blockTo, snap.Transactions))
	d, err := compress.NewDecompressor(segmentFilePath)
	if err != nil {
		return err
	}
	defer d.Close()
	if uint64(d.Count()) != expectedCount {
		panic(fmt.Errorf("expect: %d, got %d\n", expectedCount, d.Count()))
	}

	txnHashIdx, err := recsplit.NewRecSplit(recsplit.RecSplitArgs{
		KeyCount:    d.Count(),
		Enums:       true,
		BucketSize:  2000,
		LeafSize:    8,
		TmpDir:      tmpDir,
		IndexFile:   filepath.Join(snapDir, snap.IdxFileName(blockFrom, blockTo, snap.Transactions.String())),
		BaseDataID:  firstTxID,
		EtlBufLimit: etl.BufferOptimalSize / 2,
	})
	if err != nil {
		return err
	}
	txnHash2BlockNumIdx, err := recsplit.NewRecSplit(recsplit.RecSplitArgs{
		KeyCount:    d.Count(),
		Enums:       false,
		BucketSize:  2000,
		LeafSize:    8,
		TmpDir:      tmpDir,
		IndexFile:   filepath.Join(snapDir, snap.IdxFileName(blockFrom, blockTo, snap.Transactions2Block.String())),
		BaseDataID:  firstBlockNum,
		EtlBufLimit: etl.BufferOptimalSize / 2,
	})
	if err != nil {
		return err
	}
	txnHashIdx.LogLvl(log.LvlDebug)
	txnHash2BlockNumIdx.LogLvl(log.LvlDebug)

	parseCtx := types2.NewTxParseContext(chainID)
	parseCtx.WithSender(false)
	slot := types2.TxSlot{}
	bodyBuf, word := make([]byte, 0, 4096), make([]byte, 0, 4096)

	withReadAhead := func(f func(g, bodyGetter *compress.Getter) error) error {
		return d.WithReadAhead(func() error {
			return bodiesSegment.WithReadAhead(func() error {
				return f(d.MakeGetter(), bodiesSegment.MakeGetter())
			})
		})
	}

RETRY:
	if err := withReadAhead(func(g, bodyGetter *compress.Getter) error {
		var i, offset, nextPos uint64
		blockNum := firstBlockNum
		body := &types.BodyForStorage{}

		bodyBuf, _ = bodyGetter.Next(bodyBuf[:0])
		if err := rlp.DecodeBytes(bodyBuf, body); err != nil {
			return err
		}

		for g.HasNext() {
			word, nextPos = g.Next(word[:0])
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}

			for body.BaseTxId+uint64(body.TxAmount) <= firstTxID+i { // skip empty blocks
				if !bodyGetter.HasNext() {
					return fmt.Errorf("not enough bodies")
				}
				bodyBuf, _ = bodyGetter.Next(bodyBuf[:0])
				if err := rlp.DecodeBytes(bodyBuf, body); err != nil {
					return err
				}
				blockNum++
			}

			isSystemTx := len(word) == 0
			if isSystemTx { // system-txs hash:pad32(txnID)
				binary.BigEndian.PutUint64(slot.IDHash[:], firstTxID+i)
			} else {
				if _, err := parseCtx.ParseTransaction(word[1+20:], 0, &slot, nil, true /* hasEnvelope */, nil); err != nil {
					return fmt.Errorf("ParseTransaction: %w, blockNum: %d, i: %d", err, blockNum, i)
				}
			}

			if err := txnHashIdx.AddKey(slot.IDHash[:], offset); err != nil {
				return err
			}
			if err := txnHash2BlockNumIdx.AddKey(slot.IDHash[:], blockNum); err != nil {
				return err
			}

			i++
			offset = nextPos
		}

		if i != expectedCount {
			panic(fmt.Errorf("expect: %d, got %d\n", expectedCount, i))
		}

		if err := txnHashIdx.Build(); err != nil {
			return fmt.Errorf("txnHashIdx: %w", err)
		}
		if err := txnHash2BlockNumIdx.Build(); err != nil {
			return fmt.Errorf("txnHash2BlockNumIdx: %w", err)
		}

		return nil
	}); err != nil {
		if errors.Is(err, recsplit.ErrCollision) {
			log.Warn("Building recsplit. Collision happened. It's ok. Restarting with another salt...", "err", err)
			txnHashIdx.ResetNextSalt()
			txnHash2BlockNumIdx.ResetNextSalt()
			goto RETRY
		}
		return err
	}

	return nil
}

// HeadersIdx - headerHash -> offset (analog of kv.HeaderNumber)
func HeadersIdx(ctx context.Context, segmentFilePath string, firstBlockNumInSegment uint64, tmpDir string, lvl log.Lvl) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			_, fName := filepath.Split(segmentFilePath)
			err = fmt.Errorf("HeadersIdx: at=%s, %v, %s", fName, rec, dbg.Stack())
		}
	}()

	d, err := compress.NewDecompressor(segmentFilePath)
	if err != nil {
		return err
	}
	defer d.Close()

	hasher := crypto.NewKeccakState()
	var h common.Hash
	if err := Idx(ctx, d, firstBlockNumInSegment, tmpDir, log.LvlDebug, func(idx *recsplit.RecSplit, i, offset uint64, word []byte) error {
		headerRlp := word[1:]
		hasher.Reset()
		hasher.Write(headerRlp)
		hasher.Read(h[:])
		if err := idx.AddKey(h[:], offset); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return fmt.Errorf("HeadersIdx: %w", err)
	}
	return nil
}

func BodiesIdx(ctx context.Context, segmentFilePath string, firstBlockNumInSegment uint64, tmpDir string, lvl log.Lvl) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			_, fName := filepath.Split(segmentFilePath)
			err = fmt.Errorf("BodiesIdx: at=%s, %v, %s", fName, rec, dbg.Stack())
		}
	}()

	num := make([]byte, 8)

	d, err := compress.NewDecompressor(segmentFilePath)
	if err != nil {
		return err
	}
	defer d.Close()

	if err := Idx(ctx, d, firstBlockNumInSegment, tmpDir, log.LvlDebug, func(idx *recsplit.RecSplit, i, offset uint64, word []byte) error {
		n := binary.PutUvarint(num, i)
		if err := idx.AddKey(num[:n], offset); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return fmt.Errorf("BodyNumberIdx: %w", err)
	}
	return nil
}

type decompressItem struct {
	i, offset uint64
	word      []byte
	err       error
}

func forEachAsync(ctx context.Context, d *compress.Decompressor) chan decompressItem {
	ch := make(chan decompressItem, 1024)
	go func() {
		defer close(ch)
		if err := d.WithReadAhead(func() error {
			g := d.MakeGetter()
			var wc, pos, nextPos uint64
			word := make([]byte, 0, 4096)
			for g.HasNext() {
				word, nextPos = g.Next(word[:0])
				select {
				case <-ctx.Done():
					return nil
				case ch <- decompressItem{i: wc, offset: pos, word: common2.Copy(word)}:
				}
				wc++
				pos = nextPos
			}
			return nil
		}); err != nil {
			ch <- decompressItem{err: err}
		}
	}()
	return ch
}

// Idx - iterate over segment and building .idx file
func Idx(ctx context.Context, d *compress.Decompressor, firstDataID uint64, tmpDir string, lvl log.Lvl, walker func(idx *recsplit.RecSplit, i, offset uint64, word []byte) error) error {
	segmentFileName := d.FilePath()
	var extension = filepath.Ext(segmentFileName)
	var idxFilePath = segmentFileName[0:len(segmentFileName)-len(extension)] + ".idx"

	rs, err := recsplit.NewRecSplit(recsplit.RecSplitArgs{
		KeyCount:   d.Count(),
		Enums:      true,
		BucketSize: 2000,
		LeafSize:   8,
		TmpDir:     tmpDir,
		IndexFile:  idxFilePath,
		BaseDataID: firstDataID,
	})
	if err != nil {
		return err
	}
	rs.LogLvl(lvl)

RETRY:
	if err := d.WithReadAhead(func() error {
		g := d.MakeGetter()
		var i, offset, nextPos uint64
		word := make([]byte, 0, 4096)
		for g.HasNext() {
			word, nextPos = g.Next(word[:0])
			if err := walker(rs, i, offset, word); err != nil {
				return err
			}
			i++
			offset = nextPos

			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
		return nil
	}); err != nil {
		return err
	}

	if err = rs.Build(); err != nil {
		if errors.Is(err, recsplit.ErrCollision) {
			log.Info("Building recsplit. Collision happened. It's ok. Restarting with another salt...", "err", err)
			rs.ResetNextSalt()
			goto RETRY
		}
		return err
	}
	return nil
}

func ForEachHeader(ctx context.Context, s *RoSnapshots, walker func(header *types.Header) error) error {
	r := bytes.NewReader(nil)
	err := s.Headers.View(func(snapshots []*HeaderSegment) error {
		for _, sn := range snapshots {
			ch := forEachAsync(ctx, sn.seg)
			for it := range ch {
				if it.err != nil {
					return nil
				}

				header := new(types.Header)
				r.Reset(it.word[1:])
				if err := rlp.Decode(r, header); err != nil {
					return err
				}
				if err := walker(header); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	return nil
}

type Merger struct {
	lvl      log.Lvl
	workers  int
	tmpDir   string
	chainID  uint256.Int
	notifier DBEventNotifier
}

func NewMerger(tmpDir string, workers int, lvl log.Lvl, chainID uint256.Int, notifier DBEventNotifier) *Merger {
	return &Merger{tmpDir: tmpDir, workers: workers, lvl: lvl, chainID: chainID, notifier: notifier}
}

type Range struct {
	from, to uint64
}

func (r Range) String() string { return fmt.Sprintf("%dk-%dk", r.from/1000, r.to/1000) }

func (*Merger) FindMergeRanges(currentRanges []Range) (toMerge []Range) {
	for i := len(currentRanges) - 1; i > 0; i-- {
		r := currentRanges[i]
		if r.to-r.from >= snap.DEFAULT_SEGMENT_SIZE { // is complete .seg
			continue
		}

		for _, span := range []uint64{500_000, 100_000, 10_000} {
			if r.to%span != 0 {
				continue
			}
			if r.to-r.from == span {
				break
			}
			aggFrom := r.to - span
			toMerge = append(toMerge, Range{from: aggFrom, to: r.to})
			for currentRanges[i].from > aggFrom {
				i--
			}
			break
		}
	}
	slices.SortFunc(toMerge, func(i, j Range) bool { return i.from < j.from })
	return toMerge
}

func (m *Merger) filesByRange(snapshots *RoSnapshots, from, to uint64) (map[snap.Type][]string, error) {
	toMerge := map[snap.Type][]string{}
	err := snapshots.Headers.View(func(hSegments []*HeaderSegment) error {
		return snapshots.Bodies.View(func(bSegments []*BodySegment) error {
			return snapshots.Txs.View(func(tSegments []*TxnSegment) error {
				for i, sn := range hSegments {
					if sn.ranges.from < from {
						continue
					}
					if sn.ranges.to > to {
						break
					}
					toMerge[snap.Headers] = append(toMerge[snap.Headers], hSegments[i].seg.FilePath())
					toMerge[snap.Bodies] = append(toMerge[snap.Bodies], bSegments[i].seg.FilePath())
					toMerge[snap.Transactions] = append(toMerge[snap.Transactions], tSegments[i].Seg.FilePath())
				}

				return nil
			})
		})
	})
	return toMerge, err
}

// Merge does merge segments in given ranges
func (m *Merger) Merge(ctx context.Context, snapshots *RoSnapshots, mergeRanges []Range, snapDir string, doIndex bool) error {
	if len(mergeRanges) == 0 {
		return nil
	}
	logEvery := time.NewTicker(30 * time.Second)
	defer logEvery.Stop()
	log.Log(m.lvl, "[snapshots] Merge segments", "ranges", fmt.Sprintf("%v", mergeRanges))
	for _, r := range mergeRanges {
		toMerge, err := m.filesByRange(snapshots, r.from, r.to)
		if err != nil {
			return err
		}
		for _, t := range snap.AllSnapshotTypes {
			f, _ := snap.ParseFileName(snapDir, snap.SegmentFileName(r.from, r.to, t))
			if err := m.merge(ctx, toMerge[t], f.Path, logEvery); err != nil {
				return fmt.Errorf("mergeByAppendSegments: %w", err)
			}
			if doIndex {
				if err := buildIdx(ctx, f, m.chainID, m.tmpDir, m.lvl); err != nil {
					return err
				}
			}
		}
		if err := snapshots.ReopenFolder(); err != nil {
			return fmt.Errorf("ReopenSegments: %w", err)
		}
		snapshots.LogStat()
		if m.notifier != nil { // notify about new snapshots of any size
			m.notifier.OnNewSnapshot()
			time.Sleep(1 * time.Second) // i working on blocking API - to ensure client does not use old snapsthos - and then delete them
		}
		for _, t := range snap.AllSnapshotTypes {
			m.removeOldFiles(toMerge[t], snapDir)
		}
	}
	log.Log(m.lvl, "[snapshots] Merge done", "from", mergeRanges[0].from)
	return nil
}

func (m *Merger) merge(ctx context.Context, toMerge []string, targetFile string, logEvery *time.Ticker) error {
	var word = make([]byte, 0, 4096)
	var expectedTotal int
	cList := make([]*compress.Decompressor, len(toMerge))
	for i, cFile := range toMerge {
		d, err := compress.NewDecompressor(cFile)
		if err != nil {
			return err
		}
		defer d.Close()
		cList[i] = d
		expectedTotal += d.Count()
	}

	f, err := compress.NewCompressor(ctx, "Snapshots merge", targetFile, m.tmpDir, compress.MinPatternScore, m.workers, m.lvl)
	if err != nil {
		return err
	}
	defer f.Close()

	for _, d := range cList {
		if err := d.WithReadAhead(func() error {
			g := d.MakeGetter()
			for g.HasNext() {
				word, _ = g.Next(word[:0])
				if err := f.AddWord(word); err != nil {
					return err
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-logEvery.C:
					_, fName := filepath.Split(targetFile)
					log.Info("[Snapshots] Merge", "progress", fmt.Sprintf("%.2f%%", 100*float64(f.Count())/float64(expectedTotal)), "to", fName)
				default:
				}
			}
			return nil
		}); err != nil {
			return err
		}
		d.Close()
	}
	if f.Count() != expectedTotal {
		return fmt.Errorf("unexpected amount after segments merge. got: %d, expected: %d\n", f.Count(), expectedTotal)
	}
	if err = f.Compress(); err != nil {
		return err
	}
	return nil
}

func (m *Merger) removeOldFiles(toDel []string, snapDir string) {
	for _, f := range toDel {
		_ = os.Remove(f)
		ext := filepath.Ext(f)
		withoutExt := f[:len(f)-len(ext)]
		_ = os.Remove(withoutExt + ".idx")
		isTxnType := strings.HasSuffix(withoutExt, snap.Transactions.String())
		if isTxnType {
			_ = os.Remove(withoutExt + "-to-block.idx")
		}
	}
	tmpFiles, err := snap.TmpFiles(snapDir)
	if err != nil {
		return
	}
	for _, f := range tmpFiles {
		_ = os.Remove(f)
	}
}

func NewDownloadRequest(ranges *Range, path string, torrentHash string) DownloadRequest {
	return DownloadRequest{
		ranges:      ranges,
		path:        path,
		torrentHash: torrentHash,
	}
}

// RequestSnapshotsDownload - builds the snapshots download request and downloads them
func RequestSnapshotsDownload(ctx context.Context, downloadRequest []DownloadRequest, downloader proto_downloader.DownloaderClient) error {
	// start seed large .seg of large size
	req := BuildProtoRequest(downloadRequest)
	if _, err := downloader.Download(ctx, req); err != nil {
		return err
	}
	return nil
}

func BuildProtoRequest(downloadRequest []DownloadRequest) *proto_downloader.DownloadRequest {
	req := &proto_downloader.DownloadRequest{Items: make([]*proto_downloader.DownloadItem, 0, len(snap.AllSnapshotTypes))}
	for _, r := range downloadRequest {
		if r.path != "" {
			if r.torrentHash != "" {
				req.Items = append(req.Items, &proto_downloader.DownloadItem{
					TorrentHash: downloadergrpc.String2Proto(r.torrentHash),
					Path:        r.path,
				})
			} else {
				req.Items = append(req.Items, &proto_downloader.DownloadItem{
					Path: r.path,
				})
			}
		} else {
			if r.ranges.to-r.ranges.from != snap.DEFAULT_SEGMENT_SIZE {
				continue
			}
			for _, t := range snap.AllSnapshotTypes {
				req.Items = append(req.Items, &proto_downloader.DownloadItem{
					Path: snap.SegmentFileName(r.ranges.from, r.ranges.to, t),
				})
			}
		}
	}
	return req
}
