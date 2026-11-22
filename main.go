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

	path, err := configPath()
	if err != nil {
		log.Fatalf("[mcbot-go] %v", err)
	}
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("[mcbot-go] %v", err)
	}

	renderer, fontPath, err := newRenderer(cfg.Font)
	if err != nil {
		log.Fatalf("[mcbot-go] %v", err)
	}

	api := newQQAPI(cfg.AppID, cfg.ClientSecret)
	bot := newBot(cfg, api, renderer)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("[mcbot-go] starting: pid=%d config=%s servers=%d font=%s", os.Getpid(), path, len(cfg.Servers), fontPath)
	log.Printf("[mcbot-go] listening for group and private messages; send /ls")
	gateway := newGateway(api, bot.handleEvent)
	if err := gateway.run(ctx); err != nil {
		log.Fatalf("[mcbot-go] %v", err)
	}
	log.Printf("[mcbot-go] stopped")
}
