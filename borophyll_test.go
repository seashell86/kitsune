package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------
// Unit Tests for the CacheSystem
// ---------------------------------------------------------------

func TestCacheSystem_BasicOperations(t *testing.T) {
	cache := NewCacheSystem(1024, 10_000, 5, 999999) // Large enough; 5s TTL
	defer cache.Stop()

	// Check that retrieving a non-existent key returns empty string
	val := cache.Get("default", "non-existent")
	if val != "" {
		t.Fatalf("expected empty string for non-existent key, got %q", val)
	}

	// Insert a key, read it back
	cache.Set("default", "foo", "bar")
	val = cache.Get("default", "foo")
	if val != "bar" {
		t.Fatalf("expected 'bar', got %q", val)
	}

	// Overwrite a key, read it back
	cache.Set("default", "foo", "baz")
	val = cache.Get("default", "foo")
	if val != "baz" {
		t.Fatalf("expected 'baz' after overwrite, got %q", val)
	}

	// Delete the key, ensure it’s gone
	deletedVal := cache.Delete("default", "foo")
	if deletedVal != "baz" {
		t.Fatalf("expected 'baz' from Delete, got %q", deletedVal)
	}
	val = cache.Get("default", "foo")
	if val != "" {
		t.Fatalf("expected empty string after Delete, got %q", val)
	}
}

func TestCacheSystem_TTLExpiration(t *testing.T) {
	cache := NewCacheSystem(1024, 10_000, 1, 999999) // 1s TTL
	defer cache.Stop()

	cache.Set("bucket1", "key1", "value1")
	val := cache.Get("bucket1", "key1")
	if val != "value1" {
		t.Fatalf("expected 'value1', got %q", val)
	}

	// Wait for 2 seconds so the 1s TTL definitely expires
	time.Sleep(2 * time.Second)

	val = cache.Get("bucket1", "key1")
	if val != "" {
		t.Fatalf("expected entry to be expired and removed, got %q", val)
	}
}

func TestCacheSystem_MaxEntrySize(t *testing.T) {
	// Each entry can only be up to 10 bytes
	cache := NewCacheSystem(10, 1000, 60, 999999)
	defer cache.Stop()

	// "1234567890" -> 10 bytes exactly
	cache.Set("bucket", "key10", "1234567890")
	got := cache.Get("bucket", "key10")
	if got != "1234567890" {
		t.Fatalf("expected to store '1234567890', got %q", got)
	}

	// This one is 11 bytes -> too large
	cache.Set("bucket", "key11", "12345678901")
	got = cache.Get("bucket", "key11")
	if got != "" {
		t.Fatalf("expected nothing because the entry is oversized, got %q", got)
	}
}

func TestCacheSystem_MaxTotalSize(t *testing.T) {
	// Max total size = 50 bytes
	cache := NewCacheSystem(25, 50, 60, 999999)
	defer cache.Stop()

	// Insert multiple entries
	// key/val each ~10 bytes => total ~20-25 bytes
	cache.Set("b", "k1", "0123456789") // ~ 11 bytes plus bucket "b" + key "k1" => ~14 bytes
	cache.Set("b", "k2", "abcdefghij") // also ~14 bytes
	cache.Set("b", "k3", "K333333333") // also ~14 bytes

	// We have 3 entries, each ~14 bytes => total ~42 bytes. That’s < 50, so probably all still there.
	val1 := cache.Get("b", "k1")
	val2 := cache.Get("b", "k2")
	val3 := cache.Get("b", "k3")
	if val1 == "" || val2 == "" || val3 == "" {
		t.Fatalf("expected all three entries to remain, got k1=%q, k2=%q, k3=%q", val1, val2, val3)
	}

	// Insert a new entry that pushes total size > 50 => triggers eviction of the LRU
	cache.Set("b", "k4", "XYZXYZXYZX") // ~14 bytes
	// The new entry k4 is now MRU, so the oldest (LRU) among k1, k2, k3 should be evicted
	// The order of LRU depends on usage, but let's see if "k1" was the first inserted => it might get evicted.

	val4 := cache.Get("b", "k4")
	if val4 != "XYZXYZXYZX" {
		t.Fatalf("expected 'XYZXYZXYZX' for k4, got %q", val4)
	}

	// By default, the least recently used is k1 (since we've never re-GET them).
	val1 = cache.Get("b", "k1")
	val2 = cache.Get("b", "k2")
	val3 = cache.Get("b", "k3")

	var survivors int
	if val1 != "" {
		survivors++
	}
	if val2 != "" {
		survivors++
	}
	if val3 != "" {
		survivors++
	}

	if survivors != 2 {
		t.Fatalf("expected exactly 2 survivors after insertion of k4, found %d", survivors)
	}
}

func TestCacheSystem_ClearBucket(t *testing.T) {
	cache := NewCacheSystem(1024, 999999, 60, 999999)
	defer cache.Stop()

	cache.Set("b1", "k1", "v1")
	cache.Set("b1", "k2", "v2")
	cache.Set("b2", "x1", "y1")

	cache.Clear("b1")

	if got := cache.Get("b1", "k1"); got != "" {
		t.Fatalf("expected empty after clearing, got %q", got)
	}
	if got := cache.Get("b1", "k2"); got != "" {
		t.Fatalf("expected empty after clearing, got %q", got)
	}
	// b2 is untouched
	if got := cache.Get("b2", "x1"); got != "y1" {
		t.Fatalf("expected 'y1' in b2, got %q", got)
	}
}

func TestCacheSystem_ClearAll(t *testing.T) {
	cache := NewCacheSystem(1024, 999999, 60, 999999)
	defer cache.Stop()

	cache.Set("b1", "k1", "v1")
	cache.Set("b2", "x1", "y1")
	cache.ClearAll()

	if got := cache.Get("b1", "k1"); got != "" {
		t.Fatalf("expected empty after clearing all, got %q", got)
	}
	if got := cache.Get("b2", "x1"); got != "" {
		t.Fatalf("expected empty after clearing all, got %q", got)
	}
}

func TestCacheSystem_ConcurrentAccess(t *testing.T) {
	cache := NewCacheSystem(1024, 999999, 60, 999999)
	defer cache.Stop()

	var wg sync.WaitGroup
	var ops int64

	for i := 0; i < 10; i++ {
		// Writers
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for k := 0; k < 100; k++ {
				cache.Set("bucket", "key-"+strconv.Itoa(k), "val-"+strconv.Itoa(k))
				atomic.AddInt64(&ops, 1)
			}
		}(i)

		// Readers
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for k := 0; k < 100; k++ {
				_ = cache.Get("bucket", "key-"+strconv.Itoa(k))
				atomic.AddInt64(&ops, 1)
			}
		}(i)
	}

	wg.Wait()
	if ops < 1 {
		t.Fatalf("expected some operations, got %d", ops)
	}
	// Just ensure no data race or panic occurred
}

func TestCacheSystem_LazyExpirationOnGet(t *testing.T) {
	// small TTL to ensure we can quickly test lazy expiration
	cache := NewCacheSystem(1024, 999999, 1, 999999)
	defer cache.Stop()

	cache.Set("b", "k", "v")
	time.Sleep(2 * time.Second) // wait beyond the 1s TTL

	// Access it => should see empty string, and it should be removed from the map
	val := cache.Get("b", "k")
	if val != "" {
		t.Fatalf("expected empty due to lazy expiration, got %q", val)
	}

	// Confirm it’s truly gone
	if val2 := cache.Get("b", "k"); val2 != "" {
		t.Fatalf("expected empty on subsequent get, got %q", val2)
	}
}

// ---------------------------------------------------------------
// Integration Tests for the HTTP Endpoints
// ---------------------------------------------------------------

func TestHTTP_Integration_DefaultKeyspace(t *testing.T) {
	// TTL=60, maxEntrySize=1MB, maxSize=10MB, cleanup=999999 => no forced eviction
	cache := NewCacheSystem(1_000_000, 10_000_000, 60, 999999)
	defer cache.Stop()

	handler := createHandler(cache, "__root__")
	server := httptest.NewServer(handler)
	defer server.Close()

	// GET / => health check
	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatalf("GET / => unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / => expected 200, got %d", resp.StatusCode)
	}
	var health map[string]string
	if err = json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatalf("GET / => decoding error: %v", err)
	}
	if health["status"] != "healthy" {
		t.Fatalf("GET / => expected healthy, got %q", health["status"])
	}

	// GET /keys/non-existent => should return {"value":""}
	resp, err = http.Get(server.URL + "/keys/non-existent")
	if err != nil {
		t.Fatalf("GET /keys/non-existent => %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /keys/non-existent => expected 200, got %d", resp.StatusCode)
	}
	var getRes map[string]string
	if err = json.NewDecoder(resp.Body).Decode(&getRes); err != nil {
		t.Fatalf("GET /keys/non-existent => decode err: %v", err)
	}
	if getRes["value"] != "" {
		t.Fatalf("expected empty string for non-existent key, got %q", getRes["value"])
	}

	// PUT /keys/foo => {"value":"bar"}
	body := []byte(`{"value":"bar"}`)
	resp, err = httpPut(server.URL+"/keys/foo", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("PUT /keys/foo => %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /keys/foo => expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// GET /keys/foo => {"value":"bar"}
	resp, err = http.Get(server.URL + "/keys/foo")
	if err != nil {
		t.Fatalf("GET /keys/foo => %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /keys/foo => expected 200, got %d", resp.StatusCode)
	}
	if err = json.NewDecoder(resp.Body).Decode(&getRes); err != nil {
		t.Fatalf("GET /keys/foo => decode err: %v", err)
	}
	if getRes["value"] != "bar" {
		t.Fatalf("expected 'bar', got %q", getRes["value"])
	}

	// DELETE /keys/foo
	req, err := http.NewRequest(http.MethodDelete, server.URL+"/keys/foo", nil)
	if err != nil {
		t.Fatalf("DELETE /keys/foo => request creation failed: %v", err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /keys/foo => %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /keys/foo => expected 200, got %d", resp.StatusCode)
	}

	// GET /keys/foo => should be empty
	resp, err = http.Get(server.URL + "/keys/foo")
	if err != nil {
		t.Fatalf("GET /keys/foo => %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /keys/foo => expected 200, got %d", resp.StatusCode)
	}
	if err = json.NewDecoder(resp.Body).Decode(&getRes); err != nil {
		t.Fatalf("GET /keys/foo => decode err: %v", err)
	}
	if getRes["value"] != "" {
		t.Fatalf("expected empty string after delete, got %q", getRes["value"])
	}
}

func TestHTTP_Integration_Buckets(t *testing.T) {
	cache := NewCacheSystem(1_000_000, 10_000_000, 60, 999999)
	defer cache.Stop()

	handler := createHandler(cache, "default_bucket") // custom default
	server := httptest.NewServer(handler)
	defer server.Close()

	// PUT /buckets/foo/key => set "bar"
	body := []byte(`{"value":"bar"}`)
	resp, err := httpPut(server.URL+"/buckets/foo/key", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("PUT /buckets/foo/key => %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /buckets/foo/key => expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// GET /buckets/foo/key => should be {"value":"bar"}
	resp, err = http.Get(server.URL + "/buckets/foo/key")
	if err != nil {
		t.Fatalf("GET /buckets/foo/key => %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /buckets/foo/key => expected 200, got %d", resp.StatusCode)
	}
	var getRes map[string]string
	if err = json.NewDecoder(resp.Body).Decode(&getRes); err != nil {
		t.Fatalf("GET /buckets/foo/key => decode error: %v", err)
	}
	if getRes["value"] != "bar" {
		t.Fatalf("expected 'bar', got %q", getRes["value"])
	}

	// GET /buckets/foo => should be {"count":1}
	resp, err = http.Get(server.URL + "/buckets/foo")
	if err != nil {
		t.Fatalf("GET /buckets/foo => %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /buckets/foo => expected 200, got %d", resp.StatusCode)
	}
	var countRes map[string]int
	if err = json.NewDecoder(resp.Body).Decode(&countRes); err != nil {
		t.Fatalf("GET /buckets/foo => decode error: %v", err)
	}
	if countRes["count"] != 1 {
		t.Fatalf("expected count=1, got %d", countRes["count"])
	}

	// DELETE /buckets/foo/key => remove it
	req, err := http.NewRequest(http.MethodDelete, server.URL+"/buckets/foo/key", nil)
	if err != nil {
		t.Fatalf("DELETE /buckets/foo/key => req creation error: %v", err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /buckets/foo/key => %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /buckets/foo/key => expected 200, got %d", resp.StatusCode)
	}

	// GET /buckets/foo/key => empty
	resp, err = http.Get(server.URL + "/buckets/foo/key")
	if err != nil {
		t.Fatalf("GET /buckets/foo/key => %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /buckets/foo/key => expected 200, got %d", resp.StatusCode)
	}
	if err = json.NewDecoder(resp.Body).Decode(&getRes); err != nil {
		t.Fatalf("GET /buckets/foo/key => decode error: %v", err)
	}
	if getRes["value"] != "" {
		t.Fatalf("expected empty after delete, got %q", getRes["value"])
	}

	// DELETE /buckets/foo => remove entire bucket
	body = []byte(`{"value":"something"}`)
	_, _ = httpPut(server.URL+"/buckets/foo/k1", "application/json", bytes.NewReader(body))
	_, _ = httpPut(server.URL+"/buckets/foo/k2", "application/json", bytes.NewReader(body))
	req, err = http.NewRequest(http.MethodDelete, server.URL+"/buckets/foo", nil)
	if err != nil {
		t.Fatalf("DELETE /buckets/foo => req creation error: %v", err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /buckets/foo => %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /buckets/foo => expected 200, got %d", resp.StatusCode)
	}

	// GET /buckets/foo => count=0
	resp, err = http.Get(server.URL + "/buckets/foo")
	if err != nil {
		t.Fatalf("GET /buckets/foo => %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /buckets/foo => expected 200, got %d", resp.StatusCode)
	}
	if err = json.NewDecoder(resp.Body).Decode(&countRes); err != nil {
		t.Fatalf("GET /buckets/foo => decode error: %v", err)
	}
	if countRes["count"] != 0 {
		t.Fatalf("expected 0 after clearing the bucket, got %d", countRes["count"])
	}

	// DELETE /buckets => remove ALL buckets
	_, _ = httpPut(server.URL+"/buckets/a/1", "application/json", bytes.NewReader(body))
	_, _ = httpPut(server.URL+"/buckets/b/2", "application/json", bytes.NewReader(body))
	req, err = http.NewRequest(http.MethodDelete, server.URL+"/buckets", nil)
	if err != nil {
		t.Fatalf("DELETE /buckets => request creation error: %v", err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /buckets => %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /buckets => expected 200, got %d", resp.StatusCode)
	}

	// Check that they are gone
	resp, err = http.Get(server.URL + "/buckets/a/1")
	if err != nil {
		t.Fatalf("GET /buckets/a/1 => %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /buckets/a/1 => expected 200, got %d", resp.StatusCode)
	}
	if err = json.NewDecoder(resp.Body).Decode(&getRes); err != nil {
		t.Fatalf("GET /buckets/a/1 => decode error: %v", err)
	}
	if getRes["value"] != "" {
		t.Fatalf("expected empty after clearing all buckets, got %q", getRes["value"])
	}
}

func TestHTTP_Integration_Expiration(t *testing.T) {
	// Very short TTL => 1s
	cache := NewCacheSystem(1_000_000, 10_000_000, 1, 999999)
	defer cache.Stop()

	handler := createHandler(cache, "__root__")
	server := httptest.NewServer(handler)
	defer server.Close()

	// PUT /keys/foo => bar
	body := []byte(`{"value":"bar"}`)
	resp, err := httpPut(server.URL+"/keys/foo", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("PUT => %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT => expected 200, got %d", resp.StatusCode)
	}

	// Immediately GET => should see "bar"
	resp, err = http.Get(server.URL + "/keys/foo")
	if err != nil {
		t.Fatalf("GET => %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET => expected 200, got %d", resp.StatusCode)
	}
	var getRes map[string]string
	if err = json.NewDecoder(resp.Body).Decode(&getRes); err != nil {
		t.Fatalf("decode => %v", err)
	}
	if getRes["value"] != "bar" {
		t.Fatalf("expected 'bar', got %q", getRes["value"])
	}

	// Wait >1s
	time.Sleep(2 * time.Second)

	// GET again => should be expired
	resp, err = http.Get(server.URL + "/keys/foo")
	if err != nil {
		t.Fatalf("GET => %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET => expected 200, got %d", resp.StatusCode)
	}
	if err = json.NewDecoder(resp.Body).Decode(&getRes); err != nil {
		t.Fatalf("decode => %v", err)
	}
	if getRes["value"] != "" {
		t.Fatalf("expected empty, got %q", getRes["value"])
	}
}

// ---------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------

// httpPut is a convenience for making PUT requests with a body
func httpPut(url, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPut, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	return http.DefaultClient.Do(req)
}
