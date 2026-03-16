package cassandra

import (
	"sync"

	"github.com/gocql/gocql"
	"go.temporal.io/server/common/config"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/log/tag"
	"go.temporal.io/server/common/metrics"
	p "go.temporal.io/server/common/persistence"
	commongocql "go.temporal.io/server/common/persistence/nosql/nosqlplugin/cassandra/gocql"
	"go.temporal.io/server/common/persistence/serialization"
	"go.temporal.io/server/common/resolver"
)

type (
	// Factory vends datastore implementations backed by cassandra
	Factory struct {
		sync.RWMutex
		cfg            config.Cassandra
		clusterName    string
		logger         log.Logger
		defaultSession commongocql.Session
		groupSessions  map[config.CassandraSessionGroupName]commongocql.Session
		serializer     serialization.Serializer
	}
)

// NewFactory returns an instance of a factory object which can be used to create
// data stores that are backed by cassandra
func NewFactory(
	cfg config.Cassandra,
	r resolver.ServiceResolver,
	clusterName string,
	logger log.Logger,
	metricsHandler metrics.Handler,
	serializer serialization.Serializer,
) *Factory {
	session, err := commongocql.NewSession(
		func() (*gocql.ClusterConfig, error) {
			return commongocql.NewCassandraCluster(cfg, r)
		},
		logger,
		metricsHandler,
	)
	if err != nil {
		logger.Fatal("unable to initialize cassandra session", tag.Error(err))
	}

	validGroups := map[config.CassandraSessionGroupName]struct{}{
		config.CassandraSessionGroupExecution: {},
		config.CassandraSessionGroupMatching:  {},
		config.CassandraSessionGroupMetadata:  {},
		config.CassandraSessionGroupQueue:     {},
	}

	groupSessions := make(map[config.CassandraSessionGroupName]commongocql.Session)
	for groupName, override := range cfg.SessionGroups {
		if _, ok := validGroups[groupName]; !ok {
			logger.Fatal("invalid cassandra session group name",
				tag.NewStringTag("group", string(groupName)),
			)
		}
		groupCfg := cfg
		if override.MaxConns > 0 {
			groupCfg.MaxConns = override.MaxConns
		}
		gs, err := commongocql.NewSession(
			func() (*gocql.ClusterConfig, error) {
				return commongocql.NewCassandraCluster(groupCfg, r)
			},
			logger,
			metricsHandler,
		)
		if err != nil {
			logger.Fatal("unable to initialize cassandra session for group",
				tag.NewStringTag("group", string(groupName)),
				tag.Error(err),
			)
		}
		groupSessions[groupName] = gs
	}

	return &Factory{
		cfg:            cfg,
		clusterName:    clusterName,
		logger:         logger,
		defaultSession: session,
		groupSessions:  groupSessions,
		serializer:     serializer,
	}
}

// NewFactoryFromSession returns an instance of a factory object from the given session.
func NewFactoryFromSession(
	cfg config.Cassandra,
	clusterName string,
	logger log.Logger,
	session commongocql.Session,
	serializer serialization.Serializer,
) *Factory {
	return &Factory{
		cfg:            cfg,
		clusterName:    clusterName,
		logger:         logger,
		defaultSession: session,
		serializer:     serializer,
	}
}

// sessionFor returns the session for the given group, falling back to the default session.
func (f *Factory) sessionFor(group config.CassandraSessionGroupName) commongocql.Session {
	if s, ok := f.groupSessions[group]; ok {
		return s
	}
	return f.defaultSession
}

// NewTaskStore returns a new task store
func (f *Factory) NewTaskStore() (p.TaskStore, error) {
	return NewMatchingTaskStore(f.sessionFor(config.CassandraSessionGroupMatching), f.logger, false), nil
}

// NewFairTaskStore returns a new task store with fairness enabled
func (f *Factory) NewFairTaskStore() (p.TaskStore, error) {
	return NewMatchingTaskStore(f.sessionFor(config.CassandraSessionGroupMatching), f.logger, true), nil
}

// NewShardStore returns a new shard store
func (f *Factory) NewShardStore() (p.ShardStore, error) {
	return NewShardStore(f.clusterName, f.sessionFor(config.CassandraSessionGroupExecution), f.logger), nil
}

// NewMetadataStore returns a metadata store
func (f *Factory) NewMetadataStore() (p.MetadataStore, error) {
	return NewMetadataStore(f.clusterName, f.sessionFor(config.CassandraSessionGroupMetadata), f.logger)
}

// NewClusterMetadataStore returns a metadata store
func (f *Factory) NewClusterMetadataStore() (p.ClusterMetadataStore, error) {
	return NewClusterMetadataStore(f.sessionFor(config.CassandraSessionGroupMetadata), f.logger)
}

// NewExecutionStore returns a new ExecutionStore.
func (f *Factory) NewExecutionStore() (p.ExecutionStore, error) {
	return NewExecutionStore(f.sessionFor(config.CassandraSessionGroupExecution), f.serializer, f.logger), nil
}

// NewQueue returns a new queue backed by cassandra
func (f *Factory) NewQueue(queueType p.QueueType) (p.Queue, error) {
	return NewQueueStore(queueType, f.sessionFor(config.CassandraSessionGroupQueue), f.logger)
}

// NewQueueV2 returns a new data-access object for queues and messages stored in Cassandra. It will never return an
// error.
func (f *Factory) NewQueueV2() (p.QueueV2, error) {
	return NewQueueV2Store(f.sessionFor(config.CassandraSessionGroupQueue), f.logger), nil
}

// NewNexusEndpointStore returns a new NexusEndpointStore
func (f *Factory) NewNexusEndpointStore() (p.NexusEndpointStore, error) {
	return NewNexusEndpointStore(f.sessionFor(config.CassandraSessionGroupMetadata), f.logger), nil
}

// Close closes the factory
func (f *Factory) Close() {
	f.Lock()
	defer f.Unlock()
	f.defaultSession.Close()
	for _, s := range f.groupSessions {
		s.Close()
	}
}
