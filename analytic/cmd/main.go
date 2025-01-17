package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/go-redis/redis/v8"
	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/wormhole-foundation/wormhole-explorer/analytic/config"
	"github.com/wormhole-foundation/wormhole-explorer/analytic/consumer"
	"github.com/wormhole-foundation/wormhole-explorer/analytic/http/infrastructure"
	"github.com/wormhole-foundation/wormhole-explorer/analytic/metric"
	"github.com/wormhole-foundation/wormhole-explorer/analytic/queue"
	wormscanNotionalCache "github.com/wormhole-foundation/wormhole-explorer/common/client/cache/notional"
	sqs_client "github.com/wormhole-foundation/wormhole-explorer/common/client/sqs"
	health "github.com/wormhole-foundation/wormhole-explorer/common/health"
	"github.com/wormhole-foundation/wormhole-explorer/common/logger"
	"go.uber.org/zap"
)

type exitCode int

func handleExit() {
	if r := recover(); r != nil {
		if e, ok := r.(exitCode); ok {
			os.Exit(int(e))
		}
		panic(r) // not an Exit, bubble up
	}
}

func main() {
	defer handleExit()
	rootCtx, rootCtxCancel := context.WithCancel(context.Background())

	// load config.
	config, err := config.New(rootCtx)
	if err != nil {
		log.Fatal("Error creating config", err)
	}

	// build logger
	logger := logger.New("wormhole-explorer-analytic", logger.WithLevel(config.LogLevel))

	logger.Info("Starting wormhole-explorer-analytic ...")

	// create influxdb client.
	influxCli := newInfluxClient(config.InfluxUrl, config.InfluxToken)
	influxCli.Options().SetBatchSize(100)

	// get health check functions.
	healthChecks, err := newHealthChecks(rootCtx, config, influxCli)
	if err != nil {
		logger.Fatal("failed to create health checks", zap.Error(err))
	}

	//create notional cache
	notionalCache, err := newNotionalCache(rootCtx, config, logger)
	if err != nil {
		logger.Fatal("failed to create notional cache", zap.Error(err))
	}

	// create a metrics instance
	metric, err := metric.New(rootCtx, influxCli, config.InfluxOrganization, config.InfluxBucketInfinite,
		config.InfluxBucket30Days, config.InfluxBucket24Hours, notionalCache, logger)
	if err != nil {
		logger.Fatal("failed to create metrics instance", zap.Error(err))
	}

	// create and start a consumer.
	vaaConsumeFunc := newVAAConsume(rootCtx, config, logger)
	consumer := consumer.New(vaaConsumeFunc, metric.Push, logger, config.P2pNetwork)
	consumer.Start(rootCtx)

	// create and start server.
	server := infrastructure.NewServer(logger, config.Port, config.PprofEnabled, healthChecks...)
	server.Start()

	logger.Info("Started wormhole-explorer-analytic")

	// Waiting for signal
	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-rootCtx.Done():
		logger.Warn("Terminating with root context cancelled.")
	case signal := <-sigterm:
		logger.Info("Terminating with signal.", zap.String("signal", signal.String()))
	}

	logger.Info("root context cancelled, exiting...")
	rootCtxCancel()
	logger.Info("Closing metric client ...")
	metric.Close()
	logger.Info("Closing Http server ...")
	server.Stop()
	logger.Info("Finished wormhole-explorer-analytic")
}

// Creates a callbacks depending on whether the execution is local (memory queue) or not (SQS queue)
func newVAAConsume(appCtx context.Context, config *config.Configuration, logger *zap.Logger) queue.VAAConsumeFunc {
	sqsConsumer, err := newSQSConsumer(appCtx, config)
	if err != nil {
		logger.Fatal("failed to create sqs consumer", zap.Error(err))
	}

	vaaQueue := queue.NewVaaSqs(sqsConsumer, logger)
	return vaaQueue.Consume
}

func newSQSConsumer(appCtx context.Context, config *config.Configuration) (*sqs_client.Consumer, error) {
	awsconfig, err := newAwsConfig(appCtx, config)
	if err != nil {
		return nil, err
	}

	return sqs_client.NewConsumer(awsconfig, config.SQSUrl,
		sqs_client.WithMaxMessages(10),
		sqs_client.WithVisibilityTimeout(120))
}

func newAwsConfig(appCtx context.Context, cfg *config.Configuration) (aws.Config, error) {
	region := cfg.AwsRegion

	if cfg.AwsAccessKeyID != "" && cfg.AwsSecretAccessKey != "" {
		credentials := credentials.NewStaticCredentialsProvider(cfg.AwsAccessKeyID, cfg.AwsSecretAccessKey, "")
		customResolver := aws.EndpointResolverFunc(func(service, region string) (aws.Endpoint, error) {
			if cfg.AwsEndpoint != "" {
				return aws.Endpoint{
					PartitionID:   "aws",
					URL:           cfg.AwsEndpoint,
					SigningRegion: region,
				}, nil
			}

			return aws.Endpoint{}, &aws.EndpointNotFoundError{}
		})

		awsCfg, err := awsconfig.LoadDefaultConfig(appCtx,
			awsconfig.WithRegion(region),
			awsconfig.WithEndpointResolver(customResolver),
			awsconfig.WithCredentialsProvider(credentials),
		)
		return awsCfg, err
	}
	return awsconfig.LoadDefaultConfig(appCtx, awsconfig.WithRegion(region))
}

func newInfluxClient(url, token string) influxdb2.Client {
	return influxdb2.NewClient(url, token)
}

func newHealthChecks(ctx context.Context, config *config.Configuration, influxCli influxdb2.Client) ([]health.Check, error) {
	awsConfig, err := newAwsConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	return []health.Check{health.SQS(awsConfig, config.SQSUrl), health.Influx(influxCli)}, nil
}

func newNotionalCache(
	ctx context.Context,
	cfg *config.Configuration,
	logger *zap.Logger,
) (wormscanNotionalCache.NotionalLocalCacheReadable, error) {

	// use a distributed cache and for notional a pubsub to sync local cache.
	redisClient := redis.NewClient(&redis.Options{Addr: cfg.CacheURL})

	// get notional cache client and init load to local cache
	notionalCache, err := wormscanNotionalCache.NewNotionalCache(ctx, redisClient, cfg.CacheChannel, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create notional cache client: %w", err)
	}
	notionalCache.Init(ctx)

	return notionalCache, nil
}
