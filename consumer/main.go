package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	cpuBurn    = flag.Bool("cpu-burn", false, "Whether to use CPU while processing messages")
	httpAddr   = flag.String("http", ":8080", "Address to listen for requests on")
	redisAddr  = flag.String("redis-server", os.Getenv("REDIS_ADDR"), "Redis server to consume messages from")
	redisQueue = flag.String("redis-queue", os.Getenv("REDIS_QUEUE"), "Redis queue to consume messages from")
	timePerMsg = flag.Duration("per-msg", time.Second, "The amount of time the consumer spends on each message")
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	flag.Parse()

	// This context will get canceled upon receiving a SIGINT signal from the
	// operating system. We use this to shut the consumer down gracefully.
	ctx, shutdown := signal.NotifyContext(context.Background(), os.Interrupt)

	defer shutdown()

	var wg sync.WaitGroup

	// Start consuming messages from Redis.
	client := redis.NewClient(&redis.Options{Addr: *redisAddr})

	// Test the connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := client.Ping(ctx).Result()
	if err != nil {
		return fmt.Errorf("failed to connect to Redis: %w", err)
	}

	log.Println("Successfully connected to Redis")

	c := newConsumer(client, *redisQueue, *timePerMsg, *cpuBurn)
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.consumeMessages(ctx)
	}()

	// Start serving HTTP requests.
	srv := http.Server{Addr: *httpAddr, Handler: c.router}
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Printf("Listening on %s...", *httpAddr)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Printf("Error: unable to listen for requests: %s", err)
			shutdown()
		}
	}()
	// Shut the HTTP server down when the context is cancelled.
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("Error: failed to shut down HTTP server gracefully: %s", err.Error())
		}
	}()

	<-ctx.Done()
	log.Println("Shutting down...")
	wg.Wait()

	return nil
}

// =============================================================================
// Consumer
// =============================================================================

type consumer struct {
	wg sync.WaitGroup

	router *mux.Router

	client *redis.Client
	queue  string

	perMsg  time.Duration
	cpuBurn bool

	healthy bool

	consumed prometheus.Counter
}

func newConsumer(client *redis.Client, queue string, perMsg time.Duration, cpuBurn bool) *consumer {
	c := consumer{
		router:  mux.NewRouter(),
		client:  client,
		queue:   queue,
		perMsg:  perMsg,
		cpuBurn: cpuBurn,
		healthy: true,
		consumed: promauto.NewCounter(prometheus.CounterOpts{
			Name: "consumer_messages_consumed_total",
			Help: "The total number of consumed messages",
		}),
	}

	c.router.HandleFunc("/healthz", c.handleHealthcheck).Methods("GET")
	c.router.Handle("/metrics", promhttp.Handler())

	return &c
}

// =============================================================================
// HTTP handlers
// =============================================================================

func (p *consumer) handleHealthcheck(w http.ResponseWriter, r *http.Request) {
	switch p.healthy {
	case true:
		fmt.Fprintf(w, "The server is healthy")
	case false:
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "The server is unhealthy: the last attempt to publish a message failed")
	}
}

// =============================================================================
// Message consumption
// =============================================================================

func (c *consumer) consumeMessages(ctx context.Context) {
	messages := make(chan string)
	go func() {
		var backoff time.Duration

		for {
			select {
			case <-ctx.Done():
				close(messages)
				return
			case <-time.After(backoff):
				result, err := c.client.BLPop(ctx, 0, c.queue).Result()
				if err != nil {
					backoff = min(backoff+time.Second, 5*time.Second)
					log.Printf("Error getting message from queue: %s", err.Error())
					continue
				}
				backoff = 0
				messages <- result[1]
			}
		}
	}()

	log.Println("Starting to comsume messages...")

	for msg := range messages {
		log.Printf("Received a message: %q.", msg)
		log.Println("Processing message...")

		if c.cpuBurn {
			done := time.After(c.perMsg)
			for {
				select {
				case <-done:
					return
				default:
					time.Sleep(1 * time.Second)
				}
			}
		} else {
			time.Sleep(c.perMsg)
		}

		c.consumed.Inc()
		log.Println("Message processed.")
	}

	log.Println("Done consuming messages.")
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
