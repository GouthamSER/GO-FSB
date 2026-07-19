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

// resolveBinChannel finds BIN_CHANNEL's access hash via messages.checkChatInvite
// on its invite link. Bots can't call messages.getDialogs (BOT_METHOD_INVALID —
// "dialogs" is a user-account concept), so the invite-link check is the
// actual working bootstrap: the bot must already be a member (added via that
// same invite link, or promoted directly), and checkChatInvite returns the
// full Channel object — including access_hash — either way.
func resolveBinChannel(ctx context.Context, api *tg.Client, invite string, rawID int64) (*tg.InputChannel, error) {
	hash := extractInviteHash(invite)
	if hash == "" {
		return nil, fmt.Errorf("could not parse invite hash out of BIN_CHANNEL_INVITE %q", invite)
	}

	res, err := api.MessagesCheckChatInvite(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("checkChatInvite: %w", err)
	}

	var chat tg.ChatClass
	switch v := res.(type) {
	case *tg.ChatInviteAlready:
		chat = v.Chat
	case *tg.ChatInvitePeek:
		chat = v.Chat
	default:
		return nil, fmt.Errorf(
			"bot is not a member of BIN_CHANNEL yet (got %T back) — add/promote the "+
				"bot in the channel via this exact invite link first", res)
	}

	ch, ok := chat.(*tg.Channel)
	if !ok {
		return nil, fmt.Errorf("BIN_CHANNEL_INVITE does not point at a channel (got %T)", chat)
	}
	if ch.ID != rawID {
		return nil, fmt.Errorf(
			"BIN_CHANNEL_INVITE resolves to channel id %d, but BIN_CHANNEL env is %d — "+
				"double check they point at the same channel", ch.ID, rawID)
	}
	return &tg.InputChannel{ChannelID: ch.ID, AccessHash: ch.AccessHash}, nil
}

// extractInviteHash pulls the hash out of a t.me invite link. Accepts
// https://t.me/+HASH, t.me/+HASH, https://t.me/joinchat/HASH, or a bare hash.
func extractInviteHash(link string) string {
	s := strings.TrimSpace(link)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "t.me/")
	s = strings.TrimPrefix(s, "+")
	s = strings.TrimPrefix(s, "joinchat/")
	return s
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
