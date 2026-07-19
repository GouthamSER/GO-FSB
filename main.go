package main

import (
	"context"
	"log"
	"os"
	"os/signal"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/tg"
)

func main() {
	cfg := loadConfig()

	dispatcher := tg.NewUpdateDispatcher()
	client := telegram.NewClient(cfg.APIID, cfg.APIHash, telegram.Options{
		SessionStorage: &session.FileStorage{Path: cfg.SessionFile},
		UpdateHandler:  dispatcher,
	})

	app := &App{
		cfg:   cfg,
		api:   tg.NewClient(client),
		cache: newFileCache(),
	}
	app.sender = message.NewSender(app.api)

	dispatcher.OnNewMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewMessage) error {
		m, ok := u.Message.(*tg.Message)
		if !ok || m.Out {
			return nil
		}
		if m.Message == "/start" {
			return app.handleStart(ctx, e, u)
		}
		if m.Message == "/help" {
			return app.handleHelp(ctx, e, u)
		}
		if m.Media != nil {
			return app.handleMedia(ctx, e, u, m)
		}
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

		rawID := rawChannelID(cfg.BinChannel)
		binChannel, err := resolveBinChannel(ctx, app.api, cfg.BinChannelInvite, rawID)
		if err != nil {
			return err
		}
		app.binChannel = binChannel
		app.binPeer = &tg.InputPeerChannel{ChannelID: binChannel.ChannelID, AccessHash: binChannel.AccessHash}
		log.Printf("resolved BIN_CHANNEL (raw id %d)", rawID)

		go func() {
			if err := app.runServer(); err != nil {
				log.Fatalf("http server: %v", err)
			}
		}()

		log.Printf("URL => %s", cfg.URL)
		<-ctx.Done()
		return ctx.Err()
	})
	if err != nil && ctx.Err() == nil {
		log.Fatalf("fatal: %v", err)
	}
}
