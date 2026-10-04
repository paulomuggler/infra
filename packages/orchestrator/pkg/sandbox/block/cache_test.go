package block

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"math"
	"os"
	"syscall"
	"testing"
	"unsafe"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/uffd/testutils"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

func allocateTestMemory(t *testing.T, size uint64, pageSize uint64) (addr uint64, expectedData []byte) {
	t.Helper()

	mem, memoryStart, err := testutils.NewPageMmap(t, size, pageSize)
	require.NoError(t, err)

	n, err := rand.Read(mem)
	require.NoError(t, err)
	require.Equal(t, len(mem), n)

	return uint64(memoryStart), mem
}

func TestCopyFromProcess_FullRange(t *testing.T) {
	t.Parallel()

	pageSize := int64(header.PageSize)
	size := pageSize * 30

	addr, mem := allocateTestMemory(t, uint64(size), uint64(pageSize))

	ranges := []Range{
		{Start: int64(addr), Size: size},
	}

	cache, err := NewCacheFromProcessMemory(
		t.Context(),
		pageSize,
		t.TempDir()+"/cache",
		os.Getpid(),
		ranges,
	)
	require.NoError(t, err)

	t.Cleanup(func() {
		cache.Close()
	})

	data := make([]byte, size)
	n, err := cache.ReadAt(data, 0)
	require.NoError(t, err)
	require.Equal(t, int(size), n)

	require.NoError(t, compareData(data[:n], mem[:n]))
}

func TestCopyFromProcess_LargeRanges(t *testing.T) {
	t.Parallel()

	pageSize := uint64(header.PageSize)
	totalSize := pageSize * 5

	addr, mem := allocateTestMemory(t, totalSize, pageSize)

	ranges := []Range{
		{Start: int64(addr), Size: int64(pageSize)},
		{Start: int64(addr + pageSize*3), Size: int64(pageSize)},
		{Start: int64(addr + pageSize), Size: int64(pageSize)},
	}

	cache, err := NewCacheFromProcessMemory(
		t.Context(),
		int64(pageSize),
		t.TempDir()+"/cache",
		os.Getpid(),
		ranges,
	)
	require.NoError(t, err)

	t.Cleanup(func() {
		cache.Close()
	})

	data1 := make([]byte, pageSize)
	n, err := cache.ReadAt(data1, 0)
	require.NoError(t, err)
	require.Equal(t, int(pageSize), n)
	require.NoError(t, compareData(data1[:n], mem[0:pageSize]))

	data2 := make([]byte, pageSize)
	n, err = cache.ReadAt(data2, int64(pageSize))
	require.NoError(t, err)
	require.Equal(t, int(pageSize), n)
	require.NoError(t, compareData(data2[:n], mem[pageSize*3:pageSize*4]))

	data3 := make([]byte, pageSize)
	n, err = cache.ReadAt(data3, int64(pageSize*2))
	require.NoError(t, err)
	require.Equal(t, int(pageSize), n)
	require.NoError(t, compareData(data3[:n], mem[pageSize:pageSize*2]))
}

func TestCopyFromProcess_MultipleRanges(t *testing.T) {
	t.Parallel()

	numRanges := 1500
	pageSize := uint64(header.PageSize)
	rangeSize := int64(pageSize * 64)

	totalSize := rangeSize * int64(numRanges)

	addr, mem := allocateTestMemory(t, uint64(totalSize), pageSize)

	ranges := make([]Range, numRanges)
	for i := range numRanges {
		ranges[i] = Range{
			Start: int64(addr) + int64(i)*rangeSize,
			Size:  rangeSize,
		}
	}

	cache, err := NewCacheFromProcessMemory(
		t.Context(),
		int64(pageSize),
		t.TempDir()+"/cache",
		os.Getpid(),
		ranges,
	)
	require.NoError(t, err)

	t.Cleanup(func() {
		cache.Close()
	})

	checkCount := min(numRanges, 10)
	for i := range checkCount {
		actualOffset := int64(i) * rangeSize
		alignedOffset := (actualOffset / int64(pageSize)) * int64(pageSize)

		data := make([]byte, pageSize)

		n, err := cache.ReadAt(data, alignedOffset)
		require.NoError(t, err)
		require.Equal(t, int(pageSize), n)

		require.NoError(t, compareData(data[:n], mem[alignedOffset:alignedOffset+int64(pageSize)]))
	}
}

func TestCopyFromProcess_HugepageToRegularPage(t *testing.T) {
	t.Parallel()

	pageSize := int64(header.HugepageSize)
	size := pageSize * 10

	mem, addr, err := testutils.NewPageMmap(t, uint64(size), uint64(pageSize))
	require.NoError(t, err)

	n, err := rand.Read(mem)
	require.NoError(t, err)
	require.Equal(t, len(mem), n)

	ranges := []Range{
		{Start: int64(addr), Size: pageSize * 2},
		{Start: int64(addr) + pageSize*4, Size: pageSize * 4},
	}

	cache, err := NewCacheFromProcessMemory(
		t.Context(),
		// Regular 4KiB pages.
		header.PageSize,
		t.TempDir()+"/cache",
		os.Getpid(),
		ranges,
	)
	require.NoError(t, err)

	t.Cleanup(func() {
		cache.Close()
	})

	data := make([]byte, pageSize*2)
	n, err = cache.ReadAt(data, 0)
	require.NoError(t, err)
	require.Equal(t, int(pageSize*2), n)
	require.NoError(t, compareData(data[:n], mem[0:pageSize*2]))

	data = make([]byte, pageSize*4)
	n, err = cache.ReadAt(data, pageSize*2)
	require.NoError(t, err)
	require.Equal(t, int(pageSize*4), n)
	require.NoError(t, compareData(data[:n], mem[pageSize*4:pageSize*8]))
}

func TestSliceDirectOutOfBoundsReturnsBytesNotAvailable(t *testing.T) {
	t.Parallel()

	cache, err := NewCache(16, 4, t.TempDir()+"/cache", false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cache.Close() })

	_, err = cache.sliceDirect(16, 4)
	require.ErrorIs(t, err, BytesNotAvailableError{})

	_, err = cache.sliceDirect(32, 4)
	require.ErrorIs(t, err, BytesNotAvailableError{})
}

func TestEmptyRanges(t *testing.T) {
	t.Parallel()

	c, err := NewCacheFromProcessMemory(
		t.Context(),
		header.PageSize,
		t.TempDir()+"/cache",
		os.Getpid(),
		nil,
	)
	require.NoError(t, err)

	t.Cleanup(func() {
		c.Close()
	})
}

func TestCacheExportToDiff_ZeroDirtyBlockEmittedAsDirtyPayload(t *testing.T) {
	t.Parallel()

	const blockSize = header.RootfsBlockSize
	cache, err := NewCache(blockSize, blockSize, t.TempDir()+"/cache", false)
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, cache.Close())
	})

	zeroBlock := make([]byte, blockSize)
	n, err := cache.WriteAt(zeroBlock, 0)
	require.NoError(t, err)
	require.Equal(t, int(blockSize), n)

	out, err := os.CreateTemp(t.TempDir(), "diff-*")
	require.NoError(t, err)
	defer out.Close()

	diffMetadata, err := cache.ExportToDiff(t.Context(), out)
	require.NoError(t, err)

	require.EqualValues(t, 1, diffMetadata.Dirty.GetCardinality(), "zero-filled dirty block should be emitted as dirty payload")
	require.EqualValues(t, 0, diffMetadata.Empty.GetCardinality(), "zero-filled dirty block should not be tracked in empty metadata")

	stat, err := out.Stat()
	require.NoError(t, err)
	require.EqualValues(t, blockSize, stat.Size(), "zero-filled dirty block should write block payload bytes")
}

func TestCacheExportToDiff_ZeroDirtyBlockMapsToSnapshotBuild(t *testing.T) {
	t.Parallel()

	const blockSize = header.RootfsBlockSize
	cache, err := NewCache(blockSize, blockSize, t.TempDir()+"/cache", false)
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, cache.Close())
	})

	zeroBlock := make([]byte, blockSize)
	n, err := cache.WriteAt(zeroBlock, 0)
	require.NoError(t, err)
	require.Equal(t, int(blockSize), n)

	out, err := os.CreateTemp(t.TempDir(), "diff-*")
	require.NoError(t, err)
	defer out.Close()

	diffMetadata, err := cache.ExportToDiff(t.Context(), out)
	require.NoError(t, err)

	baseBuildID := uuid.New()
	originalHeader, err := header.NewHeader(
		header.NewTemplateMetadata(baseBuildID, uint64(blockSize), uint64(blockSize)),
		nil,
	)
	require.NoError(t, err)

	snapshotBuildID := uuid.New()
	diffHeader, err := diffMetadata.ToDiffHeader(t.Context(), originalHeader, snapshotBuildID)
	require.NoError(t, err)

	_, _, mappedBuildID, err := diffHeader.GetShiftedMapping(t.Context(), 0)
	require.NoError(t, err)

	require.NotNil(t, mappedBuildID)
	require.Equal(t, snapshotBuildID, *mappedBuildID, "zero-filled dirty block should map to the snapshot diff when empty detection is skipped")
	require.NotEqual(t, uuid.Nil, *mappedBuildID, "zero-filled dirty block should no longer be represented as an empty mapping")
}

func TestCacheExportToDiff_MixedDirtyBlocksKeepsZeroBlockInDiff(t *testing.T) {
	t.Parallel()

	const blockSize = header.RootfsBlockSize
	const size = blockSize * 3

	cache, err := NewCache(size, blockSize, t.TempDir()+"/cache", false)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, cache.Close())
	})

	zeroBlock := make([]byte, blockSize)
	nonZeroBlock := bytes.Repeat([]byte{0xAB}, int(blockSize))

	_, err = cache.WriteAt(zeroBlock, 0)
	require.NoError(t, err)

	_, err = cache.WriteAt(nonZeroBlock, blockSize)
	require.NoError(t, err)

	out, err := os.CreateTemp(t.TempDir(), "diff-*")
	require.NoError(t, err)
	defer out.Close()

	diffMetadata, err := cache.ExportToDiff(t.Context(), out)
	require.NoError(t, err)

	require.EqualValues(t, 2, diffMetadata.Dirty.GetCardinality())
	require.EqualValues(t, 0, diffMetadata.Empty.GetCardinality(), "mixed export should still skip empty tracking for zero-filled dirty blocks")

	_, err = out.Seek(0, io.SeekStart)
	require.NoError(t, err)
	exported, err := io.ReadAll(out)
	require.NoError(t, err)
	expected := make([]byte, 0, len(zeroBlock)+len(nonZeroBlock))
	expected = append(expected, zeroBlock...)
	expected = append(expected, nonZeroBlock...)
	require.Equal(t, expected, exported)

	baseBuildID := uuid.New()
	originalHeader, err := header.NewHeader(
		header.NewTemplateMetadata(baseBuildID, uint64(blockSize), uint64(size)),
		nil,
	)
	require.NoError(t, err)

	snapshotBuildID := uuid.New()
	diffHeader, err := diffMetadata.ToDiffHeader(t.Context(), originalHeader, snapshotBuildID)
	require.NoError(t, err)

	_, _, firstBlockBuildID, err := diffHeader.GetShiftedMapping(t.Context(), 0)
	require.NoError(t, err)
	require.Equal(t, snapshotBuildID, *firstBlockBuildID, "zero-filled dirty block should still map to the snapshot diff")

	_, _, secondBlockBuildID, err := diffHeader.GetShiftedMapping(t.Context(), blockSize)
	require.NoError(t, err)
	require.Equal(t, snapshotBuildID, *secondBlockBuildID)

	_, _, thirdBlockBuildID, err := diffHeader.GetShiftedMapping(t.Context(), 2*blockSize)
	require.NoError(t, err)
	require.Equal(t, baseBuildID, *thirdBlockBuildID, "clean blocks should keep the base mapping")
}

func TestCacheExportToDiff_NonContiguousDirtyBlocksPreserveRangeOrder(t *testing.T) {
	t.Parallel()

	const blockSize = header.RootfsBlockSize
	const size = blockSize * 5

	cache, err := NewCache(size, blockSize, t.TempDir()+"/cache", false)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, cache.Close())
	})

	firstBlock := bytes.Repeat([]byte{0x11}, int(blockSize))
	secondBlock := bytes.Repeat([]byte{0x22}, int(blockSize))

	_, err = cache.WriteAt(firstBlock, 0)
	require.NoError(t, err)

	_, err = cache.WriteAt(secondBlock, 3*blockSize)
	require.NoError(t, err)

	out, err := os.CreateTemp(t.TempDir(), "diff-*")
	require.NoError(t, err)
	defer out.Close()

	diffMetadata, err := cache.ExportToDiff(t.Context(), out)
	require.NoError(t, err)

	require.EqualValues(t, 2, diffMetadata.Dirty.GetCardinality())
	require.True(t, diffMetadata.Dirty.Contains(0))
	require.True(t, diffMetadata.Dirty.Contains(3))
	require.EqualValues(t, 0, diffMetadata.Empty.GetCardinality())

	_, err = out.Seek(0, io.SeekStart)
	require.NoError(t, err)
	exported, err := io.ReadAll(out)
	require.NoError(t, err)
	require.Equal(t, append(firstBlock, secondBlock...), exported)
}

func TestCache_ZeroLengthIsCachedAndSetIsCached(t *testing.T) {
	t.Parallel()

	const blockSize int64 = 4096
	const size int64 = blockSize * 10

	cache, err := NewCache(size, blockSize, t.TempDir()+"/cache", false)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, cache.Close())
	})

	// Before any writes, isCached(0, blockSize) should be false
	require.False(t, cache.isCached(0, blockSize), "block 0 should not be cached initially")

	// Zero-length isCached should return true (vacuous truth) and NOT check dirty map
	require.True(t, cache.isCached(0, 0), "zero-length isCached should return true (no-op)")

	// Zero-length setIsCached should be a no-op and NOT mark anything as cached
	cache.setIsCached(0, 0)

	// Block 0 should still not be cached after zero-length setIsCached
	require.False(t, cache.isCached(0, blockSize), "block 0 should still not be cached after zero-length setIsCached")

	// Test with various offsets to ensure zero-length is always a no-op
	cache.setIsCached(blockSize, 0)
	require.False(t, cache.isCached(blockSize, blockSize), "block 1 should not be cached after zero-length setIsCached")

	cache.setIsCached(blockSize*5, 0)
	require.False(t, cache.isCached(blockSize*5, blockSize), "block 5 should not be cached after zero-length setIsCached")
}

func compareData(readBytes []byte, expectedBytes []byte) error {
	// The bytes.Equal is the first place in this flow that actually touches the uffd managed memory and triggers the pagefault, so any deadlocks will manifest here.
	if !bytes.Equal(readBytes, expectedBytes) {
		idx, want, got := testutils.FirstDifferentByte(readBytes, expectedBytes)

		return fmt.Errorf("content mismatch: want '%x, got %x at index %d", want, got, idx)
	}

	return nil
}

func TestSplitOversizedRanges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		ranges   []Range
		maxSize  int64
		expected []Range
	}{
		{
			name:     "empty input",
			ranges:   nil,
			maxSize:  100,
			expected: nil,
		},
		{
			name: "all ranges within limit",
			ranges: []Range{
				{Start: 0, Size: 50},
				{Start: 100, Size: 50},
			},
			maxSize: 100,
			expected: []Range{
				{Start: 0, Size: 50},
				{Start: 100, Size: 50},
			},
		},
		{
			name: "range exactly at limit",
			ranges: []Range{
				{Start: 0, Size: 100},
			},
			maxSize: 100,
			expected: []Range{
				{Start: 0, Size: 100},
			},
		},
		{
			name: "single oversized range splits evenly",
			ranges: []Range{
				{Start: 0, Size: 300},
			},
			maxSize: 100,
			expected: []Range{
				{Start: 0, Size: 100},
				{Start: 100, Size: 100},
				{Start: 200, Size: 100},
			},
		},
		{
			name: "single oversized range with remainder",
			ranges: []Range{
				{Start: 0, Size: 250},
			},
			maxSize: 100,
			expected: []Range{
				{Start: 0, Size: 100},
				{Start: 100, Size: 100},
				{Start: 200, Size: 50},
			},
		},
		{
			name: "mixed ranges - some need splitting",
			ranges: []Range{
				{Start: 0, Size: 50},
				{Start: 100, Size: 250},
				{Start: 400, Size: 80},
			},
			maxSize: 100,
			expected: []Range{
				{Start: 0, Size: 50},
				{Start: 100, Size: 100},
				{Start: 200, Size: 100},
				{Start: 300, Size: 50},
				{Start: 400, Size: 80},
			},
		},
		{
			name: "range just over limit",
			ranges: []Range{
				{Start: 0, Size: 101},
			},
			maxSize: 100,
			expected: []Range{
				{Start: 0, Size: 100},
				{Start: 100, Size: 1},
			},
		},
		{
			name: "preserves start addresses correctly",
			ranges: []Range{
				{Start: 1000, Size: 250},
			},
			maxSize: 100,
			expected: []Range{
				{Start: 1000, Size: 100},
				{Start: 1100, Size: 100},
				{Start: 1200, Size: 50},
			},
		},
		{
			name: "demonstrate unoptimal split",
			ranges: []Range{
				{Start: 1000, Size: 250},
				{Start: 1250, Size: 250},
			},
			maxSize: 240,
			expected: []Range{
				{Start: 1000, Size: 240},
				{Start: 1240, Size: 10},
				{Start: 1250, Size: 240},
				{Start: 1490, Size: 10},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := splitOversizedRanges(tt.ranges, tt.maxSize)
			require.Equal(t, tt.expected, result)
		})
	}
}

// This test is used to verify that the code correctly splits the ranges when the total size exceeds MAX_RW_COUNT.
func TestCopyFromProcess_Exceed_MAX_RW_COUNT(t *testing.T) {
	t.Parallel()

	pageSize := int64(header.PageSize)
	// We allocate more than MAX_RW_COUNT to trigger the MAX_RW_COUNT error if the ranges are not split correctly.
	size := ((MAX_RW_COUNT + 4*pageSize + pageSize - 1) / pageSize) * pageSize

	// Initialize the memory we will copy from.
	mem, addr, err := testutils.NewPageMmap(t, uint64(size), uint64(pageSize))
	require.NoError(t, err)

	n, err := rand.Read(mem)
	require.NoError(t, err)
	require.Equal(t, len(mem), n)

	ranges := []Range{
		// We make it so that at least one of the ranges is larger than MAX_RW_COUNT.
		{Start: int64(addr), Size: ((MAX_RW_COUNT + 2*pageSize + pageSize - 1) / pageSize) * pageSize},
		{Start: int64(addr) + ((MAX_RW_COUNT+2*pageSize+pageSize-1)/pageSize)*pageSize, Size: ((2*pageSize + pageSize - 1) / pageSize) * pageSize},
	}

	cache, err := NewCacheFromProcessMemory(
		t.Context(),
		// Regular 4KiB pages for the cache/mmap we will copy to.
		header.PageSize,
		t.TempDir()+"/cache",
		os.Getpid(),
		ranges,
	)
	require.NoError(t, err)

	t.Cleanup(func() {
		cache.Close()
	})

	data := make([]byte, size)
	n, err = cache.ReadAt(data, 0)
	require.NoError(t, err)
	require.Equal(t, int(size), n)
	require.NoError(t, compareData(data[:n], mem[0:size]))
}

// Tests for a misalignment of the block size and the MAX_RW_COUNT that causes incorrect dirty tracking.
func TestCopyFromProcess_MAX_RW_COUNT_Misalignment_Hugepage(t *testing.T) {
	t.Parallel()

	pageSize := int64(header.HugepageSize)
	// We allocate more than MAX_RW_COUNT/pageSize + 2 to misalign the dirty tracking if the range split is unaligned to the block size.
	size := ((MAX_RW_COUNT/pageSize + 2) * pageSize)

	mem, addr, err := testutils.NewPageMmap(t, uint64(size), uint64(pageSize))
	require.NoError(t, err)

	n, err := rand.Read(mem)
	require.NoError(t, err)
	require.Equal(t, len(mem), n)

	ranges := []Range{
		{Start: int64(addr), Size: size},
	}

	cache, err := NewCacheFromProcessMemory(
		t.Context(),
		header.HugepageSize,
		t.TempDir()+"/cache",
		os.Getpid(),
		ranges,
	)
	require.NoError(t, err)

	t.Cleanup(func() {
		cache.Close()
	})

	for _, offset := range header.BlocksOffsets(size, pageSize) {
		buf := make([]byte, pageSize)
		n, err := cache.ReadAt(buf, offset)
		require.NoError(t, err)
		require.Equal(t, int(pageSize), n)
		require.NoError(t, compareData(buf, mem[offset:offset+pageSize]))
	}
}

func BenchmarkCopyFromHugepagesFile(b *testing.B) {
	pageSize := int64(header.HugepageSize)
	size := pageSize * 500

	b.StopTimer()
	for {
		l := int(math.Ceil(float64(size)/float64(pageSize)) * float64(pageSize))
		mem, err := syscall.Mmap(
			-1,
			0,
			l,
			syscall.PROT_READ|syscall.PROT_WRITE,
			syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS|unix.MAP_HUGETLB|unix.MAP_HUGE_2MB,
		)

		require.NoError(b, err)

		addr := uintptr(unsafe.Pointer(&mem[0]))

		n, err := rand.Read(mem)
		require.NoError(b, err)
		require.Equal(b, len(mem), n)

		var totalCovered int64
		numRanges := 40
		ranges := make([]Range, 0, numRanges)
		cur := int64(addr)

		for i := range numRanges {
			sizePages := int64(1 + (i % 5)) // pseudo-random but deterministic
			sizeR := sizePages * pageSize
			if totalCovered+sizeR > size*8/10 && i > 0 { // Stop if we have covered ~80% total
				break
			}
			ranges = append(ranges, Range{
				Start: cur,
				Size:  sizeR,
			})
			cur += sizeR + pageSize // GAP of 1 page between each range
			totalCovered += sizeR
		}

		pid := os.Getpid()

		filePath := b.TempDir() + "/cache"

		size := GetSize(ranges)

		cache, err := NewCache(size, header.PageSize, filePath, false)
		require.NoError(b, err)

		b.StartTimer()
		if !b.Loop() {
			b.StopTimer()
			err = cache.Close()
			require.NoError(b, err)
			err = syscall.Munmap(mem)
			require.NoError(b, err)

			break
		}

		err = cache.copyProcessMemory(b.Context(), pid, ranges, 0)
		require.NoError(b, err)

		b.StopTimer()

		err = cache.Close()
		require.NoError(b, err)

		err = syscall.Munmap(mem)
		require.NoError(b, err)

		b.SetBytes(GetSize(ranges))
	}
}

// sliceBase is a Slicer over an in-memory image, standing in for the memfile
// a sandbox was resumed from.
type sliceBase struct {
	data      []byte
	blockSize int64
	reads     []int64
}

func (s *sliceBase) Slice(_ context.Context, off, length int64) ([]byte, error) {
	s.reads = append(s.reads, off)

	return s.data[off : off+length], nil
}

func (s *sliceBase) BlockSize() int64 { return s.blockSize }

// The self-contained memfile layout: every included block, packed in block
// order, dirty ones from process memory and the rest from the base.
func TestNewCacheFromProcessMemoryAndBase_PacksProcessAndBaseBlocks(t *testing.T) {
	t.Parallel()

	blockSize := int64(header.PageSize)
	blocks := int64(10)

	addr, mem := allocateTestMemory(t, uint64(blocks*blockSize), uint64(blockSize))

	baseData := make([]byte, blocks*blockSize)
	_, err := rand.Read(baseData)
	require.NoError(t, err)

	base := &sliceBase{data: baseData, blockSize: blockSize}

	include := roaring.BitmapOf(0, 1, 3, 4, 5, 7, 8)
	// 9 is dirty but not included (the caller decided it is empty): it must
	// not be copied anywhere.
	fromProcess := roaring.BitmapOf(1, 4, 5, 8, 9)

	hostRanges := func(off, size int64) ([]Range, error) {
		return []Range{{Start: int64(addr) + off, Size: size}}, nil
	}

	cache, err := NewCacheFromProcessMemoryAndBase(
		t.Context(),
		blockSize,
		t.TempDir()+"/cache",
		os.Getpid(),
		include,
		fromProcess,
		hostRanges,
		base,
	)
	require.NoError(t, err)

	t.Cleanup(func() { cache.Close() })

	size, err := cache.Size()
	require.NoError(t, err)
	require.Equal(t, int64(include.GetCardinality())*blockSize, size)

	for i, idx := range include.ToArray() {
		src := baseData
		if fromProcess.Contains(idx) {
			src = mem
		}

		want := src[int64(idx)*blockSize : int64(idx+1)*blockSize]

		got, err := cache.Slice(int64(i)*blockSize, blockSize)
		require.NoError(t, err, "block %d (cache position %d) is not marked cached", idx, i)
		require.Equal(t, want, got, "block %d (cache position %d)", idx, i)
	}

	// Only the clean blocks are read from the base.
	require.Equal(t, []int64{0, 3 * blockSize, 7 * blockSize}, base.reads)
}

// A block that is neither in process memory nor readable from a base cannot be
// written, and saying so beats writing zeros.
func TestNewCacheFromProcessMemoryAndBase_NoBaseForCleanBlock(t *testing.T) {
	t.Parallel()

	blockSize := int64(header.PageSize)

	addr, _ := allocateTestMemory(t, uint64(4*blockSize), uint64(blockSize))

	path := t.TempDir() + "/cache"

	_, err := NewCacheFromProcessMemoryAndBase(
		t.Context(),
		blockSize,
		path,
		os.Getpid(),
		roaring.BitmapOf(0, 1),
		roaring.BitmapOf(0),
		func(off, size int64) ([]Range, error) {
			return []Range{{Start: int64(addr) + off, Size: size}}, nil
		},
		nil,
	)
	require.ErrorContains(t, err, "no base to read them from")

	_, statErr := os.Stat(path)
	require.ErrorIs(t, statErr, os.ErrNotExist, "a failed build must not leave its cache file behind")
}

// fileBase is a Slicer over a file, read through the page cache the way a
// local template-storage memfile is.
type fileBase struct {
	f         *os.File
	blockSize int64
	buf       []byte
}

func (s *fileBase) Slice(_ context.Context, off, length int64) ([]byte, error) {
	if int64(len(s.buf)) < length {
		s.buf = make([]byte, length)
	}

	n, err := s.f.ReadAt(s.buf[:length], off)
	if err != nil {
		return nil, err
	}

	return s.buf[:n], nil
}

func (s *fileBase) BlockSize() int64 { return s.blockSize }

// BenchmarkNewCacheFromProcessMemoryAndBase compares the two sources of a
// self-contained memfile per byte: blocks copied out of process memory (what
// every pause already does for dirty blocks) and blocks read from a cold base
// file (what a self-contained pause adds for clean ones). Run it with TMPDIR
// on the disk the orchestrator caches on; tmpfs measures nothing.
func BenchmarkNewCacheFromProcessMemoryAndBase(b *testing.B) {
	blockSize := int64(header.HugepageSize)
	blocks := int64(512) // 1 GiB
	size := blocks * blockSize

	mem, err := syscall.Mmap(-1, 0, int(size), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS)
	require.NoError(b, err)
	b.Cleanup(func() { _ = syscall.Munmap(mem) })

	_, err = rand.Read(mem)
	require.NoError(b, err)

	addr := int64(uintptr(unsafe.Pointer(&mem[0])))

	basePath := b.TempDir() + "/base"
	require.NoError(b, os.WriteFile(basePath, mem, 0o600))

	f, err := os.Open(basePath)
	require.NoError(b, err)
	b.Cleanup(func() { _ = f.Close() })

	all := roaring.New()
	all.AddRange(0, uint64(blocks))

	for _, tc := range []struct {
		name        string
		fromProcess *roaring.Bitmap
	}{
		{"process", all},
		{"cold-base", roaring.New()},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(size)

			for b.Loop() {
				b.StopTimer()
				require.NoError(b, f.Sync())
				require.NoError(b, unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED))
				path := b.TempDir() + "/cache"
				b.StartTimer()

				cache, err := NewCacheFromProcessMemoryAndBase(b.Context(), blockSize, path, os.Getpid(), all, tc.fromProcess,
					func(off, size int64) ([]Range, error) {
						return []Range{{Start: addr + off, Size: size}}, nil
					},
					&fileBase{f: f, blockSize: blockSize})
				require.NoError(b, err)

				b.StopTimer()
				require.NoError(b, cache.Close())
				b.StartTimer()
			}
		})
	}
}
