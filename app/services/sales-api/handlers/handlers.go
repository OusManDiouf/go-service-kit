// Package handlers manages the different versions of the API.
package handlers

import (
	"context"
	"net/http"
	"os"

	v1 "github.com/ardanlabs/service/app/services/sales-api/handlers/v1"
	"github.com/ardanlabs/service/business/web/auth"
	"github.com/ardanlabs/service/business/web/v1/mid"
	"github.com/ardanlabs/service/foundation/web"
	"github.com/jmoiron/sqlx"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// Options represent optional parameters.
type Options struct {
	corsOrigin string
}

// WithCORS provides configuration options for CORS.
func WithCORS(origin string) func(opts *Options) {
	return func(opts *Options) {
		opts.corsOrigin = origin
	}
}

// APIMuxConfig contains all the mandatory systems required by handlers.
type APIMuxConfig struct {
	Shutdown chan os.Signal
	Log      *zap.SugaredLogger
	Auth     *auth.Auth
	DB       *sqlx.DB
	Tracer   trace.Tracer
}

// APIMux constructs a http.Handler with all application routes defined.
func APIMux(cfg APIMuxConfig, options ...func(opts *Options)) http.Handler {
	var opts Options
	for _, option := range options {
		option(&opts)
	}

	// Construct the web.App which holds all routes as well as common Middleware.
	var app *web.App

	// Do we need CORS?
	if opts.corsOrigin != "" {
		app = web.NewApp(
			cfg.Shutdown,
			cfg.Tracer,
			mid.Logger(cfg.Log),
			mid.Errors(cfg.Log),
			mid.Metrics(),
			mid.Cors(opts.corsOrigin),
			mid.Panics(),
		)

		// Accept CORS 'OPTIONS' preflight requests if config has been provided.
		// Don't forget to apply the CORS middleware to the routes that need it.
		// Example Config: `conf:"default:https://MY_DOMAIN.COM"`
		h := func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
			return nil
		}
		app.Handle(http.MethodOptions, "", "/*", h, mid.Cors(opts.corsOrigin))
	}

	if app == nil {
		app = web.NewApp(
			cfg.Shutdown,
			cfg.Tracer,
			mid.Logger(cfg.Log),
			mid.Errors(cfg.Log),
			mid.Metrics(),
			mid.Panics(),
		)
	}

	// Load the v1 routes.
	v1.Routes(app, v1.Config{
		Log:  cfg.Log,
		Auth: cfg.Auth,
		DB:   cfg.DB,
	})

	return app
}
