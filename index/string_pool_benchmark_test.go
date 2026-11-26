package index

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

var poolBenchmarkChecksum uint64
var poolRetentionRecords []Record

// Fresh backing allocations model independently parsed records. Low cardinality
// has 4 studies and 32 series, while high cardinality makes metadata unique.
// Both have unique instance UIDs. Synthetic values never come from patient data.
func poolBenchmarkRecord(i int, high bool) Record {
	r := poolFixture(fmt.Sprintf("1.2.826.0.1.3680043.10.999.%d", i))
	study, series := i%4, i%32
	if high {
		study, series = i, i
	}
	r.Study.InstanceUID = fmt.Sprintf("1.2.999.1.%d", study)
	r.Series.InstanceUID = fmt.Sprintf("1.2.999.2.%d", series)
	r.Study.Description = fmt.Sprintf("Síntese 合成 study %d", study)
	r.Series.Description = fmt.Sprintf("Synthétique series %d", series)
	r.Patient.ID = fmt.Sprintf("synthetic-%d", study)
	r.Patient.Name = fmt.Sprintf("Synthétique^患者%d", study)
	fields := []*string{&r.FileMeta.TransferSyntaxUID, &r.FileMeta.TransferSyntaxName, &r.FileMeta.MediaStorageSOPClassUID, &r.FileMeta.MediaStorageSOPInstanceUID, &r.Patient.Name, &r.Patient.ID, &r.Patient.BirthDate, &r.Patient.Sex, &r.Study.InstanceUID, &r.Study.AccessionNumber, &r.Study.Date, &r.Study.Time, &r.Study.Description, &r.Series.InstanceUID, &r.Series.Modality, &r.Series.Number, &r.Series.Date, &r.Series.Time, &r.Series.Description, &r.Instance.SOPClassUID, &r.Instance.SOPInstanceUID, &r.Instance.Number}
	for _, p := range fields {
		*p = strings.Clone(*p)
	}
	return r
}
func poolRecordChecksum(r Record) uint64 {
	return uint64(len(r.Study.Description) + len(r.Patient.Name) + len(r.Instance.SOPInstanceUID))
}

// The benchmark includes synthetic record creation for both strategies and all
// pooling/copy/Close costs. It is consumer-only, not scanner IO throughput.
func BenchmarkIndexStringPool(b *testing.B) {
	for _, distribution := range []string{"low", "high", "stream"} {
		for _, strategy := range []string{"baseline", "pool"} {
			b.Run(distribution+"/"+strategy, func(b *testing.B) {
				const batch = 4096
				high := distribution == "high"
				retain := distribution != "stream"
				usePool := strategy == "pool"
				// Full Record equality is verified before timing; checksum alone is no oracle.
				p, _ := NewStringPool(StringPoolOptions{})
				for i := 0; i < batch; i++ {
					r := poolBenchmarkRecord(i, high)
					got, err := p.ShareRecord(nil, r)
					if err != nil || !reflect.DeepEqual(r, got) {
						b.Fatal("synthetic content mismatch", err)
					}
				}
				_ = p.Close()
				b.ReportAllocs()
				b.ResetTimer()
				var total uint64
				for n := 0; n < b.N; n++ {
					var pool *StringPool
					if usePool {
						pool, _ = NewStringPool(StringPoolOptions{})
					}
					var records []Record
					if retain {
						records = make([]Record, 0, batch)
					}
					for i := 0; i < batch; i++ {
						r := poolBenchmarkRecord(i, high)
						if pool != nil {
							var err error
							r, err = pool.ShareRecord(nil, r)
							if err != nil {
								b.Fatal(err)
							}
						}
						total += poolRecordChecksum(r)
						if retain {
							records = append(records, r)
						}
					}
					if pool != nil {
						_ = pool.Close()
					}
					runtime.KeepAlive(records)
				}
				poolBenchmarkChecksum = total
			})
		}
	}
}

// This opt-in worker is run in a fresh process for each of six cells. Five
// repetitions are planned. HeapAlloc after GC is retained Go heap, not RSS.
// External /usr/bin/time -l measures process user/sys CPU and peak RSS separately.
func TestIndexStringPoolRetentionWorker(t *testing.T) {
	cell := os.Getenv("DICOM_INDEX_POOL_RETENTION_CELL")
	if cell == "" {
		t.Skip("opt-in six-cell fresh-process retention measurement")
	}
	valid := false
	for _, s := range []string{"low/baseline", "low/pool", "high/baseline", "high/pool", "stream/baseline", "stream/pool"} {
		if cell == s {
			valid = true
		}
	}
	if !valid {
		t.Fatal("invalid synthetic cell")
	}
	high := strings.HasPrefix(cell, "high/")
	retain := !strings.HasPrefix(cell, "stream/")
	usePool := strings.HasSuffix(cell, "/pool")
	const count = 32768
	var pool *StringPool
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	if usePool {
		pool, _ = NewStringPool(StringPoolOptions{MaxEntries: 16384, MaxBytes: 4 << 20})
	}
	if retain {
		poolRetentionRecords = make([]Record, 0, count)
	}
	var checksum uint64
	for i := 0; i < count; i++ {
		r := poolBenchmarkRecord(i, high)
		if pool != nil {
			var err error
			r, err = pool.ShareRecord(nil, r)
			if err != nil {
				t.Fatal(err)
			}
		}
		checksum += poolRecordChecksum(r)
		if retain {
			poolRetentionRecords = append(poolRetentionRecords, r)
		}
	}
	elapsed := time.Since(start).Nanoseconds()
	runtime.GC()
	var active runtime.MemStats
	runtime.ReadMemStats(&active)
	runtime.KeepAlive(pool)
	runtime.KeepAlive(poolRetentionRecords)
	stats := StringPoolStats{}
	if pool != nil {
		stats = pool.Stats()
		if stats.Entries > 16384 || stats.Bytes > 4<<20 {
			t.Fatal("retention budget")
		}
		_ = pool.Close()
		if pool.Stats().Entries != 0 || pool.Stats().Bytes != 0 {
			t.Fatal("close retained ownership")
		}
	}
	// Outputs remain valid after pool release, including all builtin Unicode data.
	if retain {
		for i, r := range poolRetentionRecords {
			if !reflect.DeepEqual(r, poolBenchmarkRecord(i, high)) {
				t.Fatal("retained content mismatch after close")
			}
		}
	}
	pool = nil
	runtime.GC()
	var closed runtime.MemStats
	runtime.ReadMemStats(&closed)
	runtime.KeepAlive(poolRetentionRecords)
	poolRetentionRecords = nil
	runtime.GC()
	var released runtime.MemStats
	runtime.ReadMemStats(&released)
	metric := struct {
		Cell                                                                string
		Records                                                             int
		ConstructionNS                                                      int64
		Checksum                                                            uint64
		AllocatedBytes, Mallocs                                             uint64
		ActiveHeapDelta, AfterPoolCloseHeapDelta, AfterOutputsDropHeapDelta int64
		Pool                                                                StringPoolStats
	}{cell, count, elapsed, checksum, active.TotalAlloc - before.TotalAlloc, active.Mallocs - before.Mallocs, int64(active.HeapAlloc) - int64(before.HeapAlloc), int64(closed.HeapAlloc) - int64(before.HeapAlloc), int64(released.HeapAlloc) - int64(before.HeapAlloc), stats}
	data, err := json.Marshal(metric)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("INDEX_POOL_METRIC %s\n", data)
}
