package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"deposit-escrow/internal/api"
	"deposit-escrow/internal/app"
	"deposit-escrow/internal/auth"
	"deposit-escrow/internal/config"
	"deposit-escrow/internal/db"
	"deposit-escrow/internal/events"

	"github.com/redis/go-redis/v9"
	"github.com/segmentio/kafka-go"
)

func main() {
	cfg := config.Load()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("connect postgres: %v", err)
	}
	defer pool.Close()
	if cfg.AutoMigrate {
		if err := db.Migrate(ctx, pool); err != nil {
			log.Fatalf("migrate: %v", err)
		}
		log.Println("[boot] migrations applied")
	}

	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("connect redis: %v", err)
	}

	authMgr := auth.NewManager(rdb, pool)
	var seed *app.SeedUsers
	if cfg.Seed {
		seed, err = app.Seed(ctx, pool, authMgr)
		if err != nil {
			log.Fatalf("seed: %v", err)
		}
		log.Printf("[boot] seed users ready: tenant=%s landlord=%s property=%s",
			seed.TenantID, seed.LandlordID, seed.PropertyID)
	}

	var writer *kafka.Writer
	if !cfg.KafkaDisabled {
		writer = &kafka.Writer{
			Addr:                   kafka.TCP(cfg.KafkaBrokers),
			Topic:                  cfg.KafkaTopic,
			AllowAutoTopicCreation: true,
			Balancer:               &kafka.Hash{},
		}
		// relay 自带失败重试; 即使此刻 broker 暂不可达也启动, 恢复后自动补发 outbox。
		relay := events.NewRelay(pool, writer, cfg.KafkaTopic)
		go relay.Run(ctx)
		log.Println("[boot] kafka outbox relay started")
	}

	svc := app.New(pool, rdb, writer, cfg.KafkaTopic)
	queries := app.NewQueries(pool)
	srv := api.NewServer(svc, queries, authMgr, rdb)

	// 房东超时验房定时巡检: 首次期限二次提醒/逾期标记, 最终期限三检查+状态指纹后自动释放或转人工。
	go app.RunOverdueSweeper(ctx, pool, svc, time.Duration(cfg.OverdueSweepSecs)*time.Second)

	mux := http.NewServeMux()
	mux.Handle("/swagger/", api.SwaggerHandler())
	mux.Handle("/openapi.yaml", api.OpenAPIHandler())
	mux.Handle("/", srv.Routes())

	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("[boot] listening on %s (swagger: http://localhost%s/swagger/)", cfg.HTTPAddr, cfg.HTTPAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("[shutdown] draining ...")
	shCtx, shCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shCancel()
	_ = httpSrv.Shutdown(shCtx)
	if writer != nil {
		_ = writer.Close()
	}
	_ = rdb.Close()
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}
