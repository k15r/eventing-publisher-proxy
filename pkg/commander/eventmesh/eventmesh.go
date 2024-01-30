package eventmesh

import (
	"context"

	"github.com/kelseyhightower/envconfig"
	"github.com/kyma-project/eventing-publisher-proxy/pkg/application"
	"github.com/kyma-project/eventing-publisher-proxy/pkg/cloudevents/builder"
	"github.com/kyma-project/eventing-publisher-proxy/pkg/cloudevents/eventtype"
	"github.com/kyma-project/eventing-publisher-proxy/pkg/env"
	"github.com/kyma-project/eventing-publisher-proxy/pkg/handler"
	"github.com/kyma-project/eventing-publisher-proxy/pkg/handler/health"
	"github.com/kyma-project/eventing-publisher-proxy/pkg/informers"
	"github.com/kyma-project/eventing-publisher-proxy/pkg/legacy"
	"github.com/kyma-project/eventing-publisher-proxy/pkg/metrics"
	"github.com/kyma-project/eventing-publisher-proxy/pkg/oauth"
	"github.com/kyma-project/eventing-publisher-proxy/pkg/options"
	"github.com/kyma-project/eventing-publisher-proxy/pkg/receiver"
	"github.com/kyma-project/eventing-publisher-proxy/pkg/sender/eventmesh"
	"github.com/kyma-project/eventing-publisher-proxy/pkg/signals"
	"github.com/kyma-project/eventing-publisher-proxy/pkg/subscribed"
	"go.uber.org/zap"
	"golang.org/x/xerrors"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/kyma-project/eventing-manager/pkg/backend/cleaner"
	"github.com/kyma-project/eventing-manager/pkg/logger"

	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp" // TODO: remove as this is only used in a development setup
)

const (
	backend       = "beb"
	commanderName = backend + "-commander"
)

// Commander implements the Commander interface.
type Commander struct {
	cancel           context.CancelFunc
	envCfg           *env.EventMeshConfig
	logger           *logger.Logger
	metricsCollector *metrics.Collector
	opts             *options.Options
}

// NewCommander creates the Commander for publisher to EventMesh.
func NewCommander(opts *options.Options, metricsCollector *metrics.Collector, logger *logger.Logger) *Commander {
	return &Commander{
		metricsCollector: metricsCollector,
		logger:           logger,
		envCfg:           new(env.EventMeshConfig),
		opts:             opts,
	}
}

// Init implements the Commander interface and initializes the publisher to BEB.
func (c *Commander) Init() error {
	if err := envconfig.Process("", c.envCfg); err != nil {
		return xerrors.Errorf("failed to read configuration for %s : %v", commanderName, err)
	}
	return nil
}

// Start implements the Commander interface and starts the publisher.
func (c *Commander) Start() error {
	c.namedLogger().Infow("Starting Event Publisher", "configuration", c.envCfg.String(), "startup arguments", c.opts)

	// configure message receiver
	messageReceiver := receiver.NewHTTPMessageReceiver(c.envCfg.Port)

	// assure uniqueness
	var ctx context.Context
	ctx, c.cancel = context.WithCancel(signals.NewContext())

	// configure auth client
	client := oauth.NewClient(ctx, c.envCfg)
	defer client.CloseIdleConnections()

	// configure message sender
	messageSender := eventmesh.NewSender(c.envCfg.EventMeshPublishURL, client, c.logger)

	// cluster config
	k8sConfig := config.GetConfigOrDie()

	// setup application lister
	var applicationLister *application.Lister
	if c.envCfg.ApplicationCRDEnabled {
		dynamicClient := dynamic.NewForConfigOrDie(k8sConfig)
		applicationLister = application.NewLister(ctx, dynamicClient)
		c.namedLogger().Info("Application CR lister is enabled!")
	} else {
		c.namedLogger().Info("Application CR lister is disabled!")
	}

	// configure legacyTransformer
	legacyTransformer := legacy.NewTransformer(
		c.envCfg.EventMeshNamespace,
		c.envCfg.EventTypePrefix,
		applicationLister,
	)

	// Configure Subscription Lister
	subDynamicSharedInfFactory := subscribed.GenerateSubscriptionInfFactory(k8sConfig)
	subLister := subDynamicSharedInfFactory.ForResource(subscribed.SubscriptionGVR()).Lister()
	subscribedProcessor := &subscribed.Processor{
		SubscriptionLister: &subLister,
		Prefix:             c.envCfg.EventTypePrefix,
		Namespace:          c.envCfg.EventMeshNamespace,
		Logger:             c.logger,
	}
	// Sync informer cache or die
	c.namedLogger().Info("Waiting for informers caches to sync")
	informers.WaitForCacheSyncOrDie(ctx, subDynamicSharedInfFactory, c.logger)
	c.namedLogger().Info("Informers were successfully synced")

	// configure event type cleaner
	eventTypeCleanerV1 := eventtype.NewCleaner(c.envCfg.EventTypePrefix, applicationLister, c.logger)

	// configure event type cleaner for subscription CRD v1alpha2
	eventTypeCleaner := cleaner.NewEventMeshCleaner(c.logger)

	// configure cloud event builder for subscription CRD v1alpha2
	ceBuilder := builder.NewEventMeshBuilder(c.envCfg.EventTypePrefix, c.envCfg.EventMeshNamespace, eventTypeCleaner,
		applicationLister, c.logger)

	// start handler which blocks until it receives a shutdown signal
	if err := handler.New(
		messageReceiver,
		messageSender,
		health.NewChecker(),
		c.envCfg.RequestTimeout,
		legacyTransformer,
		c.opts,
		subscribedProcessor,
		c.logger,
		c.metricsCollector,
		eventTypeCleanerV1,
		ceBuilder,
		c.envCfg.EventTypePrefix,
		env.EventMeshBackend,
	).Start(ctx); err != nil {
		return xerrors.Errorf("failed to start handler for %s : %v", commanderName, err)
	}
	c.namedLogger().Info("Event Publisher was shut down")
	return nil
}

// Stop implements the Commander interface and stops the publisher.
func (c *Commander) Stop() error {
	c.cancel()
	return nil
}

func (c *Commander) namedLogger() *zap.SugaredLogger {
	return c.logger.WithContext().Named(commanderName).With("backend", backend)
}
