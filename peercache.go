package main

import (
	"encoding/json"
	"os"
)

// peerCache persists BIN_CHANNEL's resolved access hash next to the session
// file, so a restart doesn't need a fresh live update to re-learn it (pyrogram
// gets this for free from its sqlite peer-cache; we don't have one, so we
// roll our own single-entry version). Deleting this file forces the
// remove+re-add-admin dance again on next start.
type peerCache struct {
	ChannelID  int64 `json:"channel_id"`
	AccessHash int64 `json:"access_hash"`
}

func loadPeerCache(path string) (peerCache, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return peerCache{}, false
	}
	var pc peerCache
	if err := json.Unmarshal(data, &pc); err != nil {
		return peerCache{}, false
	}
	return pc, true
}

func savePeerCache(path string, pc peerCache) {
	data, err := json.Marshal(pc)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o600)
}
