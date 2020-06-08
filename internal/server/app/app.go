package app

import (
	"sync"

	"github.com/jkuri/abstruse/internal/server/db/repository"
	"github.com/jkuri/abstruse/internal/server/options"
	"github.com/jkuri/abstruse/internal/server/websocket"
	"go.etcd.io/etcd/clientv3"
	"go.uber.org/zap"
)

// App represent main gRPC application and holds data
// for established worker connections.
type App struct {
	mu        sync.RWMutex
	opts      *options.Options
	Workers   map[string]*worker
	client    *clientv3.Client
	ws        *websocket.App
	logger    *zap.SugaredLogger
	scheduler *scheduler
	errch     chan error

	buildRepository repository.BuildRepository
	jobRepository   repository.JobRepository
	repoRepository  repository.RepoRepository
}

// NewApp returns new instance of App.
func NewApp(opts *options.Options, ws *websocket.App, rr repository.RepoRepository, jr repository.JobRepository, br repository.BuildRepository, logger *zap.Logger) (*App, error) {
	app := &App{
		opts:            opts,
		Workers:         make(map[string]*worker),
		ws:              ws,
		buildRepository: br,
		jobRepository:   jr,
		repoRepository:  rr,
		logger:          logger.With(zap.String("type", "app")).Sugar(),
		errch:           make(chan error),
	}

	return app, nil
}

// Start starts gRPC application.
func (app *App) Start(client *clientv3.Client) error {
	app.client = client

	go func() {
		if err := app.watchWorkers(); err != nil {
			app.errch <- err
		}
	}()

	go func() {
		app.scheduler = newScheduler(app.client, app.logger.Desugar(), app)
		if err := app.scheduler.run(); err != nil {
			app.errch <- err
		}
	}()

	return <-app.errch
}

func (app *App) getCapacity() (int32, int32) {
	var max, running int32
	app.mu.Lock()
	defer app.mu.Unlock()
	for _, w := range app.Workers {
		max += w.max
		running += w.running
	}
	return max, running
}
