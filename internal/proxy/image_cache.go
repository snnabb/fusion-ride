package proxy

import (
	"container/list"
	"net/http"
	"sync"
)

type imageCache struct {
	mu          sync.Mutex
	ll          *list.List
	items       map[string]*list.Element
	sizeBytes   int64
	maxBytes    int64
	maxItemSize int64
}

type imageCacheEntry struct {
	key     string
	status  int
	body    []byte
	headers http.Header
	size    int64
}

func newImageCache(maxBytes, maxItemSize int64) *imageCache {
	return &imageCache{
		ll:          list.New(),
		items:       make(map[string]*list.Element),
		maxBytes:    maxBytes,
		maxItemSize: maxItemSize,
	}
}

func (c *imageCache) Get(key string) (imageCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	element, ok := c.items[key]
	if !ok {
		return imageCacheEntry{}, false
	}

	c.ll.MoveToFront(element)
	entry := element.Value.(*imageCacheEntry)
	return imageCacheEntry{
		key:     entry.key,
		status:  entry.status,
		body:    append([]byte(nil), entry.body...),
		headers: cloneHeader(entry.headers),
		size:    entry.size,
	}, true
}

func (c *imageCache) Put(key string, status int, headers http.Header, body []byte) {
	if c == nil {
		return
	}
	if int64(len(body)) > c.maxItemSize {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if existing, ok := c.items[key]; ok {
		entry := existing.Value.(*imageCacheEntry)
		c.sizeBytes -= entry.size
		entry.status = status
		entry.body = append([]byte(nil), body...)
		entry.headers = cloneHeader(headers)
		entry.size = int64(len(body))
		c.sizeBytes += entry.size
		c.ll.MoveToFront(existing)
		c.evictIfNeeded()
		return
	}

	entry := &imageCacheEntry{
		key:     key,
		status:  status,
		body:    append([]byte(nil), body...),
		headers: cloneHeader(headers),
		size:    int64(len(body)),
	}
	element := c.ll.PushFront(entry)
	c.items[key] = element
	c.sizeBytes += entry.size
	c.evictIfNeeded()
}

func (c *imageCache) evictIfNeeded() {
	for c.sizeBytes > c.maxBytes && c.ll.Len() > 0 {
		element := c.ll.Back()
		if element == nil {
			return
		}
		entry := element.Value.(*imageCacheEntry)
		delete(c.items, entry.key)
		c.sizeBytes -= entry.size
		c.ll.Remove(element)
	}
}

func cloneHeader(src http.Header) http.Header {
	if src == nil {
		return make(http.Header)
	}
	dst := make(http.Header, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}
