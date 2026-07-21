package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

const chunkSize = 1024 * 1024 // 1 MiB, same as python

// Bots can't call messages.getDialogs OR messages.checkChatInvite
// (both are BOT_METHOD_INVALID — pure user-account concepts). The only
// thing that actually works for a bot account is passive: every update the
// bot receives comes with a resolved tg.Entities bundle, and the moment the
// bot is added/promoted as admin in BIN_CHANNEL (or sees any message posted
// there), that update's Entities.Channels[id] carries the full Channel
// object including AccessHash. captureEntities watches for that on every
// single update and latches it in once, see main.go wiring.
func (a *App) captureEntities(e tg.Entities, rawID int64) {
	if a.binChannel == nil {
		if ch, ok := e.Channels[rawID]; ok {
			a.setBinChannel(ch.ID, ch.AccessHash)
			savePeerCache(a.cfg.BinChannelCache, peerCache{ChannelID: ch.ID, AccessHash: ch.AccessHash})
			log.Printf(
				"resolved BIN_CHANNEL (id %d) from a live update — set "+
					"BIN_CHANNEL_ACCESS_HASH=%d as an env var to skip discovery on "+
					"future deploys (useful on platforms with ephemeral disks)",
				ch.ID, ch.AccessHash)
		}
	}
	if a.cfg.FsubChannel != 0 && a.fsubChannel == nil {
		fsubRawID := rawChannelID(a.cfg.FsubChannel)
		if ch, ok := e.Channels[fsubRawID]; ok {
			a.setFsubChannel(ch.ID, ch.AccessHash)
			savePeerCache(a.cfg.FsubChannelCache, peerCache{ChannelID: ch.ID, AccessHash: ch.AccessHash})
			log.Printf(
				"resolved FSUB_CHANNEL (id %d) from a live update — set "+
					"FSUB_CHANNEL_ACCESS_HASH=%d as an env var to skip discovery on "+
					"future deploys",
				ch.ID, ch.AccessHash)
		}
	}
}

// setBinChannel is the single place that actually latches a resolved
// channel in, whether it came from a live update, the on-disk cache, or the
// BIN_CHANNEL_ACCESS_HASH env var override. Idempotent/safe to call more
// than once.
func (a *App) setBinChannel(channelID, accessHash int64) {
	a.binChannel = &tg.InputChannel{ChannelID: channelID, AccessHash: accessHash}
	a.binPeer = &tg.InputPeerChannel{ChannelID: channelID, AccessHash: accessHash}
	select {
	case <-a.resolved:
	default:
		close(a.resolved)
	}
}

// setFsubChannel is the FSUB_CHANNEL equivalent of setBinChannel.
func (a *App) setFsubChannel(channelID, accessHash int64) {
	a.fsubChannel = &tg.InputChannel{ChannelID: channelID, AccessHash: accessHash}
	select {
	case <-a.fsubResolved:
	default:
		close(a.fsubResolved)
	}
}

func (a *App) isResolved() bool {
	return a.binChannel != nil
}

// fsubEnabled reports whether FSUB_CHANNEL was configured at all.
func (a *App) fsubEnabled() bool {
	return a.cfg.FsubChannel != 0
}

func (a *App) isFsubResolved() bool {
	return a.fsubChannel != nil
}

// isMember checks whether userPeer belongs to FSUB_CHANNEL. Only call this
// once fsubEnabled() && isFsubResolved() are both true.
func (a *App) isMember(ctx context.Context, userPeer tg.InputPeerClass) (bool, error) {
	_, err := a.api.ChannelsGetParticipant(ctx, &tg.ChannelsGetParticipantRequest{
		Channel:     a.fsubChannel,
		Participant: userPeer,
	})
	if err != nil {
		if tgerr.Is(err, "USER_NOT_PARTICIPANT") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// fetchFileInfo refreshes/loads a bin-channel message's file location via
// channels.getMessages. Mirrors get_file_ids in file_properties.py.
func (a *App) fetchFileInfo(ctx context.Context, msgID int) (FileInfo, error) {
	if fi, ok := a.cache.get(msgID); ok {
		return fi, nil
	}

	res, err := a.api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
		Channel: a.binChannel,
		ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: msgID}},
	})
	if err != nil {
		return FileInfo{}, fmt.Errorf("get message: %w", err)
	}

	var msgs []tg.MessageClass
	switch m := res.(type) {
	case *tg.MessagesMessages:
		msgs = m.Messages
	case *tg.MessagesMessagesSlice:
		msgs = m.Messages
	case *tg.MessagesChannelMessages:
		msgs = m.Messages
	}
	if len(msgs) == 0 {
		return FileInfo{}, errFileNotFound
	}
	m, ok := msgs[0].(*tg.Message)
	if !ok {
		return FileInfo{}, errFileNotFound
	}

	name, mimeType, size, mediaID, ok := mediaOf(m)
	if !ok {
		return FileInfo{}, errFileNotFound
	}
	loc, ok := locationOf(m)
	if !ok {
		return FileInfo{}, errFileNotFound
	}

	fi := FileInfo{
		Location: loc,
		DCID:     dcidOf(m),
		Size:     size,
		MimeType: mimeType,
		Name:     name,
		MediaID:  mediaID,
	}
	a.cache.set(msgID, fi)
	return fi, nil
}

var errFileNotFound = fmt.Errorf("file not found")

// downloadRange streams [from, until] inclusive to w, chunkSize at a time.
// Single-DC only: if the media lives on a DC other than the one our client
// session is bound to, upload.getFile will error out (FILE_MIGRATE_X) —
// cross-DC media session juggling (what custom_dl.py's generate_media_session
// does) was dropped for this "core only" port. Works fine when bot + bin
// channel share a DC, which is the common case.
// downloadRange used to fetch one 1 MiB chunk, wait for the full RPC
// round-trip, write it, then fetch the next — completely serial, so total
// throughput was capped at 1 chunk/RTT no matter how fast the pipe was.
// This version prefetches up to perStreamParallel chunks concurrently and
// writes them out strictly in order, so the RTT of chunk N+1 overlaps with
// writing chunk N instead of stacking on top of it.
func (a *App) downloadRange(ctx context.Context, loc tg.InputFileLocationClass, from, until int64, w io.Writer) error {
	parallel := a.cfg.PerStreamParallel
	offset := from - (from % chunkSize)
	firstCut := from - offset
	lastCut := until%chunkSize + 1
	total := int((until-offset)/chunkSize + 1)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type job struct {
		idx int
		off int64
	}
	type res struct {
		idx  int
		data []byte
		err  error
	}

	jobs := make(chan job)
	results := make(chan res, parallel)

	var wg sync.WaitGroup
	workers := parallel
	if total < workers {
		workers = total
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				data, err := a.getFileChunk(ctx, loc, j.off)
				select {
				case results <- res{j.idx, data, err}:
				case <-ctx.Done():
					return
				}
				if err != nil {
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for i := 0; i < total; i++ {
			select {
			case jobs <- job{i, offset + int64(i)*chunkSize}:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	pending := make(map[int][]byte, parallel)
	next := 0
	var firstErr error
	stop := false
	for r := range results {
		if stop {
			continue // drain so producer goroutines above don't leak on ctx.Done
		}
		if r.err != nil {
			firstErr = fmt.Errorf("upload.getFile at offset %d: %w", offset+int64(r.idx)*chunkSize, r.err)
			cancel()
			stop = true
			continue
		}
		pending[r.idx] = r.data
		for {
			data, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			if len(data) == 0 {
				stop = true
				cancel()
				break
			}
			start, end := int64(0), int64(len(data))
			if next == 0 {
				start = firstCut
			}
			if next == total-1 && lastCut < end {
				end = lastCut
			}
			if start > end {
				start = end
			}
			if _, werr := w.Write(data[start:end]); werr != nil {
				firstErr = werr
				cancel()
				stop = true
				break
			}
			next++
		}
	}
	return firstErr
}

// getFileChunk wraps upload.getFile with (a) a global concurrency cap, since
// hammering Telegram with dozens of parallel chunk requests — which happens
// fast when a video player opens several overlapping Range requests for
// seeking/buffering — trips FLOOD_WAIT almost immediately, and (b)
// transparent retry-with-backoff when FLOOD_WAIT does happen anyway.
func (a *App) getFileChunk(ctx context.Context, loc tg.InputFileLocationClass, offset int64) ([]byte, error) {
	a.dlSem <- struct{}{}
	defer func() { <-a.dlSem }()

	for attempt := 0; attempt < 5; attempt++ {
		r, err := a.api.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Location: loc,
			Offset:   offset,
			Limit:    chunkSize,
			Precise:  true,
		})
		if err != nil {
			if waited, werr := tgerr.FloodWait(ctx, err); waited {
				continue
			} else if werr != nil && werr != err {
				return nil, werr
			}
			return nil, err
		}
		f, ok := r.(*tg.UploadFile)
		if !ok {
			return nil, fmt.Errorf("unexpected upload.getFile response %T (CDN redirect not supported)", r)
		}
		return f.Bytes, nil
	}
	return nil, fmt.Errorf("gave up after repeated FLOOD_WAIT at offset %d", offset)
}

var pathRe = regexp.MustCompile(`^(\d+)(?:/(.*))?$`)

func (a *App) rootHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"server_status":"running","telegram_bot":"@%s"}`, a.botUser.Username)
}

func (a *App) streamHandler(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	m := pathRe.FindStringSubmatch(path)
	if m == nil {
		http.Error(w, "400: bad request, invalid link", http.StatusBadRequest)
		return
	}
	msgID, _ := strconv.Atoi(m[1])
	urlFileName, _ := url.QueryUnescape(m[2])
	secureHash := r.URL.Query().Get("hash")

	// Only the metadata lookup gets a short timeout. The actual byte
	// streaming below uses r.Context() directly (cancels only if the
	// client disconnects) — reusing the 30s metadata timeout here was a
	// bug: any file that took longer than 30s to fully stream had its
	// connection killed mid-transfer, and clients would just retry
	// forever, causing exactly the FLOOD_WAIT storm this was meant to fix.
	metaCtx, metaCancel := context.WithTimeout(r.Context(), 30*time.Second)
	fi, err := a.fetchFileInfo(metaCtx, msgID)
	metaCancel()
	if err != nil {
		http.Error(w, "404: file not found", http.StatusNotFound)
		return
	}
	if fileHash(a.cfg.BinChannel, fi.MediaID, a.cfg.HashLength) != secureHash {
		http.Error(w, "403: invalid hash", http.StatusForbidden)
		return
	}
	ctx := r.Context()

	fileSize := fi.Size
	var from, until int64
	rangeHeader := r.Header.Get("Range")
	status := http.StatusOK
	if rangeHeader != "" {
		status = http.StatusPartialContent
		from, until = parseRange(rangeHeader, fileSize)
	} else {
		until = fileSize - 1
	}

	if until >= fileSize || from < 0 || until < from {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", fileSize))
		http.Error(w, "416: range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return
	}

	name := fi.Name
	if urlFileName != "" {
		name = urlFileName
	}
	mimeType := fi.MimeType
	if mimeType == "" {
		mimeType = mime.TypeByExtension(name)
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
	}

	h := w.Header()
	h.Set("Content-Type", mimeType)
	h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, until, fileSize))
	h.Set("Content-Length", strconv.FormatInt(until-from+1, 10))
	h.Set("Content-Disposition", fmt.Sprintf("attachment; filename*=UTF-8''%s", url.PathEscape(name)))
	h.Set("Accept-Ranges", "bytes")
	w.WriteHeader(status)

	if r.Method == http.MethodHead {
		return
	}
	cw := &countingWriter{w: w}
	start := time.Now()
	err = a.downloadRange(ctx, fi.Location, from, until, cw)
	elapsed := time.Since(start)
	mbps := float64(cw.n) * 8 / 1e6 / elapsed.Seconds()
	if err != nil {
		log.Printf("stream error for message %d after %s (%.1f MB, %.1f Mbps): %v",
			msgID, elapsed.Round(time.Millisecond), float64(cw.n)/1e6, mbps, err)
	} else {
		log.Printf("stream done for message %d: %.1f MB in %s (%.1f Mbps)",
			msgID, float64(cw.n)/1e6, elapsed.Round(time.Millisecond), mbps)
	}
}

// countingWriter tracks total bytes written, purely for throughput logging.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func parseRange(header string, size int64) (from, until int64) {
	spec := strings.TrimPrefix(header, "bytes=")
	parts := strings.SplitN(spec, "-", 2)
	from, _ = strconv.ParseInt(parts[0], 10, 64)
	if len(parts) > 1 && parts[1] != "" {
		until, _ = strconv.ParseInt(parts[1], 10, 64)
	} else {
		until = size - 1
	}
	return
}

func (a *App) runServer() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			a.rootHandler(w, r)
			return
		}
		a.streamHandler(w, r)
	})
	addr := a.cfg.BindAddr + ":" + a.cfg.Port
	log.Printf("web server listening on %s", addr)
	return http.ListenAndServe(addr, mux)
}
