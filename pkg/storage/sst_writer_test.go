// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package storage

import (
	"context"
	"math/rand"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/clusterversion"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/util/encoding"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/randutil"
	"github.com/cockroachdb/pebble/sstable"
	"github.com/stretchr/testify/require"
)

func makeIntTableKVs(numKeys, valueSize, maxRevisions int) []MVCCKeyValue {
	prefix := encoding.EncodeUvarintAscending(keys.SystemSQLCodec.TablePrefix(uint32(100)), uint64(1))
	kvs := make([]MVCCKeyValue, numKeys)
	r, _ := randutil.NewTestRand()

	var k int
	for i := 0; i < numKeys; {
		k += 1 + rand.Intn(100)
		key := encoding.EncodeVarintAscending(append([]byte{}, prefix...), int64(k))
		buf := make([]byte, valueSize)
		randutil.ReadTestdataBytes(r, buf)
		revisions := 1 + r.Intn(maxRevisions)

		ts := int64(maxRevisions * 100)
		for j := 0; j < revisions && i < numKeys; j++ {
			ts -= 1 + r.Int63n(99)
			kvs[i].Key.Key = key
			kvs[i].Key.Timestamp.WallTime = ts
			kvs[i].Key.Timestamp.Logical = r.Int31()
			kvs[i].Value = roachpb.MakeValueFromString(string(buf)).RawBytes
			i++
		}
	}
	return kvs
}

func makePebbleSST(t testing.TB, kvs []MVCCKeyValue, ingestion bool) []byte {
	ctx := context.Background()
	st := cluster.MakeTestingClusterSettings()
	f := &MemFile{}
	var w SSTWriter
	if ingestion {
		w = MakeIngestionSSTWriter(ctx, st, f)
	} else {
		w = MakeBackupSSTWriter(ctx, st, f)
	}
	defer w.Close()

	for i := range kvs {
		if err := w.Put(kvs[i].Key, kvs[i].Value); err != nil {
			t.Fatal(err)
		}
	}
	err := w.Finish()
	require.NoError(t, err)
	return f.Data()
}

func TestMakeIngestionWriterOptions(t *testing.T) {
	defer leaktest.AfterTest(t)()

	testCases := []struct {
		name string
		st   *cluster.Settings
		want sstable.TableFormat
	}{
		{
			name: "before feature gate",
			st: cluster.MakeTestingClusterSettingsWithVersions(
				clusterversion.ByKey(clusterversion.EnablePebbleFormatVersionBlockProperties-1),
				clusterversion.TestingBinaryMinSupportedVersion,
				true,
			),
			want: sstable.TableFormatRocksDBv2,
		},
		{
			name: "at feature gate",
			st: cluster.MakeTestingClusterSettingsWithVersions(
				clusterversion.ByKey(clusterversion.EnablePebbleFormatVersionBlockProperties),
				clusterversion.TestingBinaryMinSupportedVersion,
				true,
			),
			want: sstable.TableFormatPebblev1,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			opts := MakeIngestionWriterOptions(ctx, tc.st)
			require.Equal(t, tc.want, opts.TableFormat)
		})
	}
}

func TestSSTWriterRangeKeysUnsupported(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()

	// Set up a version that doesn't support range keys.
	version := clusterversion.ByKey(clusterversion.EnsurePebbleFormatVersionRangeKeys - 1)
	st := cluster.MakeTestingClusterSettingsWithVersions(version, version, true)

	writers := map[string]SSTWriter{
		"ingestion": MakeIngestionSSTWriter(ctx, st, &MemFile{}),
		"backup":    MakeBackupSSTWriter(ctx, st, &MemFile{}),
	}

	for name, w := range writers {
		t.Run(name, func(t *testing.T) {
			defer w.Close()

			rangeKey := rangeKey("a", "b", 2)

			// Put should error, but clears are noops.
			err := w.PutMVCCRangeKey(rangeKey, MVCCValue{})
			require.Error(t, err)
			require.Contains(t, err.Error(), "range keys not supported")

			err = w.PutEngineRangeKey(rangeKey.StartKey, rangeKey.EndKey,
				EncodeMVCCTimestampSuffix(rangeKey.Timestamp), nil)
			require.Error(t, err)
			require.Contains(t, err.Error(), "range keys not supported")

			require.NoError(t, w.ClearMVCCRangeKey(rangeKey))
			require.NoError(t, w.ClearEngineRangeKey(rangeKey.StartKey, rangeKey.EndKey,
				EncodeMVCCTimestampSuffix(rangeKey.Timestamp)))
			require.NoError(t, w.ClearRawRange(rangeKey.StartKey, rangeKey.EndKey, false, true))
		})
	}
}

func TestSSTWriterRangeKeys(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	DisableMetamorphicSimpleValueEncoding(t)

	ctx := context.Background()
	st := cluster.MakeTestingClusterSettings()
	sstFile := &MemFile{}
	sst := MakeIngestionSSTWriter(ctx, st, sstFile)
	defer sst.Close()

	require.NoError(t, sst.Put(pointKey("a", 1), stringValueRaw("foo")))
	require.EqualValues(t, 9, sst.DataSize)

	require.NoError(t, sst.PutMVCCRangeKey(rangeKey("a", "e", 2), tombstoneLocalTS(1)))
	require.EqualValues(t, 20, sst.DataSize)

	require.NoError(t, sst.PutEngineRangeKey(roachpb.Key("f"), roachpb.Key("g"),
		wallTSRaw(2), tombstoneLocalTSRaw(1)))
	require.EqualValues(t, 31, sst.DataSize)

	require.NoError(t, sst.Finish())

	iter, err := NewPebbleMemSSTIterator(sstFile.Bytes(), false /* verify */, IterOptions{
		KeyTypes:   IterKeyTypePointsAndRanges,
		UpperBound: keys.MaxKey,
	})
	require.NoError(t, err)
	defer iter.Close()

	require.Equal(t, []interface{}{
		rangeKV("a", "e", 2, tombstoneLocalTS(1)),
		pointKV("a", 1, "foo"),
		rangeKV("f", "g", 2, tombstoneLocalTS(1)),
	}, scanIter(t, iter))
}

func BenchmarkWriteSSTable(b *testing.B) {
	b.StopTimer()
	// Writing the SST 10 times keeps size needed for ~10s benchtime under 1gb.
	const valueSize, revisions, ssts = 100, 100, 10
	kvs := makeIntTableKVs(b.N, valueSize, revisions)
	approxUserDataSizePerKV := kvs[b.N/2].Key.EncodedSize() + valueSize
	b.SetBytes(int64(approxUserDataSizePerKV * ssts))
	b.ResetTimer()
	b.StartTimer()
	for i := 0; i < ssts; i++ {
		_ = makePebbleSST(b, kvs, true /* ingestion */)
	}
	b.StopTimer()
}
