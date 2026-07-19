package main

import (
	"context"
	"fmt"
	"log"
	"net/url"

	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/tg"
)

// App holds everything the handlers need.
type App struct {
	cfg        Config
	api        *tg.Client
	sender     *message.Sender
	cache      *fileCache
	binPeer    tg.InputPeerClass
	binChannel *tg.InputChannel
	botUser    *tg.User
	resolved   chan struct{} // closed once binChannel is resolved
	dlSem      chan struct{} // caps concurrent upload.getFile calls
}

func mediaOf(m *tg.Message) (name string, mime string, size int64, mediaID int64, ok bool) {
	switch media := m.Media.(type) {
	case *tg.MessageMediaDocument:
		doc, isDoc := media.Document.AsNotEmpty()
		if !isDoc {
			return "", "", 0, 0, false
		}
		return getDocFilename(doc), doc.MimeType, doc.Size, doc.ID, true
	case *tg.MessageMediaPhoto:
		photo, isPhoto := media.Photo.AsNotEmpty()
		if !isPhoto {
			return "", "", 0, 0, false
		}
		return fmt.Sprintf("photo_%d.jpg", photo.ID), "image/jpeg", 0, photo.ID, true
	default:
		return "", "", 0, 0, false
	}
}

// locationOf/dcidOf build the raw download location straight from a
// tg.Message's media — used both right after forwarding and when refreshing
// an expired cache entry via channels.getMessages (see stream.go).
func locationOf(m *tg.Message) (tg.InputFileLocationClass, bool) {
	switch media := m.Media.(type) {
	case *tg.MessageMediaDocument:
		doc, ok := media.Document.AsNotEmpty()
		if !ok {
			return nil, false
		}
		return doc.AsInputDocumentFileLocation(), true
	case *tg.MessageMediaPhoto:
		photo, ok := media.Photo.AsNotEmpty()
		if !ok {
			return nil, false
		}
		var thumbSize string
		var maxW, maxH int
		for _, s := range photo.Sizes {
			sz, ok := s.(interface {
				GetW() int
				GetH() int
				GetType() string
			})
			if ok && sz.GetW() > maxW && sz.GetH() > maxH {
				maxW, maxH = sz.GetW(), sz.GetH()
				thumbSize = sz.GetType()
			}
		}
		if thumbSize == "" {
			return nil, false
		}
		return &tg.InputPhotoFileLocation{
			ID:            photo.ID,
			AccessHash:    photo.AccessHash,
			FileReference: photo.FileReference,
			ThumbSize:     thumbSize,
		}, true
	default:
		return nil, false
	}
}

func dcidOf(m *tg.Message) int {
	switch media := m.Media.(type) {
	case *tg.MessageMediaDocument:
		if doc, ok := media.Document.AsNotEmpty(); ok {
			return doc.DCID
		}
	case *tg.MessageMediaPhoto:
		if photo, ok := media.Photo.AsNotEmpty(); ok {
			return photo.DCID
		}
	}
	return 0
}

func getDocFilename(doc *tg.Document) string {
	for _, attr := range doc.Attributes {
		if fn, ok := attr.(*tg.DocumentAttributeFilename); ok && fn.FileName != "" {
			return sanitizeFilename(fn.FileName)
		}
	}
	return fmt.Sprintf("file_%d", doc.ID)
}

func fromPeerOf(e tg.Entities, m *tg.Message) (tg.InputPeerClass, bool) {
	if pu, ok := m.PeerID.(*tg.PeerUser); ok {
		if u, ok := e.Users[pu.UserID]; ok {
			return &tg.InputPeerUser{UserID: u.ID, AccessHash: u.AccessHash}, true
		}
	}
	return nil, false
}

// extractForwardedID pulls the new message id out of the Updates returned by
// ForwardIDs().Send(), i.e. the id it got inside BIN_CHANNEL.
func extractForwardedID(upd tg.UpdatesClass) (int, bool) {
	u, ok := upd.(*tg.Updates)
	if !ok {
		return 0, false
	}
	for _, uc := range u.Updates {
		if unm, ok := uc.(*tg.UpdateNewChannelMessage); ok {
			if m, ok := unm.Message.(*tg.Message); ok {
				return m.ID, true
			}
		}
	}
	return 0, false
}

func (a *App) buildStreamLink(msgID int, name string, mediaID int64) string {
	hash := fileHash(a.cfg.BinChannel, mediaID, a.cfg.HashLength)
	return fmt.Sprintf("%s%d/%s?hash=%s", a.cfg.URL, msgID, url.PathEscape(name), hash)
}

func (a *App) handleStart(ctx context.Context, e tg.Entities, u message.AnswerableMessageUpdate) error {
	_, err := a.sender.Reply(e, u).Text(ctx,
		"Hello! I'm File to link.\n\n"+
			"Send me any file (document/video/audio/photo) and I'll generate a "+
			"direct download / streaming link for it.")
	return err
}

func (a *App) handleHelp(ctx context.Context, e tg.Entities, u message.AnswerableMessageUpdate) error {
	_, err := a.sender.Reply(e, u).Text(ctx,
		"Send any file to get an instant download/streaming link.")
	return err
}

func (a *App) handleMedia(ctx context.Context, e tg.Entities, u message.AnswerableMessageUpdate, m *tg.Message) error {
	if !a.isResolved() {
		_, err := a.sender.Reply(e, u).Text(ctx,
			"⏳ Still linking to storage channel, try again in a few seconds.")
		return err
	}
	name, mime, size, mediaID, ok := mediaOf(m)
	if !ok {
		return nil
	}
	fromPeer, ok := fromPeerOf(e, m)
	if !ok {
		log.Printf("could not resolve sender peer for message %d", m.ID)
		return nil
	}

	upd, err := a.sender.To(a.binPeer).ForwardIDs(fromPeer, m.ID).Send(ctx)
	if err != nil {
		log.Printf("forward to bin channel failed: %v", err)
		return nil
	}
	binMsgID, ok := extractForwardedID(upd)
	if !ok {
		log.Printf("could not read forwarded message id for %d", m.ID)
		return nil
	}

	// Cache straight away using what we already know, so the first stream
	// request doesn't need a channels.getMessages round trip.
	if loc, ok := locationOf(m); ok {
		a.cache.set(binMsgID, FileInfo{
			Location: loc,
			DCID:     dcidOf(m),
			Size:     size,
			MimeType: mime,
			Name:     name,
			MediaID:  mediaID,
		})
	}

	link := a.buildStreamLink(binMsgID, name, mediaID)
	sizeStr := "Unknown"
	if size > 0 {
		sizeStr = readableSize(size)
	}

	text := fmt.Sprintf(
		"✅ Your link is ready!\n\nFile name: %s\nFile size: %s\n\nLink: %s",
		name, sizeStr, link,
	)
	_, err = a.sender.Reply(e, u).Text(ctx, text)
	return err
}
