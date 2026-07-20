package main

import (
	"sync"
	"time"

	"github.com/gotd/td/tg"
)

// FileInfo is what we cache per bin-channel message id — enough to build a
// download location and answer HTTP range requests. Mirrors the FileId
// object pyrogram builds in utils/custom_dl.py.
type FileInfo struct {
	Location tg.InputFileLocationClass
	DCID     int
	Size     int64
	MimeType string
	Name     string
	MediaID  int64 // document id or photo id, used for hash verification
}

type fileCache struct {
	mu   sync.RWMutex
	data map[int]FileInfo
}

func newFileCache() *fileCache {
	c := &fileCache{data: make(map[int]FileInfo)}
	go c.cleanLoop()
	return c
}

func (c *fileCache) count() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.data)
}

func (c *fileCache) get(msgID int) (FileInfo, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.data[msgID]
	return v, ok
}

func (c *fileCache) set(msgID int, fi FileInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data[msgID] = fi
}

// cleanLoop mirrors ByteStreamer.clean_cache: wipe the whole cache every 30
// minutes so deleted/expired bin-channel files stop being served from stale
// cached locations.
func (c *fileCache) cleanLoop() {
	for {
		time.Sleep(30 * time.Minute)
		c.mu.Lock()
		c.data = make(map[int]FileInfo)
		c.mu.Unlock()
	}
}
