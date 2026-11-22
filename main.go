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

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "init":
			dir := ""
			if len(os.Args) > 2 {
				dir = os.Args[2]
			}
			if len(os.Args) > 3 {
				log.Fatalf("[mcbot-go] usage: mcbot-go init [dir]")
			}
			if err := runInit(dir, os.Stdin, os.Stdout); err != nil {
				log.Fatalf("[mcbot-go] %v", err)
			}
			return
		default:
			log.Fatalf("[mcbot-go] unknown command %q; usage: mcbot-go [init [dir]]", os.Args[1])
		}
	}

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

	dataPath, err := dataDir()
	if err != nil {
		log.Fatalf("[mcbot-go] %v", err)
	}

	api := newQQAPI(cfg.AppID, cfg.ClientSecret)
	bot := newBot(api, renderer, newSessionStore(dataPath))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("[mcbot-go] starting: pid=%d config=%s data=%s font=%s", os.Getpid(), path, dataPath, fontPath)
	log.Printf("[mcbot-go] listening for group and private messages; send /ls")
	gateway := newGateway(api, bot.handleEvent)
	if err := gateway.run(ctx); err != nil {
		log.Fatalf("[mcbot-go] %v", err)
	}
	log.Printf("[mcbot-go] stopped")
}
