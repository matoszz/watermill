package main

import (
	"github.com/pkg/errors"
	"time"
	"fmt"
	"sync/atomic"
	"github.com/roblaszczak/gooddd/message/handler/plugin"
	"github.com/roblaszczak/gooddd/message/handler/middleware"

	"github.com/rcrowley/go-metrics"
	"github.com/roblaszczak/gooddd/message/handler"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/deathowl/go-metrics-prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"net/http"
	"log"
	"os"
	"github.com/confluentinc/confluent-kafka-go/kafka"
	"github.com/roblaszczak/gooddd/message"
	"github.com/roblaszczak/gooddd/message/infrastructure/kafka/confluent"

	_ "net/http/pprof"
	"github.com/roblaszczak/gooddd/message/marshal"
	"github.com/roblaszczak/gooddd"
	"github.com/roblaszczak/gooddd/message/infrastructure/kafka/sarama"
	"github.com/satori/go.uuid"
)

// todo - doc why separated type
type postAdded struct {
	Author     string    `json:"author"`
	Title      string    `json:"title"`
	OccurredOn time.Time `json:"occurred_on"`
}

type postsCountUpdated struct {
	NewCount int64 `json:"new_count"`
}

// todo - replace with redis?
type countStorage interface {
	CountAdd() (int64, error)
	Count() (int64, error)
}

type memoryCountStorage struct {
	count *int64
}

func (m memoryCountStorage) CountAdd() (int64, error) {
	return atomic.AddInt64(m.count, 1), nil
}

func (m memoryCountStorage) Count() (int64, error) {
	return atomic.LoadInt64(m.count), nil
}

type PostsCounter struct {
	countStorage countStorage
}

func (p PostsCounter) Count(msg message.Message) ([]message.Message, error) {
	newCount, err := p.countStorage.CountAdd()
	if err != nil {
		return nil, errors.Wrap(err, "cannot add count")
	}

	if newCount%100000 == 0 {
		fmt.Println("> new count:", newCount)
	}

	producedMsg := postsCountUpdated{NewCount: newCount}
	//producedMsg.

	return []message.Message{message.NewDefault(uuid.NewV4().String(), producedMsg)}, nil
}

// todo - replace with mongo?
type feedStorage interface {
	AddToFeed(title, author string, time time.Time) error
}

// todo - doc (stub)
type printFeedStorage struct{}

func (printFeedStorage) AddToFeed(title, author string, time time.Time) error {
	//fmt.Printf("Adding to feed: %s by %s @%s\n", title, author, time)
	return nil
}

type FeedGenerator struct {
	feedStorage feedStorage
}

func (f FeedGenerator) UpdateFeed(message message.Message) ([]message.Message, error) {
	event := postAdded{}
	if err := message.UnmarshalPayload(&event); err != nil {
		return nil, err
	}

	err := f.feedStorage.AddToFeed(event.Title, event.Author, event.OccurredOn)
	if err != nil {
		return nil, errors.Wrap(err, "cannot update feed")
	}

	return nil, nil
}

func main() {
	go func() {
		log.Println(http.ListenAndServe("localhost:6060", nil))
	}()

	logger := gooddd.NewStdLogger(false, false)
	//logger := gooddd.NopLogger{}

	t := metrics.NewTimer()
	metrics.Register("handler.time", t)

	errs := metrics.NewCounter()
	metrics.Register("handler.errors", errs)

	success := metrics.NewCounter()
	metrics.Register("handler.success", success)

	// todo - use not default registry
	// todo - rewrite
	pClient := prometheusmetrics.NewPrometheusProvider(
		metrics.DefaultRegistry,
		"test",
		"subsys",
		prometheus.DefaultRegisterer,
		1*time.Second,
	)
	go pClient.UpdatePrometheusMetrics()

	http.Handle("/metrics", promhttp.Handler())
	go http.ListenAndServe(":9000", nil)

	counter := PostsCounter{memoryCountStorage{new(int64)}}
	feedGenerator := FeedGenerator{printFeedStorage{}}

	// todo - move this boilerplate somewhere, to make examples more clear
	listenerFactory := confluent.NewConfluentKafka(func(kafkaMsg *kafka.Message) (message.Message, error) {
		return marshal.UnmarshalJson(kafkaMsg.Value)
	}, func(subscriberMeta message.SubscriberMetadata) string {
		return fmt.Sprintf("%s_%s_v14", subscriberMeta.ServerName, subscriberMeta.SubscriberName)
	}, logger)

	publisher, err := sarama.NewSimpleSyncProducer("todo", []string{"localhost:9092"}, marshal.Json)
	if err != nil {
		panic(err)
	}

	router := handler.NewRouter("example", listenerFactory, publisher)
	router.Logger = logger

	metricsMiddleware := middleware.NewMetrics(t, errs, success)
	metricsMiddleware.ShowStats(time.Second*5, log.New(os.Stderr, "metrics: ", log.Lmicroseconds))

	retryMiddleware := middleware.NewRetry()
	retryMiddleware.OnRetryHook = func(retryNum int, delay time.Duration) {
		//fmt.Println("retrying, num:", retryNum, "delay:", delay)
	}
	retryMiddleware.MaxRetries = 1
	retryMiddleware.WaitTime = time.Millisecond * 10

	throttle, err := middleware.NewThrottlePerSecond(1, logger)
	if err != nil {
		panic(err)
	}

	router.AddMiddleware(
		metricsMiddleware.Middleware,
		middleware.AckOnSuccess,
		throttle.Middleware,
		//middleware.PoisonQueueHook(func(message *message.Message, err error) {
		//	fmt.Println("unable to process", message, "err:", err)
		//}),
		retryMiddleware.Middleware,
		middleware.Recoverer,
		middleware.CorrelationUUID,
		middleware.RandomFail(1),
		//middleware.RandomPanic(0.002),
	)

	router.AddPlugin(plugin.SignalsHandler)

	router.Subscribe(
		"posts_counter",
		"test_topic",
		counter.Count,
	)
	router.Subscribe(
		"feed_generator",
		"test_topic",
		feedGenerator.UpdateFeed,
	)

	router.Run()
}
