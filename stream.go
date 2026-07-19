package main

import (
	"context"
	"fmt"
	"log"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/tg"
)

const chunkSize = 1024 * 1024 // 1 MiB, same as python

// resolveBinChannel finds BIN_CHANNEL's access hash by walking the bot's
// dialog list (messages.getDialogs). The bot must already be a member/admin
// of the channel. This is the Go equivalent of what pyrogram's local peer
// cache gives you for free.
func resolveBinChannel(ctx context.Context, api *tg.Client, rawID int64) (*tg.InputChannel, error) {
	offsetPeer := tg.InputPeerClass(&tg.InputPeerEmpty{})
	offsetID, offsetDate := 0, 0

	for i := 0; i < 20; i++ { // hard cap of ~20 pages so a huge account can't loop forever
		res, err := api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
			OffsetDate: offsetDate,
			OffsetID:   offsetID,
			OffsetPeer: offsetPeer,
			Limit:      100,
			Hash:       0,
		})
		if err != nil {
			return nil, fmt.Errorf("getDialogs: %w", err)
		}

		var chats []tg.ChatClass
		var dialogs []tg.DialogClass
		done := false
		switch d := res.(type) {
		case *tg.MessagesDialogs:
			chats, dialogs = d.Chats, d.Dialogs
			done = true
		case *tg.MessagesDialogsSlice:
			chats, dialogs = d.Chats, d.Dialogs
		default:
			return nil, fmt.Errorf("unexpected dialogs response %T", res)
		}

		for _, c := range chats {
			if ch, ok := c.(*tg.Channel); ok && ch.ID == rawID {
				return &tg.InputChannel{ChannelID: ch.ID, AccessHash: ch.AccessHash}, nil
			}
		}

		if done || len(dialogs) == 0 {
			break
		}
		last := dialogs[len(dialogs)-1]
		if d, ok := last.(*tg.Dialog); ok {
			offsetID = d.TopMessage
		}
		// crude but fine for our purposes: stop once a page comes back short
		if len(dialogs) < 100 {
			break
		}
	}
	return nil, fmt.Errorf(
		"BIN_CHANNEL %d not found in bot's dialog list — make sure the bot "+
			"is an admin/member of it and has sent/received at least one message there",
		rawID)
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
func (a *App) downloadRange(ctx context.Context, loc tg.InputFileLocationClass, from, until int64, w http.ResponseWriter) error {
	offset := from - (from % chunkSize)
	firstCut := from - offset
	lastCut := until%chunkSize + 1

	for cur := offset; cur <= until; cur += chunkSize {
		r, err := a.api.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Location: loc,
			Offset:   cur,
			Limit:    chunkSize,
			Precise:  true,
		})
		if err != nil {
			return fmt.Errorf("upload.getFile at offset %d: %w", cur, err)
		}
		f, ok := r.(*tg.UploadFile)
		if !ok {
			return fmt.Errorf("unexpected upload.getFile response %T (CDN redirect not supported)", r)
		}
		chunk := f.Bytes
		if len(chunk) == 0 {
			break
		}

		start, end := int64(0), int64(len(chunk))
		if cur == offset {
			start = firstCut
		}
		if cur+chunkSize > until {
			if lastCut < end {
				end = lastCut
			}
		}
		if start > end {
			start = end
		}
		if _, err := w.Write(chunk[start:end]); err != nil {
			return err
		}
	}
	return nil
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

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	fi, err := a.fetchFileInfo(ctx, msgID)
	if err != nil {
		http.Error(w, "404: file not found", http.StatusNotFound)
		return
	}
	if fileHash(a.cfg.BinChannel, fi.MediaID, a.cfg.HashLength) != secureHash {
		http.Error(w, "403: invalid hash", http.StatusForbidden)
		return
	}

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
	if err := a.downloadRange(ctx, fi.Location, from, until, w); err != nil {
		log.Printf("stream error for message %d: %v", msgID, err)
	}
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
