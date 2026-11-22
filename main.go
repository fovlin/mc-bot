package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	log.SetFlags(log.LstdFlags)

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("[mc-bot] %v", err)
	}
	path, _ := configPath()

	renderer, fontPath, err := newRenderer(cfg.Font)
	if err != nil {
		log.Fatalf("[mc-bot] %v", err)
	}

	api := newQQAPI(cfg.AppID, cfg.ClientSecret)
	bot := newBot(cfg, api, renderer)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("[mc-bot] starting: config=%s servers=%d font=%s", path, len(cfg.Servers), fontPath)
	gateway := newGateway(api, bot.handleEvent)
	if err := gateway.run(ctx); err != nil {
		log.Fatalf("[mc-bot] %v", err)
	}
	log.Printf("[mc-bot] stopped")
}
