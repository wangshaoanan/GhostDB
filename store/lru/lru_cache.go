/*
 * Copyright (c) 2020, Jake Grogan
 * All rights reserved.
 *
 * Redistribution and use in source and binary forms, with or without
 * modification, are permitted provided that the following conditions are met:
 *
 *  * Redistributions of source code must retain the above copyright notice, this
 *    list of conditions and the following disclaimer.
 *
 *  * Redistributions in binary form must reproduce the above copyright notice,
 *    this list of conditions and the following disclaimer in the documentation
 *    and/or other materials provided with the distribution.
 *
 *  * Neither the name of the copyright holder nor the names of its
 *    contributors may be used to endorse or promote products derived from
 *    this software without specific prior written permission.
 *
 * THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
 * AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
 * IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
 * DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE
 * FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL
 * DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
 * SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER
 * CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY,
 * OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
 * OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
 */

package lru

import (
	"log"
	"sync"
	"sync/atomic"

	"github.com/ghostdb/ghostdb-cache-node/config"
	"github.com/ghostdb/ghostdb-cache-node/store/request"
	"github.com/ghostdb/ghostdb-cache-node/store/response"
	"github.com/ghostdb/ghostdb-cache-node/store/structures/queue"
)

const (
	CacheMiss = "CACHE_MISS"
	STORED    = "STORED"
	NotStored = "NOT_STORED"
	REMOVED   = "REMOVED"
	NotFound  = "NOT_FOUND"
	FLUSHED   = "FLUSH"
	ErrFlush  = "ERR_FLUSH"
	NotQueue  = "NOT_QUEUE"
)

// LRUCache represents a cache object
type Cache struct {
	// Size represents the maximum number of allowable
	// key-value pairs in the cache.
	Size int32

	// Count records the number of key-value pairs
	// currently in the cache.
	Count int32

	// Full tracks if Count is equal to Size
	Full bool

	// DLL is a doubly linked list containing all key-value pairs
	DLL *List `json:"omitempty"`

	// Hashtable maps to nodes in the doubly linked list
	Hashtable map[string]*Node

	// Mux is a mutex lock
	Mux sync.Mutex
}

// NewLRU will initialize the cache
func NewLRU(config config.Configuration) *Cache {
	return &Cache{
		Size:      config.KeyspaceSize,
		Count:     int32(0),
		Full:      false,
		DLL:       InitList(),
		Hashtable: newHashtable(),
	}
}

func newHashtable() map[string]*Node {
	return make(map[string]*Node)
}

// Get will fetch a key/value pair from the cache
func (cache *Cache) Get(args request.CacheRequest) response.CacheResponse {
	// Fix in the FUTURE
	// to use a method that validates the
	// request object for this method.
	key := args.Gobj.Key

	cache.Mux.Lock()
	nodeToGet := cache.Hashtable[key]
	cache.Mux.Unlock()

	if nodeToGet == nil {
		return response.NewCacheMissResponse()
	}

	cache.Mux.Lock()
	n, _ := RemoveNode(cache.DLL, nodeToGet)
	cache.Mux.Unlock()

	cache.Mux.Lock()
	node, _ := Insert(cache.DLL, n.Key, n.Value, n.TTL)
	cache.Mux.Unlock()

	cache.Mux.Lock()
	cache.Hashtable[key] = node
	cache.Mux.Unlock()

	return response.NewResponseFromValue(node.Value)
}

// Put will add a key/value pair to the cache, possibly
// overwriting an existing key/value pair. Put will evict
// a key/value pair if the cache is full.
func (cache *Cache) Put(args request.CacheRequest) response.CacheResponse {
	key := args.Gobj.Key
	value := args.Gobj.Value
	ttl := args.Gobj.TTL

	if !cache.Full {
		inCache := keyInCache(cache, key)

		newNode, _ := Insert(cache.DLL, key, value, ttl)

		insertIntoHashtable(cache, key, newNode)

		if !inCache {
			cache.Mux.Lock()
			atomic.AddInt32(&cache.Count, 1)
			cache.Mux.Unlock()
		}

		if cache.Count == cache.Size {
			cache.Full = true
		}
	} else {
		// SPECIAL CASE: Just update the value
		inCache := keyInCache(cache, key)
		if inCache {
			// Get the value node
			node := cache.Hashtable[key]

			// Update the value
			node.Value = value
			return response.NewResponseFromMessage(STORED, 1)
		}
		n, _ := RemoveLast(cache.DLL)

		deleteFromHashtable(cache, n.Key)

		newNode, _ := Insert(cache.DLL, key, value, ttl)
		insertIntoHashtable(cache, key, newNode)
	}
	return response.NewResponseFromMessage(STORED, 1)
}

func deleteFromHashtable(cache *Cache, key string) {
	cache.Mux.Lock()
	defer cache.Mux.Unlock()
	delete(cache.Hashtable, key)
}

// Add will add a key/value pair to the cache if the key
// does not exist already. It will not evict a key/value pair
// from the cache. If the cache is full, the key/value pair does
// not get added.
func (cache *Cache) Add(args request.CacheRequest) response.CacheResponse {
	key := args.Gobj.Key
	value := args.Gobj.Value
	ttl := args.Gobj.TTL

	cache.Mux.Lock()
	_, ok := cache.Hashtable[key]
	cache.Mux.Unlock()
	if ok {
		return response.NewResponseFromMessage(NotStored, 0)
	}
	if !cache.Full {
		inCache := keyInCache(cache, key)

		newNode, _ := Insert(cache.DLL, key, value, ttl)

		insertIntoHashtable(cache, key, newNode)

		if !inCache {
			atomic.AddInt32(&cache.Count, 1)
		}

		if cache.Count == cache.Size {
			cache.Full = true
		}
	} else {
		n, _ := RemoveLast(cache.DLL)
		deleteFromHashtable(cache, n.Key)

		newNode, _ := Insert(cache.DLL, key, value, ttl)
		insertIntoHashtable(cache, key, newNode)
	}
	return response.NewResponseFromMessage(STORED, 1)
}

// Delete removes a key/value pair from the cache
// Returns NOT_FOUND if the key does not exist.
func (cache *Cache) Delete(args request.CacheRequest) response.CacheResponse {
	key := args.Gobj.Key

	cache.Mux.Lock()
	_, ok := cache.Hashtable[key]
	cache.Mux.Unlock()
	if ok {
		cache.Mux.Lock()
		nodeToRemove := cache.Hashtable[key]
		cache.Mux.Unlock()

		if nodeToRemove == nil {
			return response.NewResponseFromMessage(NotFound, 0)
		}

		deleteFromHashtable(cache, nodeToRemove.Key)
		_, err := RemoveNode(cache.DLL, nodeToRemove)
		if err != nil {
			log.Println("failed to remove key-value pair")
		}

		cache.Mux.Lock()
		atomic.AddInt32(&cache.Count, -1)
		cache.Mux.Unlock()

		cache.Full = false
		return response.NewResponseFromMessage(REMOVED, 1)
	}
	return response.NewResponseFromMessage(NotFound, 0)
}

// Flush removes all key/value pairs from the cache even if they have not expired
func (cache *Cache) Flush(args request.CacheRequest) response.CacheResponse {
	for k := range cache.Hashtable {
		n, _ := RemoveLast(cache.DLL)
		if n == nil {
			break
		}
		deleteFromHashtable(cache, k)
		cache.Mux.Lock()
		atomic.AddInt32(&cache.Count, -1)
		cache.Mux.Unlock()
	}

	cache.Full = false

	if cache.Count == int32(0) {
		return response.NewResponseFromMessage(FLUSHED, 1)
	}
	return response.NewResponseFromMessage(ErrFlush, 0)
}

// CountKeys return the number of keys in the cache
func (cache *Cache) CountKeys(args request.CacheRequest) response.CacheResponse {
	return response.NewResponseFromValue(cache.Count)
}

// DeleteByKey functions the same as Delete, however it is used in various locations
// to reduce the cost of allocating request objects for internal deletion mechanisms
// e.g. the cache crawlers.
func (cache *Cache) DeleteByKey(key string) response.CacheResponse {
	cache.Mux.Lock()
	_, ok := cache.Hashtable[key]
	cache.Mux.Unlock()
	if ok {
		cache.Mux.Lock()
		nodeToRemove := cache.Hashtable[key]
		cache.Mux.Unlock()

		if nodeToRemove == nil {
			return response.NewResponseFromMessage(NotFound, 0)
		}

		deleteFromHashtable(cache, nodeToRemove.Key)
		_, err := RemoveNode(cache.DLL, nodeToRemove)
		if err != nil {
			log.Println("failed to remove key-value pair")
		}

		cache.Mux.Lock()
		atomic.AddInt32(&cache.Count, -1)
		cache.Mux.Unlock()

		cache.Full = false

		return response.NewResponseFromMessage(REMOVED, 1)
	}
	return response.NewResponseFromMessage(NotFound, 0)
}

// Enqueue adds an item to a queue
func (cache *Cache) Enqueue(args request.CacheRequest) response.CacheResponse {
	key := args.Gobj.Key
	value := args.Gobj.Value
	ttl := args.Gobj.TTL

	if !cache.Full {
		inCache := keyInCache(cache, key)
		// Queue doesn't exist so create one
		if !inCache {
			// Create empty queue and queue the value
			q := queue.New()
			q.Enqueue(value)

			// Insert the queue into the cache
			newNode, _ := Insert(cache.DLL, key, q, ttl)
			insertIntoHashtable(cache, key, newNode)
			
			// Increase count
			cache.Mux.Lock()
			atomic.AddInt32(&cache.Count, 1)
			cache.Mux.Unlock()

			if cache.Count == cache.Size {
				cache.Full = true
			}
		} else {
			// Queue exists, enqueue the value
			node := cache.Hashtable[key]

			// Cast to queue and enqueue
			q, ok := node.Value.(*queue.Queue)
			if !ok {
				return response.NewResponseFromMessage(NotQueue, 0)
			}
			q.Enqueue(value)

			return response.NewResponseFromMessage(STORED, 1)
		}
	} else {
		// Cache full, remove the LRU item and add queue
		// SPECIAL CASE: Just enqueue the value
		inCache := keyInCache(cache, key)
		if inCache {
			// Get the node
			node := cache.Hashtable[key]

			// Enqueue the value
			q, ok := node.Value.(*queue.Queue)
			if !ok {
				return response.NewResponseFromMessage(NotQueue, 0)
			}
			q.Enqueue(value)
			return response.NewResponseFromMessage(STORED, 1)
		}
		// Not in the cache, make room
		// Remove LRU item
		n, _ := RemoveLast(cache.DLL)
		deleteFromHashtable(cache, n.Key)

		// Create empty queue and queue the value
		q := queue.New()
		q.Enqueue(value)

		// Insert the queue into the cache
		newNode, _ := Insert(cache.DLL, key, q, ttl)
		insertIntoHashtable(cache, key, newNode)
	}
	return response.NewResponseFromMessage(STORED, 1)
}

// Dequeue removes an item from a queue
func (cache *Cache) Dequeue(args request.CacheRequest) response.CacheResponse {
	key := args.Gobj.Key

	cache.Mux.Lock()
	queueNode, ok := cache.Hashtable[key]
	cache.Mux.Unlock()
	if ok {
		if queueNode == nil {
			return response.NewResponseFromMessage(NotFound, 0)
		}

		// Cast to Queue and dequeue the item
		queue, ok := queueNode.Value.(*queue.Queue)
		if !ok {
			return response.NewResponseFromMessage(NotQueue, 0)
		}
		value := queue.Dequeue()

		// TODO: Make moving node to MRU a method
		// Queue node is now MRU so move it to top
		cache.Mux.Lock()
		n, _ := RemoveNode(cache.DLL, queueNode)
		cache.Mux.Unlock()

		cache.Mux.Lock()
		node, _ := Insert(cache.DLL, n.Key, n.Value, n.TTL)
		cache.Mux.Unlock()

		cache.Mux.Lock()
		cache.Hashtable[key] = node
		cache.Mux.Unlock()

		// Return the dequeued item
		return response.NewResponseFromValue(value)
	}
	return response.NewResponseFromMessage(NotFound, 0)
}

func (cache *Cache) GetHashtableReference() *map[string]*Node {
	return &cache.Hashtable
}

func insertIntoHashtable(cache *Cache, key string, node *Node) {
	cache.Mux.Lock()
	defer cache.Mux.Unlock()
	cache.Hashtable[key] = node
}

func keyInCache(cache *Cache, key string) bool {
	cache.Mux.Lock()
	defer cache.Mux.Unlock()
	_, ok := cache.Hashtable[key]
	return ok
}
