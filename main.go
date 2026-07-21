package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/tg"
)

func main() {
	cfg := loadConfig()
	rawID := rawChannelID(cfg.BinChannel)

	dispatcher := tg.NewUpdateDispatcher()
	client := telegram.NewClient(cfg.APIID, cfg.APIHash, telegram.Options{
		SessionStorage: &session.FileStorage{Path: cfg.SessionFile},
		UpdateHandler:  dispatcher,
	})

	app := &App{
		cfg:          cfg,
		api:          tg.NewClient(client),
		cache:        newFileCache(),
		resolved:     make(chan struct{}),
		fsubResolved: make(chan struct{}),
		dlSem:        make(chan struct{}, cfg.MaxConcurrentDL),
		startedAt:    time.Now(),
	}
	app.sender = message.NewSender(app.api)

	dispatcher.OnNewMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewMessage) error {
		app.captureEntities(e, rawID)
		m, ok := u.Message.(*tg.Message)
		if !ok || m.Out {
			return nil
		}
		if m.Message == "/start" {
			return app.handleStart(ctx, e, u, m)
		}
		if m.Message == "/help" {
			return app.handleHelp(ctx, e, u)
		}
		if m.Message == "/stats" {
			return app.handleStats(ctx, e, u)
		}
		if m.Media != nil {
			return app.handleMedia(ctx, e, u, m)
		}
		return nil
	})
	// Every one of these just latches BIN_CHANNEL's access hash the moment
	// it shows up in Entities — see captureEntities in stream.go for why
	// this passive approach is needed instead of an RPC lookup.
	dispatcher.OnNewChannelMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewChannelMessage) error {
		app.captureEntities(e, rawID)
		return nil
	})
	dispatcher.OnChannelParticipant(func(ctx context.Context, e tg.Entities, u *tg.UpdateChannelParticipant) error {
		app.captureEntities(e, rawID)
		return nil
	})
	dispatcher.OnChannel(func(ctx context.Context, e tg.Entities, u *tg.UpdateChannel) error {
		app.captureEntities(e, rawID)
		return nil
	})

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	err := client.Run(ctx, func(ctx context.Context) error {
		if _, err := client.Auth().Bot(ctx, cfg.BotToken); err != nil {
			return err
		}
		self, err := client.Self(ctx)
		if err != nil {
			return err
		}
		app.botUser = self
		log.Printf("bot started as @%s", self.Username)

		// Fastest path: an explicit BIN_CHANNEL_ACCESS_HASH env var (log
		// shows this value once resolved below — copy it in for platforms
		// with ephemeral disks, e.g. Koyeb, so redeploys skip discovery).
		if cfg.BinChannelAccessHash != 0 {
			app.setBinChannel(rawID, cfg.BinChannelAccessHash)
			log.Printf("BIN_CHANNEL resolved from BIN_CHANNEL_ACCESS_HASH env var")
		} else if pc, ok := loadPeerCache(cfg.BinChannelCache); ok && pc.ChannelID == rawID {
			// Next fastest: whatever we persisted last time we resolved it
			// (survives process restarts as long as the disk isn't wiped).
			app.setBinChannel(rawID, pc.AccessHash)
			log.Printf("BIN_CHANNEL resolved from cache file %s", cfg.BinChannelCache)
		}

		fsubRawID := rawChannelID(cfg.FsubChannel)
		if cfg.FsubChannel == 0 {
			close(app.fsubResolved) // fsub disabled, nothing to wait on
		} else if cfg.FsubChannelAccessHash != 0 {
			app.setFsubChannel(fsubRawID, cfg.FsubChannelAccessHash)
			log.Printf("FSUB_CHANNEL resolved from FSUB_CHANNEL_ACCESS_HASH env var")
		} else if pc, ok := loadPeerCache(cfg.FsubChannelCache); ok && pc.ChannelID == fsubRawID {
			app.setFsubChannel(fsubRawID, pc.AccessHash)
			log.Printf("FSUB_CHANNEL resolved from cache file %s", cfg.FsubChannelCache)
		}

		// Start the HTTP server right away so health checks pass even
		// before BIN_CHANNEL is resolved — /start and /help work
		// immediately, only actual file forwarding waits on it.
		go func() {
			if err := app.runServer(); err != nil {
				log.Fatalf("http server: %v", err)
			}
		}()
		log.Printf("URL => %s", cfg.URL)

		go func() {
			select {
			case <-app.resolved:
				return
			case <-time.After(10 * time.Second):
			}
			log.Printf(
				"BIN_CHANNEL (id %d) not resolved yet — the bot needs a live "+
					"update mentioning it. Open the channel and re-add/promote "+
					"this bot as admin (or post any message there) to trigger it.",
				rawID)
			<-app.resolved
			log.Printf("BIN_CHANNEL resolved, file links will work now")
		}()

		if cfg.FsubChannel != 0 {
			go func() {
				select {
				case <-app.fsubResolved:
					return
				case <-time.After(10 * time.Second):
				}
				log.Printf(
					"FSUB_CHANNEL (id %d) not resolved yet — open it and "+
						"re-add/promote this bot as admin (or post any message "+
						"there) to trigger it. The fsub gate blocks uploads with "+
						"a 'try again' message until this resolves.",
					fsubRawID)
				<-app.fsubResolved
				log.Printf("FSUB_CHANNEL resolved, the gate is enforced now")
			}()
		}

		<-ctx.Done()
		return ctx.Err()
	})
	if err != nil && ctx.Err() == nil {
		log.Fatalf("fatal: %v", err)
	}
}
