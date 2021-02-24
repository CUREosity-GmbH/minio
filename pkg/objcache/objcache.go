/*
 * Minio Cloud Storage, (C) 2021 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

// Package objcache implements in memory caching methods.
package objcache

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/valyala/bytebufferpool"
)

const (
	// NoExpiry represents caches to be permanent and can only be deleted.
	NoExpiry = time.Duration(0)

	// DefaultExpiry represents 1 hour time duration when all entries shall be expired.
	DefaultExpiry = time.Hour

	// defaultBufferRatio represents default ratio used to calculate the
	// individual cache entry buffer size.
	defaultBufferRatio = uint64(10)
)

var (
	// ErrKeyNotFoundInCache - key not found in cache.
	ErrKeyNotFoundInCache = errors.New("Key not found in cache")

	// ErrCacheFull - cache is full.
	ErrCacheFull = errors.New("Not enough space in cache")

	// ErrExcessData - excess data was attempted to be written on cache.
	ErrExcessData = errors.New("Attempted excess write on cache")
)

// buffer represents the in memory cache of a single entry.
// buffer carries value of the data and last accessed time.
type buffer struct {
	buf          *bytebufferpool.ByteBuffer
	lastAccessed time.Time // Represents time when value was last accessed.
}

// Cache holds the required variables to compose an in memory cache system
// which also provides expiring key mechanism and also maxSize.
type Cache struct {
	// Mutex is used for handling the concurrent
	// read/write requests for cache
	mutex sync.Mutex

	// Once is used for resetting GC once after
	// peak cache usage.
	onceGC sync.Once

	// maxSize is a total size for overall cache
	maxSize uint64

	// maxCacheEntrySize is a total size per key buffer.
	maxCacheEntrySize uint64

	// currentSize is a current size in memory
	currentSize uint64

	// OnEviction - callback function for eviction
	OnEviction func(key string)

	// totalEvicted counter to keep track of total expirys
	totalEvicted int

	// map of cached keys and its values
	entries map[string]*buffer

	// Expiry in time duration.
	expiry time.Duration

	// Stop garbage collection routine, stops any running GC routine.
	stopGC chan struct{}
}

// New - Return a new cache with a given default expiry
// duration. If the expiry duration is less than one
// (or NoExpiry), the items in the cache never expire
// (by default), and must be deleted manually.
func New(maxSize uint64, expiry time.Duration) (c *Cache, err error) {
	if maxSize == 0 {
		err = errors.New("invalid maximum cache size")
		return c, err
	}

	// Max cache entry size - indicates the
	// maximum buffer per key that can be held in
	// memory. Currently this value is 1/10th
	// the size of requested cache size.
	maxCacheEntrySize := func() uint64 {
		i := maxSize / defaultBufferRatio
		if i == 0 {
			i = maxSize
		}
		return i
	}()

	c = &Cache{
		onceGC:            sync.Once{},
		maxSize:           maxSize,
		maxCacheEntrySize: maxCacheEntrySize,
		entries:           make(map[string]*buffer),
		expiry:            expiry,
	}

	// We have expiry start the janitor routine.
	if expiry > 0 {
		// Initialize a new stop GC channel.
		c.stopGC = make(chan struct{})

		// Start garbage collection routine to expire objects.
		c.StartGC()
	}

	return c, nil
}

// Create - validates if object size fits with in cache size limit and returns a io.WriteCloser
// to which object contents can be written and finally Close()'d. During Close() we
// checks if the amount of data written is equal to the size of the object, in which
// case it saves the contents to object cache.
func (c *Cache) Create(key string) (wc io.WriteCloser) {
	buf := bytebufferpool.Get()

	// Function called on close which saves the object contents
	// to the object cache.
	onClose := func() error {
		c.mutex.Lock()
		defer c.mutex.Unlock()

		if buf.Len() == 0 {
			buf.Reset()
			bytebufferpool.Put(buf)

			// If nothing is written in the buffer
			// the key is not stored.
			return nil
		}

		if uint64(buf.Len()) > c.maxCacheEntrySize {
			buf.Reset()
			bytebufferpool.Put(buf)

			return ErrCacheFull
		}

		// Full object available in buf, save it to cache.
		c.entries[key] = &buffer{
			buf:          buf,
			lastAccessed: time.Now().UTC(), // Save last accessed time.
		}

		// Account for the memory allocated above.
		c.currentSize += uint64(buf.Len())
		return nil
	}

	return &writeCloser{ByteBuffer: buf, onClose: onClose}
}

// Open - open the in-memory file, returns an in memory reader.
// returns an error ErrKeyNotFoundInCache, if the key does not
// exist. ErrKeyNotFoundInCache is also returned if lastAccessed
// is older than input atime.
func (c *Cache) Open(key string, atime time.Time) (io.Reader, error) {
	// Entry exists, return the readable buffer.
	c.mutex.Lock()
	defer c.mutex.Unlock()
	b, ok := c.entries[key]
	if !ok {
		return nil, ErrKeyNotFoundInCache
	}

	// Check if buf was recently accessed.
	if b.lastAccessed.Before(atime) {
		c.delete(key)
		return nil, ErrKeyNotFoundInCache
	}

	b.lastAccessed = time.Now()
	return bytes.NewReader(b.buf.Bytes()), nil
}

// Delete - delete deletes an entry from the cache.
func (c *Cache) Delete(key string) {
	c.mutex.Lock()
	c.delete(key)
	c.mutex.Unlock()
	if c.OnEviction != nil {
		c.OnEviction(key)
	}
}

// gc - garbage collect all the expired entries from the cache.
func (c *Cache) gc() {
	var evictedEntries []string
	c.mutex.Lock()
	for k, v := range c.entries {
		if c.expiry > 0 && time.Now().UTC().Sub(v.lastAccessed) > c.expiry {
			c.delete(k)
			evictedEntries = append(evictedEntries, k)
		}
	}
	c.mutex.Unlock()
	for _, k := range evictedEntries {
		if c.OnEviction != nil {
			c.OnEviction(k)
		}
	}
}

// StopGC sends a message to the expiry routine to stop
// expiring cached entries. NOTE: once this is called, cached
// entries will not be expired, be careful if you are using this.
func (c *Cache) StopGC() {
	if c.stopGC != nil {
		c.stopGC <- struct{}{}
	}
}

// StartGC starts running a routine ticking at expiry interval,
// on each interval this routine does a sweep across the cache
// entries and garbage collects all the expired entries.
func (c *Cache) StartGC() {
	go func() {
		for {
			select {
			// Wait till cleanup interval and initiate delete expired entries.
			case <-time.After(c.expiry / 4):
				c.gc()
				// Stop the routine, usually called by the user of object cache during cleanup.
			case <-c.stopGC:
				return
			}
		}
	}()
}

// Deletes a requested entry from the cache.
func (c *Cache) delete(key string) {
	if _, ok := c.entries[key]; ok {
		deletedSize := uint64(c.entries[key].buf.Len())
		c.entries[key].buf.Reset()
		bytebufferpool.Put(c.entries[key].buf)
		delete(c.entries, key)
		c.currentSize -= deletedSize
		c.totalEvicted++
	}
}
