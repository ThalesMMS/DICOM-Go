package icc

import (
	"errors"
	"sync"
	"testing"
)

func TestTransformCacheContentReuseBoundedAdmissionAndOwnership(t *testing.T) {
	data := AdobeRGB1998.Build()
	weight := int64(len(data))*64 + (64 << 10)
	cache := NewTransformCache(CacheOptions{MaxEntries: 2, MaxBytes: 2 * weight, MaxProfileBytes: len(data)})
	var wg sync.WaitGroup
	results := make([]CachedTransform, 8)
	for n := range results {
		wg.Add(1)
		go func() { defer wg.Done(); results[n] = cache.Resolve(append([]byte(nil), data...)) }()
	}
	wg.Wait()
	for _, r := range results {
		if r.Err != nil || r.Transform != results[0].Transform {
			t.Fatalf("content not shared: %+v", r)
		}
	}
	if stats := cache.Stats(); stats.Builds != 1 || stats.Entries != 1 || stats.Bytes != weight {
		t.Fatalf("stats=%+v", stats)
	}
	beforeR, beforeG, beforeB := results[0].Transform.Convert(.8, .4, .2)
	for n := range data {
		data[n] = 0
	}
	afterR, afterG, afterB := results[0].Transform.Convert(.8, .4, .2)
	if beforeR != afterR || beforeG != afterG || beforeB != afterB {
		t.Fatal("transform borrowed caller input")
	}
	for n := 1; n <= 3; n++ {
		cache.Resolve([]byte{byte(n)})
	}
	stats := cache.Stats()
	if stats.Entries > 2 || stats.Bytes > 2*weight {
		t.Fatalf("unbounded stats=%+v", stats)
	}
	if r := cache.Resolve(make([]byte, len(data)+1)); !errors.Is(r.Err, ErrCacheResourceLimit) {
		t.Fatalf("oversize=%+v", r)
	}
	cache.Clear()
	if cache.Stats().Entries != 0 || cache.Stats().Bytes != 0 {
		t.Fatal("clear retained entries")
	}
}
