package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"gateway.jackwillis517.net/internal/data"
	"gateway.jackwillis517.net/internal/jsonlog"
	"gateway.jackwillis517.net/internal/provider"
	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
)

var version = "1.0.0"

type config struct {
	port            int
	env             string
	version         string
	upstreamTimeout int
	db              struct {
		dsn          string
		maxOpenConns int
		maxIdleConns int
		maxIdleTime  string
	}
	limiter struct {
		rps     float64
		burst   int
		enabled bool
	}
}

type application struct {
	config    config
	logger    *jsonlog.Logger
	models    data.Models
	providers map[string]provider.Provider
	wg        sync.WaitGroup
}

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Println("No .env file found, relying on system environment variables")
	}

	var cfg config
	cfg.version = version

	flag.IntVar(&cfg.port, "port", 4005, "API server port")
	flag.StringVar(&cfg.env, "env", "development", "Environment (development|staging|production)")
	flag.IntVar(&cfg.upstreamTimeout, "upstream-timeout", 120, "max time (sec) for one providers call")
	flag.StringVar(&cfg.db.dsn, "db-dsn", os.Getenv("GATEWAY_DB_DSN"), "PostgreSQL DSN")

	flag.IntVar(&cfg.db.maxOpenConns, "db-max-open-conns", 25, "PostgreSQL max open connections")
	flag.IntVar(&cfg.db.maxIdleConns, "db-max-idle-conns", 25, "PostgreSQL max idle connections")
	flag.StringVar(&cfg.db.maxIdleTime, "db-max-idle-time", "15m", "PostgreSQL max connection idle time")

	flag.Float64Var(&cfg.limiter.rps, "limiter-rps", 2, "Rate limiter maximum requests per second")
	flag.IntVar(&cfg.limiter.burst, "limiter-burst", 4, "Rate limiter maximum burst")
	flag.BoolVar(&cfg.limiter.enabled, "limiter-enabled", true, "Enable rate limiter")

	healthcheck := flag.Bool("healthcheck", false, "Run a health check against the API server and exit")

	flag.Parse()

	logger := jsonlog.New(os.Stdout, jsonlog.LevelInfo)

	// Run the health check before opening the database, so the container probe
	// stays green while the database is down or still starting.
	if *healthcheck {
		if err := probeHealthcheck(cfg.port); err != nil {
			logger.PrintFatal(err, nil)
		}
		logger.PrintInfo("healthcheck passed", nil)
		os.Exit(0)
	}

	db, err := openDB(cfg)
	if err != nil {
		logger.PrintFatal(err, nil)
	}
	defer db.Close()

	logger.PrintInfo("database connection pool established", nil)

	app := &application{
		config:    cfg,
		logger:    logger,
		models:    data.NewModels(db),
		providers: buildProviders(logger),
	}

	err = app.serve()
	if err != nil {
		logger.PrintFatal(err, nil)
	}
}

func probeHealthcheck(port int) error {
	client := http.Client{Timeout: 5 * time.Second}

	resp, err := client.Get(fmt.Sprintf("http://localhost:%d/v1/healthcheck", port))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck returned status %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "status: available") {
		return fmt.Errorf("unexpected healthcheck response: %q", string(body))
	}
	return nil
}

func openDB(cfg config) (*sql.DB, error) {
	db, err := sql.Open("postgres", cfg.db.dsn)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = db.PingContext(ctx)
	if err != nil {
		return nil, err
	}

	return db, nil
}

func buildProviders(logger *jsonlog.Logger) map[string]provider.Provider {
	providers := make(map[string]provider.Provider)
	fmt.Println(os.Getenv("OLLAMA_BASE_URL"))

	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		providers["openai"] = provider.NewOpenAI(key)
	}
	if url := os.Getenv("OLLAMA_BASE_URL"); url != "" {
		providers["ollama"] = provider.NewOllama(url)
	}

	if len(providers) == 0 {
		logger.PrintInfo("no providers configured; set OPENAI_API_KEY and/or OLLAMA_BASE_URL", map[string]string{})
	}
	for name := range providers {
		logger.PrintInfo("provider enabled", map[string]string{"provider": name})
	}
	return providers
}
