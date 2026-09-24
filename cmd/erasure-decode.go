// Copyright (c) 2015-2021 MinIO, Inc.
//
// This file is part of MinIO Object Storage stack
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	xioutil "github.com/minio/minio/internal/ioutil"
)

// Reads in parallel from readers.
type parallelReader struct {
	readers       []io.ReaderAt
	orgReaders    []io.ReaderAt
	dataBlocks    int
	offset        int64
	shardSize     int64
	shardFileSize int64
	buf           [][]byte
	readerToBuf   []int
	stashBuffer   []byte

	// Hedged read state. When fanoutDelay is > 0 reads start with
	// dataBlocks+1 shards in parallel and fan out to all remaining
	// shards after fanoutDelay.
	fanoutDelay      time.Duration
	events           chan hedgedResult
	inflight         []bool
	curBlock         *hedgedBlock
	stateLK          sync.Mutex
	missingPartsHeal atomic.Bool
	bitrotHeal       atomic.Bool
	disksNotFound    atomic.Int32
}

// hedgedBlock tracks per block launch state of a hedged read.
// All fields are guarded by parallelReader.stateLK.
type hedgedBlock struct {
	launched    []bool
	outstanding int
}

// hedgedResult is the outcome of one launched shard read. blk
// identifies the block the read was launched for.
type hedgedResult struct {
	blk *hedgedBlock
	i   int
	buf []byte
	n   int
	err error
}

// newParallelReader returns parallelReader.
func newParallelReader(readers []io.ReaderAt, e Erasure, offset, totalLength int64, fanoutDelay time.Duration) *parallelReader {
	r2b := make([]int, len(readers))
	for i := range r2b {
		r2b[i] = i
	}
	bufs := make([][]byte, len(readers))
	shardSize := int(e.ShardSize())
	var b []byte

	// When hedging is enabled shard buffers are allocated per reader
	// on first use instead of seeding from the byte pool: straggler
	// goroutines may outlive the reader and keep writing into their
	// buffer after Done() has returned pool memory.
	if fanoutDelay <= 0 && globalBytePoolCap.Load().WidthCap() >= len(readers)*shardSize {
		// Fill buffers
		b = globalBytePoolCap.Load().Get()
		// Seed the buffers.
		for i := range bufs {
			bufs[i] = b[i*shardSize : (i+1)*shardSize]
		}
	}

	p := &parallelReader{
		readers:       readers,
		orgReaders:    readers,
		dataBlocks:    e.dataBlocks,
		offset:        (offset / e.blockSize) * e.ShardSize(),
		shardSize:     e.ShardSize(),
		shardFileSize: e.ShardFileSize(totalLength),
		buf:           make([][]byte, len(readers)),
		readerToBuf:   r2b,
		stashBuffer:   b,
	}
	if fanoutDelay > 0 {
		p.fanoutDelay = fanoutDelay
		p.events = make(chan hedgedResult, len(readers))
		p.inflight = make([]bool, len(readers))
	}
	return p
}

// Done will release any resources used by the parallelReader.
func (p *parallelReader) Done() {
	if p.stashBuffer != nil {
		globalBytePoolCap.Load().Put(p.stashBuffer)
		p.stashBuffer = nil
	}
	if p.inflight != nil {
		// Readers still in flight are stragglers of a returned block.
		// Nil them in orgReaders so closeBitrotReaders skips them; their
		// in-flight goroutine closes the reader after the read returns.
		p.stateLK.Lock()
		for i := range p.inflight {
			if p.inflight[i] {
				p.orgReaders[p.readerToBuf[i]] = nil
			}
		}
		p.stateLK.Unlock()
	}
}

// preferReaders can mark readers as preferred.
// These will be chosen before others.
func (p *parallelReader) preferReaders(prefer []bool) {
	if len(prefer) != len(p.orgReaders) {
		return
	}
	// Copy so we don't change our input.
	tmp := make([]io.ReaderAt, len(p.orgReaders))
	copy(tmp, p.orgReaders)
	p.readers = tmp
	// next is the next non-preferred index.
	next := 0
	for i, ok := range prefer {
		if !ok || p.readers[i] == nil {
			continue
		}
		if i == next {
			next++
			continue
		}
		// Move reader with index i to index next.
		// Do this by swapping next and i
		p.readers[next], p.readers[i] = p.readers[i], p.readers[next]
		p.readerToBuf[next] = i
		p.readerToBuf[i] = next
		next++
	}
}

// Returns if buf can be erasure decoded.
func (p *parallelReader) canDecode(buf [][]byte) bool {
	bufCount := 0
	for _, b := range buf {
		if len(b) > 0 {
			bufCount++
		}
	}
	return bufCount >= p.dataBlocks
}

// Read reads from readers in parallel. Returns p.dataBlocks number of bufs.
func (p *parallelReader) Read(dst [][]byte) ([][]byte, error) {
	newBuf := dst
	if len(dst) != len(p.readers) {
		newBuf = make([][]byte, len(p.readers))
	} else {
		for i := range newBuf {
			newBuf[i] = newBuf[i][:0]
		}
	}

	if p.offset+p.shardSize > p.shardFileSize {
		p.shardSize = p.shardFileSize - p.offset
	}
	if p.shardSize == 0 {
		return newBuf, nil
	}

	if p.fanoutDelay > 0 {
		return p.readHedged(newBuf)
	}

	var newBufLK sync.RWMutex

	readTriggerCh := make(chan bool, len(p.readers))
	defer xioutil.SafeClose(readTriggerCh) // close the channel upon return

	for i := 0; i < p.dataBlocks; i++ {
		// Setup read triggers for p.dataBlocks number of reads so that it reads in parallel.
		readTriggerCh <- true
	}

	disksNotFound := int32(0)
	bitrotHeal := int32(0)       // Atomic bool flag.
	missingPartsHeal := int32(0) // Atomic bool flag.
	readerIndex := 0
	var wg sync.WaitGroup
	// if readTrigger is true, it implies next disk.ReadAt() should be tried
	// if readTrigger is false, it implies previous disk.ReadAt() was successful and there is no need
	// to try reading the next disk.
	for readTrigger := range readTriggerCh {
		newBufLK.RLock()
		canDecode := p.canDecode(newBuf)
		newBufLK.RUnlock()
		if canDecode {
			break
		}
		if readerIndex == len(p.readers) {
			break
		}
		if !readTrigger {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rr := p.readers[i]
			if rr == nil {
				// Since reader is nil, trigger another read.
				readTriggerCh <- true
				return
			}
			bufIdx := p.readerToBuf[i]
			if p.buf[bufIdx] == nil {
				// Reading first time on this disk, hence the buffer needs to be allocated.
				// Subsequent reads will reuse this buffer.
				p.buf[bufIdx] = make([]byte, p.shardSize)
			}
			// For the last shard, the shardsize might be less than previous shard sizes.
			// Hence the following statement ensures that the buffer size is reset to the right size.
			p.buf[bufIdx] = p.buf[bufIdx][:p.shardSize]
			n, err := rr.ReadAt(p.buf[bufIdx], p.offset)
			if err != nil {
				switch {
				case errors.Is(err, errFileNotFound):
					atomic.StoreInt32(&missingPartsHeal, 1)
				case errors.Is(err, errFileCorrupt):
					atomic.StoreInt32(&bitrotHeal, 1)
				case errors.Is(err, errDiskNotFound):
					atomic.AddInt32(&disksNotFound, 1)
				}

				// This will be communicated upstream.
				p.orgReaders[bufIdx] = nil
				if br, ok := p.readers[i].(io.Closer); ok {
					br.Close()
				}
				p.readers[i] = nil

				// Since ReadAt returned error, trigger another read.
				readTriggerCh <- true
				return
			}
			newBufLK.Lock()
			newBuf[bufIdx] = p.buf[bufIdx][:n]
			newBufLK.Unlock()
			// Since ReadAt returned success, there is no need to trigger another read.
			readTriggerCh <- false
		}(readerIndex)
		readerIndex++
	}
	wg.Wait()
	if p.canDecode(newBuf) {
		p.offset += p.shardSize
		if missingPartsHeal == 1 {
			return newBuf, errFileNotFound
		} else if bitrotHeal == 1 {
			return newBuf, errFileCorrupt
		}
		return newBuf, nil
	}

	// If we cannot decode, just return read quorum error.
	return nil, fmt.Errorf("%w (offline-disks=%d/%d)", errErasureReadQuorum, disksNotFound, len(p.readers))
}

// readHedged implements the hedged read strategy: it launches
// dataBlocks+1 reads in parallel, substitutes a new shard on error,
// and fans out to all remaining shards after p.fanoutDelay. It
// returns as soon as dataBlocks shards have been delivered, without
// waiting for stragglers. A reader that completes after its block
// returned re-joins on the next block; a straggler still in flight is
// never launched twice, so stateful readers are safe.
func (p *parallelReader) readHedged(newBuf [][]byte) ([][]byte, error) {
	p.stateLK.Lock()

	// Drain results of stragglers from a previous block, their
	// readers are available again.
	for len(p.events) > 0 {
		<-p.events
	}

	blk := &hedgedBlock{launched: make([]bool, len(p.readers))}
	p.curBlock = blk

	// launchNext launches the next launchable reader for this block.
	// Must be called with p.stateLK held.
	launchNext := func() bool {
		for i := range p.readers {
			if p.readers[i] == nil || p.inflight[i] || blk.launched[i] {
				continue
			}
			blk.launched[i] = true
			p.inflight[i] = true
			blk.outstanding++
			bufIdx := p.readerToBuf[i]
			if p.buf[bufIdx] == nil {
				p.buf[bufIdx] = make([]byte, p.shardSize)
			}
			buf := p.buf[bufIdx][:p.shardSize]
			offset := p.offset
			rr := p.readers[i]
			go func(i int, bufIdx int, rr io.ReaderAt, buf []byte, offset int64) {
				n, err := rr.ReadAt(buf, offset)
				p.stateLK.Lock()
				p.inflight[i] = false
				closeReader := false
				if p.orgReaders[bufIdx] == nil {
					// Abandoned straggler, marked by Done(); the
					// caller no longer tracks this reader so it is
					// closed here instead of by closeBitrotReaders.
					closeReader = true
				} else if err != nil {
					switch {
					case errors.Is(err, errFileNotFound):
						p.missingPartsHeal.Store(true)
					case errors.Is(err, errFileCorrupt):
						p.bitrotHeal.Store(true)
					case errors.Is(err, errDiskNotFound):
						p.disksNotFound.Add(1)
					}
					p.readers[i] = nil
					// This will be communicated upstream.
					p.orgReaders[bufIdx] = nil
					closeReader = true
				}
				p.stateLK.Unlock()
				if closeReader {
					if closer, ok := rr.(io.Closer); ok {
						closer.Close()
					}
				}
				p.events <- hedgedResult{blk: blk, i: i, buf: buf, n: n, err: err}
			}(i, bufIdx, rr, buf, offset)
			return true
		}
		return false
	}

	for n := 0; n < p.dataBlocks+1; n++ {
		// Launch dataBlocks+1 reads so that a single slow or
		// missing shard is covered without waiting for the fan-out.
		if !launchNext() {
			break
		}
	}

	fanoutPossible := false
	for i := range p.readers {
		if p.readers[i] != nil && !p.inflight[i] && !blk.launched[i] {
			fanoutPossible = true
			break
		}
	}
	var timer *time.Timer
	if fanoutPossible {
		timer = time.AfterFunc(p.fanoutDelay, func() {
			p.stateLK.Lock()
			defer p.stateLK.Unlock()
			if p.curBlock != blk {
				return
			}
			for launchNext() {
			}
		})
	}
	p.stateLK.Unlock()

	defer func() {
		if timer != nil {
			timer.Stop()
		}
		p.stateLK.Lock()
		if p.curBlock == blk {
			p.curBlock = nil
		}
		p.stateLK.Unlock()
	}()

	bufCount := 0
	for bufCount < p.dataBlocks {
		p.stateLK.Lock()
		if blk.outstanding == 0 {
			launched := launchNext()
			waiting := false
			if !launched {
				for i := range p.inflight {
					if p.inflight[i] {
						waiting = true
						break
					}
				}
			}
			p.stateLK.Unlock()
			switch {
			case launched:
				continue
			case waiting:
				// All launchable readers are exhausted but stragglers
				// of earlier blocks may still deliver.
			default:
				// If we cannot decode, just return read quorum error.
				return nil, fmt.Errorf("%w (offline-disks=%d/%d)", errErasureReadQuorum, p.disksNotFound.Load(), len(p.readers))
			}
		} else {
			p.stateLK.Unlock()
		}

		ev := <-p.events
		p.stateLK.Lock()
		if ev.blk == blk {
			blk.outstanding--
		}
		switch {
		case ev.blk == blk && ev.err == nil:
			p.stateLK.Unlock()
			newBuf[p.readerToBuf[ev.i]] = ev.buf[:ev.n]
			bufCount++
		case ev.blk == blk:
			// Since ReadAt returned error, launch a substitute shard.
			launchNext()
			p.stateLK.Unlock()
		default:
			// Stale result of an earlier block; its reader has been
			// marked available and can be relaunched on the next
			// iteration.
			p.stateLK.Unlock()
		}
	}

	p.stateLK.Lock()
	if p.curBlock == blk {
		p.curBlock = nil
	}
	p.stateLK.Unlock()

	p.offset += p.shardSize
	switch {
	case p.missingPartsHeal.Load():
		return newBuf, errFileNotFound
	case p.bitrotHeal.Load():
		return newBuf, errFileCorrupt
	}
	return newBuf, nil
}

// Decode reads from readers, reconstructs data if needed and writes the data to the writer.
// A set of preferred drives can be supplied. In that case they will be used and the data reconstructed.
func (e Erasure) Decode(ctx context.Context, writer io.Writer, readers []io.ReaderAt, offset, length, totalLength int64, prefer []bool) (written int64, derr error) {
	if offset < 0 || length < 0 {
		return -1, errInvalidArgument
	}
	if offset+length > totalLength {
		return -1, errInvalidArgument
	}

	if length == 0 {
		return 0, nil
	}

	reader := newParallelReader(readers, e, offset, totalLength, globalDriveConfig.GetReadFanoutDelay())
	if len(prefer) == len(readers) {
		reader.preferReaders(prefer)
	}
	defer reader.Done()

	startBlock := offset / e.blockSize
	endBlock := (offset + length) / e.blockSize

	var bytesWritten int64
	var bufs [][]byte
	for block := startBlock; block <= endBlock; block++ {
		var blockOffset, blockLength int64
		switch {
		case startBlock == endBlock:
			blockOffset = offset % e.blockSize
			blockLength = length
		case block == startBlock:
			blockOffset = offset % e.blockSize
			blockLength = e.blockSize - blockOffset
		case block == endBlock:
			blockOffset = 0
			blockLength = (offset + length) % e.blockSize
		default:
			blockOffset = 0
			blockLength = e.blockSize
		}
		if blockLength == 0 {
			break
		}

		var err error
		bufs, err = reader.Read(bufs)
		if len(bufs) > 0 {
			// Set only if there are be enough data for reconstruction.
			// and only for expected errors, also set once.
			if errors.Is(err, errFileNotFound) || errors.Is(err, errFileCorrupt) {
				if derr == nil {
					derr = err
				}
			}
		} else if err != nil {
			// For all errors that cannot be reconstructed fail the read operation.
			return -1, err
		}

		if err = e.DecodeDataBlocks(bufs); err != nil {
			return -1, err
		}

		n, err := writeDataBlocks(ctx, writer, bufs, e.dataBlocks, blockOffset, blockLength)
		if err != nil {
			return -1, err
		}

		bytesWritten += n
	}

	if bytesWritten != length {
		return bytesWritten, errLessData
	}

	return bytesWritten, derr
}

// Heal reads from readers, reconstruct shards and writes the data to the writers.
func (e Erasure) Heal(ctx context.Context, writers []io.Writer, readers []io.ReaderAt, totalLength int64, prefer []bool) (derr error) {
	if len(writers) != e.parityBlocks+e.dataBlocks {
		return errInvalidArgument
	}

	reader := newParallelReader(readers, e, 0, totalLength, 0)
	if len(readers) == len(prefer) {
		reader.preferReaders(prefer)
	}
	defer reader.Done()

	startBlock := int64(0)
	endBlock := totalLength / e.blockSize
	if totalLength%e.blockSize != 0 {
		endBlock++
	}

	var bufs [][]byte
	for block := startBlock; block < endBlock; block++ {
		var err error
		bufs, err = reader.Read(bufs)
		if len(bufs) > 0 {
			if errors.Is(err, errFileNotFound) || errors.Is(err, errFileCorrupt) {
				if derr == nil {
					derr = err
				}
			}
		} else if err != nil {
			return err
		}

		if err = e.DecodeDataAndParityBlocks(ctx, bufs); err != nil {
			return err
		}

		w := multiWriter{
			writers:     writers,
			writeQuorum: 1,
			errs:        make([]error, len(writers)),
		}

		if err = w.Write(ctx, bufs); err != nil {
			return err
		}
	}

	return derr
}
