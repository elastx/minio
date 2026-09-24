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
	"bytes"
	"context"
	crand "crypto/rand"
	"errors"
	"io"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/minio/minio/internal/bpool"
	"github.com/minio/minio/internal/config/drive"
)

func (a badDisk) ReadFile(ctx context.Context, volume string, path string, offset int64, buf []byte, verifier *BitrotVerifier) (n int64, err error) {
	return 0, errFaultyDisk
}

var erasureDecodeTests = []struct {
	dataBlocks                   int
	onDisks, offDisks            int
	blocksize, data              int64
	offset                       int64
	length                       int64
	algorithm                    BitrotAlgorithm
	shouldFail, shouldFailQuorum bool
}{
	{dataBlocks: 2, onDisks: 4, offDisks: 0, blocksize: int64(blockSizeV2), data: oneMiByte, offset: 0, length: oneMiByte, algorithm: BLAKE2b512, shouldFail: false, shouldFailQuorum: false},             // 0
	{dataBlocks: 3, onDisks: 6, offDisks: 0, blocksize: int64(blockSizeV2), data: oneMiByte, offset: 0, length: oneMiByte, algorithm: SHA256, shouldFail: false, shouldFailQuorum: false},                 // 1
	{dataBlocks: 4, onDisks: 8, offDisks: 0, blocksize: int64(blockSizeV2), data: oneMiByte, offset: 0, length: oneMiByte, algorithm: DefaultBitrotAlgorithm, shouldFail: false, shouldFailQuorum: false}, // 2
	{dataBlocks: 5, onDisks: 10, offDisks: 0, blocksize: int64(blockSizeV2), data: oneMiByte, offset: 1, length: oneMiByte - 1, algorithm: BLAKE2b512, shouldFail: false, shouldFailQuorum: false},        // 3
	{dataBlocks: 6, onDisks: 12, offDisks: 0, blocksize: int64(oneMiByte), data: oneMiByte, offset: oneMiByte, length: 0, algorithm: BLAKE2b512, shouldFail: false, shouldFailQuorum: false},
	// 4
	{dataBlocks: 7, onDisks: 14, offDisks: 0, blocksize: int64(oneMiByte), data: oneMiByte, offset: 3, length: 1024, algorithm: DefaultBitrotAlgorithm, shouldFail: false, shouldFailQuorum: false},                    // 5
	{dataBlocks: 8, onDisks: 16, offDisks: 0, blocksize: int64(oneMiByte), data: oneMiByte, offset: 4, length: 8 * 1024, algorithm: DefaultBitrotAlgorithm, shouldFail: false, shouldFailQuorum: false},                // 6
	{dataBlocks: 7, onDisks: 14, offDisks: 7, blocksize: int64(blockSizeV2), data: oneMiByte, offset: oneMiByte, length: 1, algorithm: DefaultBitrotAlgorithm, shouldFail: true, shouldFailQuorum: false},              // 7
	{dataBlocks: 6, onDisks: 12, offDisks: 6, blocksize: int64(blockSizeV2), data: oneMiByte, offset: 0, length: oneMiByte, algorithm: DefaultBitrotAlgorithm, shouldFail: false, shouldFailQuorum: false},             // 8
	{dataBlocks: 5, onDisks: 10, offDisks: 5, blocksize: int64(oneMiByte), data: oneMiByte, offset: 0, length: oneMiByte, algorithm: BLAKE2b512, shouldFail: false, shouldFailQuorum: false},                           // 9
	{dataBlocks: 4, onDisks: 8, offDisks: 4, blocksize: int64(blockSizeV2), data: oneMiByte, offset: 0, length: oneMiByte, algorithm: SHA256, shouldFail: false, shouldFailQuorum: false},                              // 10
	{dataBlocks: 3, onDisks: 6, offDisks: 3, blocksize: int64(oneMiByte), data: oneMiByte, offset: 0, length: oneMiByte, algorithm: DefaultBitrotAlgorithm, shouldFail: false, shouldFailQuorum: false},                // 11
	{dataBlocks: 2, onDisks: 4, offDisks: 2, blocksize: int64(blockSizeV2), data: oneMiByte, offset: 0, length: oneMiByte, algorithm: DefaultBitrotAlgorithm, shouldFail: false, shouldFailQuorum: false},              // 12
	{dataBlocks: 2, onDisks: 4, offDisks: 1, blocksize: int64(oneMiByte), data: oneMiByte, offset: 0, length: oneMiByte, algorithm: DefaultBitrotAlgorithm, shouldFail: false, shouldFailQuorum: false},                // 13
	{dataBlocks: 3, onDisks: 6, offDisks: 2, blocksize: int64(oneMiByte), data: oneMiByte, offset: 0, length: oneMiByte, algorithm: DefaultBitrotAlgorithm, shouldFail: false, shouldFailQuorum: false},                // 14
	{dataBlocks: 4, onDisks: 8, offDisks: 3, blocksize: int64(2 * oneMiByte), data: oneMiByte, offset: 0, length: oneMiByte, algorithm: DefaultBitrotAlgorithm, shouldFail: false, shouldFailQuorum: false},            // 15
	{dataBlocks: 5, onDisks: 10, offDisks: 6, blocksize: int64(oneMiByte), data: oneMiByte, offset: 0, length: oneMiByte, algorithm: DefaultBitrotAlgorithm, shouldFail: false, shouldFailQuorum: true},                // 16
	{dataBlocks: 5, onDisks: 10, offDisks: 2, blocksize: int64(blockSizeV2), data: 2 * oneMiByte, offset: oneMiByte, length: oneMiByte, algorithm: DefaultBitrotAlgorithm, shouldFail: false, shouldFailQuorum: false}, // 17
	{dataBlocks: 5, onDisks: 10, offDisks: 1, blocksize: int64(blockSizeV2), data: oneMiByte, offset: 0, length: oneMiByte, algorithm: BLAKE2b512, shouldFail: false, shouldFailQuorum: false},                         // 18
	{dataBlocks: 6, onDisks: 12, offDisks: 3, blocksize: int64(blockSizeV2), data: oneMiByte, offset: 0, length: oneMiByte, algorithm: SHA256, shouldFail: false, shouldFailQuorum: false},
	// 19
	{dataBlocks: 6, onDisks: 12, offDisks: 7, blocksize: int64(blockSizeV2), data: oneMiByte, offset: 0, length: oneMiByte, algorithm: DefaultBitrotAlgorithm, shouldFail: false, shouldFailQuorum: true},                                             // 20
	{dataBlocks: 8, onDisks: 16, offDisks: 8, blocksize: int64(blockSizeV2), data: oneMiByte, offset: 0, length: oneMiByte, algorithm: DefaultBitrotAlgorithm, shouldFail: false, shouldFailQuorum: false},                                            // 21
	{dataBlocks: 8, onDisks: 16, offDisks: 9, blocksize: int64(oneMiByte), data: oneMiByte, offset: 0, length: oneMiByte, algorithm: DefaultBitrotAlgorithm, shouldFail: false, shouldFailQuorum: true},                                               // 22
	{dataBlocks: 8, onDisks: 16, offDisks: 7, blocksize: int64(blockSizeV2), data: oneMiByte, offset: 0, length: oneMiByte, algorithm: DefaultBitrotAlgorithm, shouldFail: false, shouldFailQuorum: false},                                            // 23
	{dataBlocks: 2, onDisks: 4, offDisks: 1, blocksize: int64(blockSizeV2), data: oneMiByte, offset: 0, length: oneMiByte, algorithm: DefaultBitrotAlgorithm, shouldFail: false, shouldFailQuorum: false},                                             // 24
	{dataBlocks: 2, onDisks: 4, offDisks: 0, blocksize: int64(blockSizeV2), data: oneMiByte, offset: 0, length: oneMiByte, algorithm: DefaultBitrotAlgorithm, shouldFail: false, shouldFailQuorum: false},                                             // 25
	{dataBlocks: 2, onDisks: 4, offDisks: 0, blocksize: int64(blockSizeV2), data: int64(blockSizeV2) + 1, offset: 0, length: int64(blockSizeV2) + 1, algorithm: BLAKE2b512, shouldFail: false, shouldFailQuorum: false},                               // 26
	{dataBlocks: 2, onDisks: 4, offDisks: 0, blocksize: int64(blockSizeV2), data: int64(2 * blockSizeV2), offset: 12, length: int64(blockSizeV2) + 17, algorithm: BLAKE2b512, shouldFail: false, shouldFailQuorum: false},                             // 27
	{dataBlocks: 3, onDisks: 6, offDisks: 0, blocksize: int64(blockSizeV2), data: int64(2 * blockSizeV2), offset: 1023, length: int64(blockSizeV2) + 1024, algorithm: DefaultBitrotAlgorithm, shouldFail: false, shouldFailQuorum: false},             // 28
	{dataBlocks: 4, onDisks: 8, offDisks: 0, blocksize: int64(blockSizeV2), data: int64(2 * blockSizeV2), offset: 11, length: int64(blockSizeV2) + 2*1024, algorithm: DefaultBitrotAlgorithm, shouldFail: false, shouldFailQuorum: false},             // 29
	{dataBlocks: 6, onDisks: 12, offDisks: 0, blocksize: int64(blockSizeV2), data: int64(2 * blockSizeV2), offset: 512, length: int64(blockSizeV2) + 8*1024, algorithm: DefaultBitrotAlgorithm, shouldFail: false, shouldFailQuorum: false},           // 30
	{dataBlocks: 8, onDisks: 16, offDisks: 0, blocksize: int64(blockSizeV2), data: int64(2 * blockSizeV2), offset: int64(blockSizeV2), length: int64(blockSizeV2) - 1, algorithm: DefaultBitrotAlgorithm, shouldFail: false, shouldFailQuorum: false}, // 31
	{dataBlocks: 2, onDisks: 4, offDisks: 0, blocksize: int64(blockSizeV2), data: int64(oneMiByte), offset: -1, length: 3, algorithm: DefaultBitrotAlgorithm, shouldFail: true, shouldFailQuorum: false},                                              // 32
	{dataBlocks: 2, onDisks: 4, offDisks: 0, blocksize: int64(blockSizeV2), data: int64(oneMiByte), offset: 1024, length: -1, algorithm: DefaultBitrotAlgorithm, shouldFail: true, shouldFailQuorum: false},                                           // 33
	{dataBlocks: 4, onDisks: 6, offDisks: 0, blocksize: int64(blockSizeV2), data: int64(blockSizeV2), offset: 0, length: int64(blockSizeV2), algorithm: BLAKE2b512, shouldFail: false, shouldFailQuorum: false},                                       // 34
	{dataBlocks: 4, onDisks: 6, offDisks: 1, blocksize: int64(blockSizeV2), data: int64(2 * blockSizeV2), offset: 12, length: int64(blockSizeV2) + 17, algorithm: BLAKE2b512, shouldFail: false, shouldFailQuorum: false},                             // 35
	{dataBlocks: 4, onDisks: 6, offDisks: 3, blocksize: int64(blockSizeV2), data: int64(2 * blockSizeV2), offset: 1023, length: int64(blockSizeV2) + 1024, algorithm: DefaultBitrotAlgorithm, shouldFail: false, shouldFailQuorum: true},              // 36
	{dataBlocks: 8, onDisks: 12, offDisks: 4, blocksize: int64(blockSizeV2), data: int64(2 * blockSizeV2), offset: 11, length: int64(blockSizeV2) + 2*1024, algorithm: DefaultBitrotAlgorithm, shouldFail: false, shouldFailQuorum: false},            // 37
}

func TestErasureDecode(t *testing.T) {
	// The streaming bitrot writer draws from the global byte pool
	// which is otherwise only initialized during server pool setup.
	if globalBytePoolCap.Load() == nil {
		globalBytePoolCap.Store(bpool.NewBytePoolCap(64, int(blockSizeV2), 2*int(blockSizeV2)))
	}

	for i, test := range erasureDecodeTests {
		setup, err := newErasureTestSetup(t, test.dataBlocks, test.onDisks-test.dataBlocks, test.blocksize)
		if err != nil {
			t.Fatalf("Test %d: failed to create test setup: %v", i, err)
		}
		erasure, err := NewErasure(t.Context(), test.dataBlocks, test.onDisks-test.dataBlocks, test.blocksize)
		if err != nil {
			t.Fatalf("Test %d: failed to create ErasureStorage: %v", i, err)
		}
		disks := setup.disks
		data := make([]byte, test.data)
		if _, err = io.ReadFull(crand.Reader, data); err != nil {
			t.Fatalf("Test %d: failed to generate random test data: %v", i, err)
		}

		writeAlgorithm := test.algorithm
		if !test.algorithm.Available() {
			writeAlgorithm = DefaultBitrotAlgorithm
		}
		buffer := make([]byte, test.blocksize, 2*test.blocksize)
		writers := make([]io.Writer, len(disks))
		for i, disk := range disks {
			writers[i] = newBitrotWriter(disk, "", "testbucket", "object", erasure.ShardFileSize(test.data), writeAlgorithm, erasure.ShardSize())
		}
		n, err := erasure.Encode(t.Context(), bytes.NewReader(data), writers, buffer, erasure.dataBlocks+1)
		closeBitrotWriters(writers)
		if err != nil {
			t.Fatalf("Test %d: failed to create erasure test file: %v", i, err)
		}
		if n != test.data {
			t.Fatalf("Test %d: failed to create erasure test file", i)
		}
		for i, w := range writers {
			if w == nil {
				disks[i] = nil
			}
		}

		// Get the checksums of the current part.
		bitrotReaders := make([]io.ReaderAt, len(disks))
		for index, disk := range disks {
			if disk == OfflineDisk {
				continue
			}
			tillOffset := erasure.ShardFileOffset(test.offset, test.length, test.data)

			bitrotReaders[index] = newBitrotReader(disk, nil, "testbucket", "object", tillOffset, writeAlgorithm, bitrotWriterSum(writers[index]), erasure.ShardSize())
		}

		writer := bytes.NewBuffer(nil)
		_, err = erasure.Decode(t.Context(), writer, bitrotReaders, test.offset, test.length, test.data, nil)
		closeBitrotReaders(bitrotReaders)
		if err != nil && !test.shouldFail {
			t.Errorf("Test %d: should pass but failed with: %v", i, err)
		}
		if err == nil && test.shouldFail {
			t.Errorf("Test %d: should fail but it passed", i)
		}
		if err == nil {
			if content := writer.Bytes(); !bytes.Equal(content, data[test.offset:test.offset+test.length]) {
				t.Errorf("Test %d: read returns wrong file content.", i)
			}
		}

		for i, r := range bitrotReaders {
			if r == nil {
				disks[i] = OfflineDisk
			}
		}
		if err == nil && !test.shouldFail {
			bitrotReaders = make([]io.ReaderAt, len(disks))
			for index, disk := range disks {
				if disk == OfflineDisk {
					continue
				}
				tillOffset := erasure.ShardFileOffset(test.offset, test.length, test.data)
				bitrotReaders[index] = newBitrotReader(disk, nil, "testbucket", "object", tillOffset, writeAlgorithm, bitrotWriterSum(writers[index]), erasure.ShardSize())
			}
			for j := range disks[:test.offDisks] {
				if bitrotReaders[j] == nil {
					continue
				}
				switch r := bitrotReaders[j].(type) {
				case *wholeBitrotReader:
					r.disk = badDisk{nil}
				case *streamingBitrotReader:
					r.disk = badDisk{nil}
				}
			}
			if test.offDisks > 0 {
				bitrotReaders[0] = nil
			}
			writer.Reset()
			_, err = erasure.Decode(t.Context(), writer, bitrotReaders, test.offset, test.length, test.data, nil)
			closeBitrotReaders(bitrotReaders)
			if err != nil && !test.shouldFailQuorum {
				t.Errorf("Test %d: should pass but failed with: %v", i, err)
			}
			if err == nil && test.shouldFailQuorum {
				t.Errorf("Test %d: should fail but it passed", i)
			}
			if !test.shouldFailQuorum {
				if content := writer.Bytes(); !bytes.Equal(content, data[test.offset:test.offset+test.length]) {
					t.Errorf("Test %d: read returns wrong file content", i)
				}
			}
		}
	}
}

// Test erasureDecode with random offset and lengths.
// This test is t.Skip()ed as it a long time to run, hence should be run
// explicitly after commenting out t.Skip()
func TestErasureDecodeRandomOffsetLength(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	// The streaming bitrot writer draws from the global byte pool
	// which is otherwise only initialized during server pool setup.
	if globalBytePoolCap.Load() == nil {
		globalBytePoolCap.Store(bpool.NewBytePoolCap(64, int(blockSizeV2), 2*int(blockSizeV2)))
	}
	// Initialize environment needed for the test.
	dataBlocks := 7
	parityBlocks := 7
	blockSize := int64(1 * humanize.MiByte)
	setup, err := newErasureTestSetup(t, dataBlocks, parityBlocks, blockSize)
	if err != nil {
		t.Error(err)
		return
	}
	disks := setup.disks
	erasure, err := NewErasure(t.Context(), dataBlocks, parityBlocks, blockSize)
	if err != nil {
		t.Fatalf("failed to create ErasureStorage: %v", err)
	}
	// Prepare a slice of 5MiB with random data.
	data := make([]byte, 5*humanize.MiByte)
	length := int64(len(data))
	_, err = rand.Read(data)
	if err != nil {
		t.Fatal(err)
	}

	writers := make([]io.Writer, len(disks))
	for i, disk := range disks {
		if disk == nil {
			continue
		}
		writers[i] = newBitrotWriter(disk, "", "testbucket", "object", erasure.ShardFileSize(length), DefaultBitrotAlgorithm, erasure.ShardSize())
	}

	// 10000 iterations with random offsets and lengths.
	iterations := 10000

	// Create a test file to read from.
	buffer := make([]byte, blockSize, 2*blockSize)
	n, err := erasure.Encode(t.Context(), bytes.NewReader(data), writers, buffer, erasure.dataBlocks+1)
	closeBitrotWriters(writers)
	if err != nil {
		t.Fatal(err)
	}
	if n != length {
		t.Errorf("erasureCreateFile returned %d, expected %d", n, length)
	}

	// To generate random offset/length.
	r := rand.New(rand.NewSource(UTCNow().UnixNano()))

	buf := &bytes.Buffer{}

	// Verify erasure.Decode() for random offsets and lengths.
	for range iterations {
		offset := r.Int63n(length)
		readLen := r.Int63n(length - offset)

		expected := data[offset : offset+readLen]

		// Get the checksums of the current part.
		bitrotReaders := make([]io.ReaderAt, len(disks))
		for index, disk := range disks {
			if disk == OfflineDisk {
				continue
			}
			tillOffset := erasure.ShardFileOffset(offset, readLen, length)
			bitrotReaders[index] = newStreamingBitrotReader(disk, nil, "testbucket", "object", tillOffset, DefaultBitrotAlgorithm, erasure.ShardSize())
		}
		_, err = erasure.Decode(t.Context(), buf, bitrotReaders, offset, readLen, length, nil)
		closeBitrotReaders(bitrotReaders)
		if err != nil {
			t.Fatal(err, offset, readLen)
		}
		got := buf.Bytes()
		if !bytes.Equal(expected, got) {
			t.Fatalf("read data is different from what was expected, offset=%d length=%d", offset, readLen)
		}
		buf.Reset()
	}
}

// Benchmarks

func benchmarkErasureDecode(data, parity, dataDown, parityDown int, size int64, b *testing.B) {
	setup, err := newErasureTestSetup(b, data, parity, blockSizeV2)
	if err != nil {
		b.Fatalf("failed to create test setup: %v", err)
	}
	disks := setup.disks
	erasure, err := NewErasure(context.Background(), data, parity, blockSizeV2)
	if err != nil {
		b.Fatalf("failed to create ErasureStorage: %v", err)
	}

	writers := make([]io.Writer, len(disks))
	for i, disk := range disks {
		if disk == nil {
			continue
		}
		writers[i] = newBitrotWriter(disk, "", "testbucket", "object", erasure.ShardFileSize(size), DefaultBitrotAlgorithm, erasure.ShardSize())
	}

	content := make([]byte, size)
	buffer := make([]byte, blockSizeV2, 2*blockSizeV2)
	_, err = erasure.Encode(context.Background(), bytes.NewReader(content), writers, buffer, erasure.dataBlocks+1)
	closeBitrotWriters(writers)
	if err != nil {
		b.Fatalf("failed to create erasure test file: %v", err)
	}

	for i := range dataDown {
		writers[i] = nil
	}
	for i := data; i < data+parityDown; i++ {
		writers[i] = nil
	}

	b.SetBytes(size)
	b.ReportAllocs()
	for b.Loop() {
		bitrotReaders := make([]io.ReaderAt, len(disks))
		for index, disk := range disks {
			if writers[index] == nil {
				continue
			}
			tillOffset := erasure.ShardFileOffset(0, size, size)
			bitrotReaders[index] = newStreamingBitrotReader(disk, nil, "testbucket", "object", tillOffset, DefaultBitrotAlgorithm, erasure.ShardSize())
		}
		if _, err = erasure.Decode(context.Background(), bytes.NewBuffer(content[:0]), bitrotReaders, 0, size, size, nil); err != nil {
			panic(err)
		}
		closeBitrotReaders(bitrotReaders)
	}
}

func BenchmarkErasureDecodeQuick(b *testing.B) {
	const size = 12 * 1024 * 1024
	b.Run(" 00|00 ", func(b *testing.B) { benchmarkErasureDecode(2, 2, 0, 0, size, b) })
	b.Run(" 00|X0 ", func(b *testing.B) { benchmarkErasureDecode(2, 2, 0, 1, size, b) })
	b.Run(" X0|00 ", func(b *testing.B) { benchmarkErasureDecode(2, 2, 1, 0, size, b) })
	b.Run(" X0|X0 ", func(b *testing.B) { benchmarkErasureDecode(2, 2, 1, 1, size, b) })
}

func BenchmarkErasureDecode_4_64KB(b *testing.B) {
	const size = 64 * 1024
	b.Run(" 00|00 ", func(b *testing.B) { benchmarkErasureDecode(2, 2, 0, 0, size, b) })
	b.Run(" 00|X0 ", func(b *testing.B) { benchmarkErasureDecode(2, 2, 0, 1, size, b) })
	b.Run(" X0|00 ", func(b *testing.B) { benchmarkErasureDecode(2, 2, 1, 0, size, b) })
	b.Run(" X0|X0 ", func(b *testing.B) { benchmarkErasureDecode(2, 2, 1, 1, size, b) })
	b.Run(" 00|XX ", func(b *testing.B) { benchmarkErasureDecode(2, 2, 0, 2, size, b) })
	b.Run(" XX|00 ", func(b *testing.B) { benchmarkErasureDecode(2, 2, 2, 0, size, b) })
}

func BenchmarkErasureDecode_8_20MB(b *testing.B) {
	const size = 20 * 1024 * 1024
	b.Run(" 0000|0000 ", func(b *testing.B) { benchmarkErasureDecode(4, 4, 0, 0, size, b) })
	b.Run(" 0000|X000 ", func(b *testing.B) { benchmarkErasureDecode(4, 4, 0, 1, size, b) })
	b.Run(" X000|0000 ", func(b *testing.B) { benchmarkErasureDecode(4, 4, 1, 0, size, b) })
	b.Run(" X000|X000 ", func(b *testing.B) { benchmarkErasureDecode(4, 4, 1, 1, size, b) })
	b.Run(" 0000|XXXX ", func(b *testing.B) { benchmarkErasureDecode(4, 4, 0, 4, size, b) })
	b.Run(" XX00|XX00 ", func(b *testing.B) { benchmarkErasureDecode(4, 4, 2, 2, size, b) })
	b.Run(" XXXX|0000 ", func(b *testing.B) { benchmarkErasureDecode(4, 4, 4, 0, size, b) })
}

func BenchmarkErasureDecode_12_30MB(b *testing.B) {
	const size = 30 * 1024 * 1024
	b.Run(" 000000|000000 ", func(b *testing.B) { benchmarkErasureDecode(6, 6, 0, 0, size, b) })
	b.Run(" 000000|X00000 ", func(b *testing.B) { benchmarkErasureDecode(6, 6, 0, 1, size, b) })
	b.Run(" X00000|000000 ", func(b *testing.B) { benchmarkErasureDecode(6, 6, 1, 0, size, b) })
	b.Run(" X00000|X00000 ", func(b *testing.B) { benchmarkErasureDecode(6, 6, 1, 1, size, b) })
	b.Run(" 000000|XXXXXX ", func(b *testing.B) { benchmarkErasureDecode(6, 6, 0, 6, size, b) })
	b.Run(" XXX000|XXX000 ", func(b *testing.B) { benchmarkErasureDecode(6, 6, 3, 3, size, b) })
	b.Run(" XXXXXX|000000 ", func(b *testing.B) { benchmarkErasureDecode(6, 6, 6, 0, size, b) })
}

func BenchmarkErasureDecode_16_40MB(b *testing.B) {
	const size = 40 * 1024 * 1024
	b.Run(" 00000000|00000000 ", func(b *testing.B) { benchmarkErasureDecode(8, 8, 0, 0, size, b) })
	b.Run(" 00000000|X0000000 ", func(b *testing.B) { benchmarkErasureDecode(8, 8, 0, 1, size, b) })
	b.Run(" X0000000|00000000 ", func(b *testing.B) { benchmarkErasureDecode(8, 8, 1, 0, size, b) })
	b.Run(" X0000000|X0000000 ", func(b *testing.B) { benchmarkErasureDecode(8, 8, 1, 1, size, b) })
	b.Run(" 00000000|XXXXXXXX ", func(b *testing.B) { benchmarkErasureDecode(8, 8, 0, 8, size, b) })
	b.Run(" XXXX0000|XXXX0000 ", func(b *testing.B) { benchmarkErasureDecode(8, 8, 4, 4, size, b) })
	b.Run(" XXXXXXXX|00000000 ", func(b *testing.B) { benchmarkErasureDecode(8, 8, 8, 0, size, b) })
}

// gatedReaderAt blocks the first ReadAt call until gate is closed.
// All later calls pass through. started is closed once the first
// ReadAt is blocked, done once it has returned.
type gatedReaderAt struct {
	inner   io.ReaderAt
	gate    chan struct{}
	started chan struct{}
	done    chan struct{}
	block   sync.Once
	calls   atomic.Int32
}

func (g *gatedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	g.calls.Add(1)
	g.block.Do(func() {
		close(g.started)
		<-g.gate
		close(g.done)
	})
	return g.inner.ReadAt(p, off)
}

func (g *gatedReaderAt) Close() error {
	if c, ok := g.inner.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// missingShardReader always fails with errFileNotFound.
type missingShardReader struct{}

func (missingShardReader) ReadAt([]byte, int64) (int, error) { return 0, errFileNotFound }

// hedgedEncodeShards encodes data block by block and returns the
// shard files, mimicking the layout produced by Erasure.Encode.
func hedgedEncodeShards(t *testing.T, e Erasure, data []byte) [][]byte {
	t.Helper()
	shards := make([][]byte, e.dataBlocks+e.parityBlocks)
	for off := 0; off < len(data); off += int(e.blockSize) {
		chunk := data[off:]
		if len(chunk) > int(e.blockSize) {
			chunk = chunk[:int(e.blockSize)]
		}
		encoded, err := e.EncodeData(t.Context(), chunk)
		if err != nil {
			t.Fatalf("EncodeData failed: %v", err)
		}
		for i := range shards {
			shards[i] = append(shards[i], encoded[i]...)
		}
	}
	return shards
}

func hedgedDecodeShards(t *testing.T, e Erasure, bufs [][]byte) []byte {
	t.Helper()
	if err := e.DecodeDataBlocks(bufs); err != nil {
		t.Fatalf("DecodeDataBlocks failed: %v", err)
	}
	var out []byte
	for i := 0; i < e.dataBlocks; i++ {
		out = append(out, bufs[i]...)
	}
	return out
}

func hedgedDeliveredCount(bufs [][]byte) int {
	n := 0
	for _, b := range bufs {
		if len(b) > 0 {
			n++
		}
	}
	return n
}

func hedgedShardReaders(t *testing.T, shards [][]byte) []io.ReaderAt {
	t.Helper()
	readers := make([]io.ReaderAt, len(shards))
	for i := range shards {
		readers[i] = bytes.NewReader(shards[i])
	}
	return readers
}

// TestParallelReaderHedgedOneSlow verifies that a single slow shard is
// covered by the dataBlocks+1 initial launch without waiting for the
// fan-out delay or the slow disk.
func TestParallelReaderHedgedOneSlow(t *testing.T) {
	const dataBlocks = 3
	e, err := NewErasure(t.Context(), dataBlocks, 2, 30)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 60)
	for i := range data {
		data[i] = byte(i)
	}
	shards := hedgedEncodeShards(t, e, data)
	readers := hedgedShardReaders(t, shards)

	gated := &gatedReaderAt{
		inner:   readers[0],
		gate:    make(chan struct{}),
		started: make(chan struct{}),
		done:    make(chan struct{}),
	}
	readers[0] = gated

	const fanoutDelay = 10 * time.Second
	p := newParallelReader(readers, e, 0, int64(len(data)), fanoutDelay)
	defer p.Done()

	for block := 0; block < 2; block++ {
		start := time.Now()
		bufs, err := p.Read(nil)
		if err != nil {
			t.Fatalf("block %d: Read failed: %v", block, err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("block %d: Read blocked for %v, hedged read should not wait for the slow shard", block, elapsed)
		}
		if got := hedgedDeliveredCount(bufs); got != dataBlocks {
			t.Fatalf("block %d: got %d delivered shards, want %d", block, got, dataBlocks)
		}
		if got := hedgedDecodeShards(t, e, bufs); !bytes.Equal(got, data[block*30:(block+1)*30]) {
			t.Fatalf("block %d: decoded data mismatch", block)
		}
	}

	// The slow reader was launched once per block, the never-needed
	// 5th shard was never launched since the hedge sufficed.
	if got := gated.calls.Load(); got != 1 {
		t.Fatalf("gated reader launched %d times, want 1", got)
	}

	p.Done()
	p.stateLK.Lock()
	if readers[0] != nil {
		t.Fatal("straggler reader should be marked nil in orgReaders after Done()")
	}
	p.stateLK.Unlock()

	close(gated.gate)
	<-gated.done
}

// TestParallelReaderHedgedFanout verifies that the fan-out timer
// launches the remaining shards after fanoutDelay when two shards
// are slow.
func TestParallelReaderHedgedFanout(t *testing.T) {
	const dataBlocks = 3
	e, err := NewErasure(t.Context(), dataBlocks, 2, 30)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 30)
	for i := range data {
		data[i] = byte(i)
	}
	shards := hedgedEncodeShards(t, e, data)
	readers := hedgedShardReaders(t, shards)

	gated0 := &gatedReaderAt{inner: readers[0], gate: make(chan struct{}), started: make(chan struct{}), done: make(chan struct{})}
	gated1 := &gatedReaderAt{inner: readers[1], gate: make(chan struct{}), started: make(chan struct{}), done: make(chan struct{})}
	readers[0] = gated0
	readers[1] = gated1

	const fanoutDelay = 300 * time.Millisecond
	p := newParallelReader(readers, e, 0, int64(len(data)), fanoutDelay)
	defer p.Done()

	start := time.Now()
	bufs, err := p.Read(nil)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Fatalf("Read returned after %v, fan-out delay did not trigger", elapsed)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Read returned after %v, too slow", elapsed)
	}
	if got := hedgedDeliveredCount(bufs); got != dataBlocks {
		t.Fatalf("got %d delivered shards, want %d", got, dataBlocks)
	}
	if got := hedgedDecodeShards(t, e, bufs); !bytes.Equal(got, data) {
		t.Fatal("decoded data mismatch")
	}

	close(gated0.gate)
	close(gated1.gate)
}

// TestParallelReaderHedgedErrorSubstitution verifies that a shard
// failing immediately is substituted right away, without waiting
// for the fan-out delay.
func TestParallelReaderHedgedErrorSubstitution(t *testing.T) {
	const dataBlocks = 3
	e, err := NewErasure(t.Context(), dataBlocks, 2, 30)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 30)
	for i := range data {
		data[i] = byte(i)
	}
	shards := hedgedEncodeShards(t, e, data)
	readers := hedgedShardReaders(t, shards)
	readers[0] = missingShardReader{}

	const fanoutDelay = 10 * time.Second
	p := newParallelReader(readers, e, 0, int64(len(data)), fanoutDelay)
	defer p.Done()

	start := time.Now()
	bufs, err := p.Read(nil)
	if !errors.Is(err, errFileNotFound) {
		t.Fatalf("expected errFileNotFound heal flag, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Read blocked for %v, substitution should be immediate", elapsed)
	}
	if got := hedgedDeliveredCount(bufs); got != dataBlocks {
		t.Fatalf("got %d delivered shards, want %d", got, dataBlocks)
	}
	if got := hedgedDecodeShards(t, e, bufs); !bytes.Equal(got, data) {
		t.Fatal("decoded data mismatch")
	}
}

// TestParallelReaderHedgedQuorum verifies the read quorum error when
// too many shards fail.
func TestParallelReaderHedgedQuorum(t *testing.T) {
	const dataBlocks = 3
	e, err := NewErasure(t.Context(), dataBlocks, 2, 30)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 30)
	shards := hedgedEncodeShards(t, e, data)
	readers := hedgedShardReaders(t, shards)
	readers[0] = missingShardReader{}
	readers[1] = missingShardReader{}
	readers[2] = missingShardReader{}

	p := newParallelReader(readers, e, 0, int64(len(data)), 10*time.Second)
	defer p.Done()

	bufs, err := p.Read(nil)
	if !errors.Is(err, errErasureReadQuorum) {
		t.Fatalf("expected errErasureReadQuorum, got %v", err)
	}
	if bufs != nil {
		t.Fatal("expected nil bufs on quorum error")
	}
}

// TestParallelReaderHedgedRejoin verifies that a straggler that
// completes after its block returned rejoins for the next block.
func TestParallelReaderHedgedRejoin(t *testing.T) {
	const dataBlocks = 3
	e, err := NewErasure(t.Context(), dataBlocks, 2, 30)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 60)
	for i := range data {
		data[i] = byte(i)
	}
	shards := hedgedEncodeShards(t, e, data)
	readers := hedgedShardReaders(t, shards)

	gated := &gatedReaderAt{
		inner:   readers[0],
		gate:    make(chan struct{}),
		started: make(chan struct{}),
		done:    make(chan struct{}),
	}
	readers[0] = gated

	p := newParallelReader(readers, e, 0, int64(len(data)), 10*time.Second)
	defer p.Done()

	// Block 0: reader 0 straggles, delivered via hedge.
	bufs, err := p.Read(nil)
	if err != nil {
		t.Fatalf("block 0: Read failed: %v", err)
	}
	if got := hedgedDecodeShards(t, e, bufs); !bytes.Equal(got, data[0:30]) {
		t.Fatal("block 0: decoded data mismatch")
	}

	// Release the straggler and wait until it is available again.
	close(gated.gate)
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.stateLK.Lock()
		inflight := p.inflight[0]
		p.stateLK.Unlock()
		if !inflight {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("straggler never completed")
		}
		time.Sleep(time.Millisecond)
	}

	// Block 1: reader 0 must have rejoined and be launched again.
	bufs, err = p.Read(nil)
	if err != nil {
		t.Fatalf("block 1: Read failed: %v", err)
	}
	if got := hedgedDeliveredCount(bufs); got != dataBlocks {
		t.Fatalf("block 1: got %d delivered shards, want %d", got, dataBlocks)
	}
	if got := hedgedDecodeShards(t, e, bufs); !bytes.Equal(got, data[30:60]) {
		t.Fatal("block 1: decoded data mismatch")
	}
	if got := gated.calls.Load(); got != 2 {
		t.Fatalf("gated reader launched %d times, want 2", got)
	}
}

// TestErasureDecodeHedgedSlowDisk verifies the full decode path with
// hedged reads enabled: a disk stuck in a read does not stall the
// object read.
func TestErasureDecodeHedgedSlowDisk(t *testing.T) {
	// The streaming bitrot writer draws from the global byte pool
	// which is otherwise only initialized during server pool setup.
	if globalBytePoolCap.Load() == nil {
		globalBytePoolCap.Store(bpool.NewBytePoolCap(64, int(blockSizeV2), 2*int(blockSizeV2)))
	}

	dataBlocks := 3
	parityBlocks := 2
	setup, err := newErasureTestSetup(t, dataBlocks, parityBlocks, blockSizeV2)
	if err != nil {
		t.Fatalf("failed to create test setup: %v", err)
	}
	erasure, err := NewErasure(t.Context(), dataBlocks, parityBlocks, blockSizeV2)
	if err != nil {
		t.Fatalf("failed to create ErasureStorage: %v", err)
	}

	data := make([]byte, oneMiByte)
	if _, err = io.ReadFull(crand.Reader, data); err != nil {
		t.Fatal(err)
	}

	buffer := make([]byte, blockSizeV2, 2*blockSizeV2)
	writers := make([]io.Writer, len(setup.disks))
	for i, disk := range setup.disks {
		writers[i] = newBitrotWriter(disk, "", "testbucket", "object", erasure.ShardFileSize(int64(len(data))), DefaultBitrotAlgorithm, erasure.ShardSize())
	}
	if _, err = erasure.Encode(t.Context(), bytes.NewReader(data), writers, buffer, erasure.dataBlocks+1); err != nil {
		t.Fatal(err)
	}
	closeBitrotWriters(writers)

	bitrotReaders := make([]io.ReaderAt, len(setup.disks))
	for index, disk := range setup.disks {
		tillOffset := erasure.ShardFileOffset(0, int64(len(data)), int64(len(data)))
		bitrotReaders[index] = newBitrotReader(disk, nil, "testbucket", "object", tillOffset, DefaultBitrotAlgorithm, bitrotWriterSum(writers[index]), erasure.ShardSize())
	}
	gated := &gatedReaderAt{
		inner:   bitrotReaders[0],
		gate:    make(chan struct{}),
		started: make(chan struct{}),
		done:    make(chan struct{}),
	}
	bitrotReaders[0] = gated

	globalDriveConfig.Update(drive.Config{ReadFanoutDelay: 10 * time.Second})
	defer globalDriveConfig.Update(drive.Config{})

	writer := bytes.NewBuffer(nil)
	start := time.Now()
	_, err = erasure.Decode(t.Context(), writer, bitrotReaders, 0, int64(len(data)), int64(len(data)), nil)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Decode blocked for %v, slow disk should not stall the read", elapsed)
	}
	if !bytes.Equal(writer.Bytes(), data) {
		t.Fatal("read returns wrong file content")
	}

	// The straggler is marked in the caller's reader slice so that
	// closeBitrotReaders skips it; its goroutine closes it after the
	// read returns.
	if bitrotReaders[0] != nil {
		t.Fatal("straggler reader should be marked nil for the caller")
	}

	close(gated.gate)
	<-gated.done
	closeBitrotReaders(bitrotReaders)
}
