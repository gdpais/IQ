package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"incidentiq/apps/api/internal/config"
	"incidentiq/apps/api/internal/httpapi"
	"incidentiq/apps/api/internal/platform/db"
	"incidentiq/apps/api/internal/platform/redisx"
	"incidentiq/apps/api/internal/tickettemplate"
)

func main() {
	ctx := context.Background()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	pool, err := db.Open(ctx, cfg.DBURL)
	if err != nil {
		log.Fatalf("db open error: %v", err)
	}
	defer pool.Close()

	redisClient, err := redisx.Open(ctx, cfg.RedisAddr, cfg.RedisDB)
	if err != nil {
		log.Fatalf("redis open error: %v", err)
	}
	defer redisClient.Close()

	if cfg.TicketTemplatePath != "" {
		raw, doc, validationErrors, err := tickettemplate.LoadFile(cfg.TicketTemplatePath)
		if err != nil {
			log.Fatalf("ticket template load error: %v", err)
		}
		if len(validationErrors) > 0 {
			log.Fatalf("ticket template validation errors: %v", validationErrors)
		}
		if err := tickettemplate.NewRepository(pool).SaveDocument(ctx, raw, doc, "startup"); err != nil {
			log.Fatalf("ticket template save error: %v", err)
		}
		log.Printf("loaded ticket template version %d from %s", doc.Version, cfg.TicketTemplatePath)
	}

	srv := httpapi.New(cfg, pool, redisClient)
	httpServer := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("api listening on :%s", cfg.Port)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
}
