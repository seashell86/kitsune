package main

import (
	"container/list"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"sync"
	"time"
)

const (
	DEFAULT_TTL              = 60 * 60
	DEFAULT_MAX_ENTRY_SIZE   = math.MaxInt64
	DEFAULT_MAX_SIZE         = math.MaxInt64
	DEFAULT_CLEANUP_INTERVAL = 300
	DEFAULT_KEYSPACE         = "__root__"
)

var cacheEntryPool = sync.Pool{
	New: func() interface{} {
		return new(CacheEntry)
	},
}

// CacheEntry holds an individual item in the cache.
type CacheEntry struct {
	Bucket     string
	Key        string
	Value      string
	Expiration time.Time
	Size       int
}

// IsExpired returns true if the entry is beyond its Expiration.
func (ce *CacheEntry) IsExpired() bool {
	return time.Now().After(ce.Expiration)
}

// reset clears the CacheEntry fields so they can be reused safely.
func (ce *CacheEntry) reset() {
	ce.Bucket = ""
	ce.Key = ""
	ce.Value = ""
	ce.Size = 0
	ce.Expiration = time.Time{}
}

// CacheSystem manages all in-memory buckets and entries.
type CacheSystem struct {
	mu              sync.RWMutex
	entries         *list.List                     // Doubly linked list for LRU ordering: front=MRU, back=LRU
	items           map[[2]string]*list.Element    // (bucket,key) => list element
	buckets         map[string]map[string]struct{} // bucket => set of keys
	maxEntrySize    int64
	maxSize         int64
	ttl             time.Duration
	cleanupInterval time.Duration

	currentSize int64

	// For background cleanup
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewCacheSystem creates a new CacheSystem with the given parameters.
func NewCacheSystem(maxEntrySize, maxSize, ttl, cleanupInterval int64) *CacheSystem {
	if maxEntrySize <= 0 {
		maxEntrySize = DEFAULT_MAX_ENTRY_SIZE
	}
	if maxSize <= 0 {
		maxSize = DEFAULT_MAX_SIZE
	}
	if maxSize < maxEntrySize {
		maxSize = maxEntrySize
	}
	if ttl < 0 {
		ttl = 0
	}
	if cleanupInterval <= 0 {
		cleanupInterval = 1
	}

	cs := &CacheSystem{
		entries:         list.New(),
		items:           make(map[[2]string]*list.Element),
		buckets:         make(map[string]map[string]struct{}),
		maxEntrySize:    maxEntrySize,
		maxSize:         maxSize,
		ttl:             time.Duration(ttl) * time.Second,
		cleanupInterval: time.Duration(cleanupInterval) * time.Second,
		stopCh:          make(chan struct{}),
	}

	cs.wg.Add(1)
	go cs.expirationLoop()

	return cs
}

// Stop signals the background cleanup goroutine to exit.
func (cs *CacheSystem) Stop() {
	close(cs.stopCh)
	cs.wg.Wait()
}

// expirationLoop periodically evicts expired entries.
func (cs *CacheSystem) expirationLoop() {
	defer cs.wg.Done()
	ticker := time.NewTicker(cs.cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-cs.stopCh:
			return
		case <-ticker.C:
			cs.cleanupExpired()
		}
	}
}

// cleanupExpired removes entries whose TTL has expired.
func (cs *CacheSystem) cleanupExpired() {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	for e := cs.entries.Back(); e != nil; {
		entry := e.Value.(*CacheEntry)
		if entry.IsExpired() {
			prev := e.Prev()
			cs.removeElement(e)
			e = prev
		} else {
			e = e.Prev()
		}
	}
}

// enforceSizeLimit evicts from the LRU side until currentSize <= maxSize.
func (cs *CacheSystem) enforceSizeLimit() {
	for cs.currentSize > cs.maxSize && cs.entries.Len() > 0 {
		evictElem := cs.entries.Back()
		cs.removeElement(evictElem)
	}
}

// removeElement is an internal helper to remove a *list.Element (CacheEntry) from the list.
func (cs *CacheSystem) removeElement(elem *list.Element) {
	entry := elem.Value.(*CacheEntry)
	cs.entries.Remove(elem)
	delete(cs.items, [2]string{entry.Bucket, entry.Key})
	cs.currentSize -= int64(entry.Size)

	if setOfKeys, ok := cs.buckets[entry.Bucket]; ok {
		delete(setOfKeys, entry.Key)
		if len(setOfKeys) == 0 {
			delete(cs.buckets, entry.Bucket)
		}
	}

	// Wipe fields, then return the entry to the pool.
	entry.reset()
	cacheEntryPool.Put(entry)
}

// Get returns the value from the cache if present and not expired.
// Moves the entry to the front (MRU) if found and valid.
func (cs *CacheSystem) Get(bucket, key string) string {
	cs.mu.RLock()
	elem, found := cs.items[[2]string{bucket, key}]
	cs.mu.RUnlock()

	if !found {
		return ""
	}

	cs.mu.Lock()
	defer cs.mu.Unlock()

	// double-check existence & expiration
	if elem2, stillFound := cs.items[[2]string{bucket, key}]; !stillFound || elem2 != elem {
		// it was removed between RUnlock and Lock
		return ""
	}
	entry := elem.Value.(*CacheEntry)
	if entry.IsExpired() {
		cs.removeElement(elem)
		return ""
	}

	// Move to the front (MRU)
	cs.entries.MoveToFront(elem)
	return entry.Value
}

// Set inserts or updates an entry, respecting the maxEntrySize, maxSize, and TTL.
func (cs *CacheSystem) Set(bucket, key, value string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	compositeKey := [2]string{bucket, key}
	// If it already exists, remove it first so we can reinsert a fresh one.
	if elem, found := cs.items[compositeKey]; found {
		cs.removeElement(elem)
	}

	// Instead of creating a new CacheEntry, grab one from the pool.
	entry := cacheEntryPool.Get().(*CacheEntry)
	// Fill in the new data
	entry.Bucket = bucket
	entry.Key = key
	entry.Value = value
	entry.Expiration = time.Now().Add(cs.ttl)
	entry.Size = len(bucket) + len(key) + len(value)

	// Compare just the value size to maxEntrySize
	if int64(len(value)) > cs.maxEntrySize {
		// Return entry to pool and exit (too large)
		entry.reset()
		cacheEntryPool.Put(entry)
		return
	}

	elem := cs.entries.PushFront(entry)
	cs.items[compositeKey] = elem
	cs.currentSize += int64(entry.Size)

	// Bucket set
	if _, ok := cs.buckets[bucket]; !ok {
		cs.buckets[bucket] = make(map[string]struct{})
	}
	cs.buckets[bucket][key] = struct{}{}

	// Evict if over max size
	cs.enforceSizeLimit()
}

// Delete removes the entry with the given bucket/key, returning its value.
func (cs *CacheSystem) Delete(bucket, key string) string {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	compositeKey := [2]string{bucket, key}
	elem, found := cs.items[compositeKey]
	if !found {
		return ""
	}
	entry := elem.Value.(*CacheEntry)
	val := entry.Value
	cs.removeElement(elem)
	return val
}

// Clear removes all entries in a particular bucket.
func (cs *CacheSystem) Clear(bucket string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	keysSet, ok := cs.buckets[bucket]
	if !ok {
		return
	}
	for k := range keysSet {
		if elem, found := cs.items[[2]string{bucket, k}]; found {
			cs.removeElement(elem)
		}
	}
	delete(cs.buckets, bucket)
}

// ClearAll removes every entry in the cache.
func (cs *CacheSystem) ClearAll() {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	// We need to move through the list and return each entry to the pool
	for e := cs.entries.Front(); e != nil; {
		next := e.Next()
		entry := e.Value.(*CacheEntry)
		cs.entries.Remove(e)
		delete(cs.items, [2]string{entry.Bucket, entry.Key})
		entry.reset()
		cacheEntryPool.Put(entry)
		e = next
	}

	cs.items = make(map[[2]string]*list.Element)
	cs.buckets = make(map[string]map[string]struct{})
	cs.currentSize = 0
}

// GetBucketSize returns how many keys a given bucket has.
func (cs *CacheSystem) GetBucketSize(bucket string) int {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	if keysSet, ok := cs.buckets[bucket]; ok {
		return len(keysSet)
	}
	return 0
}

type putBucketKeyRequest struct {
	Value string `json:"value"`
}

func createHandler(cache *CacheSystem, defaultKeyspace string) http.Handler {
	mux := http.NewServeMux()

	// Health check: GET /
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
		} else {
			http.NotFound(w, r)
		}
	})

	// Keys in the default keyspace: GET/PUT/DELETE /keys/{key}
	mux.HandleFunc("/keys/", func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) <= len("/keys/") {
			http.NotFound(w, r)
			return
		}
		key := r.URL.Path[len("/keys/"):]
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			val := cache.Get(defaultKeyspace, key)
			_ = json.NewEncoder(w).Encode(map[string]string{"value": val})
		case http.MethodPut:
			var req putBucketKeyRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			cache.Set(defaultKeyspace, key, req.Value)
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			cache.Delete(defaultKeyspace, key)
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		}
	})

	// Buckets:
	//   GET /buckets/{bucket} => {"count": n}
	//   DELETE /buckets/{bucket} => clear the bucket
	//   GET /buckets/{bucket}/{key}
	//   PUT /buckets/{bucket}/{key}
	//   DELETE /buckets/{bucket}/{key}
	//   DELETE /buckets => clear all buckets
	mux.HandleFunc("/buckets", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/buckets" {
			if r.Method == http.MethodDelete {
				cache.ClearAll()
				w.WriteHeader(http.StatusOK)
				return
			}
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
	})

	mux.HandleFunc("/buckets/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path[len("/buckets/"):]

		// If path is empty => "/buckets/"
		if path == "" {
			http.NotFound(w, r)
			return
		}

		// If there's no slash, it's "/buckets/{bucket}"
		var bucket, key string
		slashIndex := -1
		for i := 0; i < len(path); i++ {
			if path[i] == '/' {
				slashIndex = i
				break
			}
		}

		if slashIndex < 0 {
			bucket = path
			switch r.Method {
			case http.MethodGet:
				w.Header().Set("Content-Type", "application/json")
				count := cache.GetBucketSize(bucket)
				_ = json.NewEncoder(w).Encode(map[string]int{"count": count})
			case http.MethodDelete:
				cache.Clear(bucket)
				w.WriteHeader(http.StatusOK)
			default:
				http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			}
			return
		}

		bucket = path[:slashIndex]
		key = path[slashIndex+1:]

		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			val := cache.Get(bucket, key)
			_ = json.NewEncoder(w).Encode(map[string]string{"value": val})
		case http.MethodPut:
			var req putBucketKeyRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			cache.Set(bucket, key, req.Value)
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			cache.Delete(bucket, key)
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		}
	})

	return mux
}

func main() {
	// Remove environment variable defaults and simplify to just flags
	hostFlag := flag.String("host", "0.0.0.0", "Host to bind")
	portFlag := flag.Int64("port", 42069, "Port to bind")
	maxEntrySizeFlag := flag.Int64("max-entry-size", DEFAULT_MAX_ENTRY_SIZE, "Max entry size (bytes)")
	maxSizeFlag := flag.Int64("max-size", DEFAULT_MAX_SIZE, "Max total cache size (bytes)")
	ttlFlag := flag.Int64("ttl", DEFAULT_TTL, "Default TTL in seconds")
	cleanupFlag := flag.Int64("cleanup-interval", DEFAULT_CLEANUP_INTERVAL, "Cleanup interval in seconds")
	defaultKeyspaceFlag := flag.String("default-keyspace", DEFAULT_KEYSPACE, "Default keyspace")
	flag.Parse()

	cache := NewCacheSystem(*maxEntrySizeFlag, *maxSizeFlag, *ttlFlag, *cleanupFlag)
	defer cache.Stop() // Cleanly stop background goroutine when the server exits

	// Log configuration information
	log.Printf("Configuration:")
	log.Printf("  Host: %s", *hostFlag)
	log.Printf("  Port: %d", *portFlag)
	log.Printf("  Max Entry Size: %d bytes", *maxEntrySizeFlag)
	log.Printf("  Max Total Cache Size: %d bytes", *maxSizeFlag)
	log.Printf("  TTL: %d seconds", *ttlFlag)
	log.Printf("  Cleanup Interval: %d seconds", *cleanupFlag)
	log.Printf("  Default Keyspace: %s", *defaultKeyspaceFlag)

	handler := createHandler(cache, *defaultKeyspaceFlag)

	addr := fmt.Sprintf("%s:%d", *hostFlag, *portFlag)
	log.Printf("Starting server on %s ...\n", addr)
	log.Fatal(http.ListenAndServe(addr, handler))
}
