package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/pendig/rute-bayar/internal/build"
	"github.com/pendig/rute-bayar/internal/config"
	"github.com/pendig/rute-bayar/internal/domain"
	"github.com/pendig/rute-bayar/internal/mcpserver"
	"github.com/pendig/rute-bayar/internal/metrics"
	"github.com/pendig/rute-bayar/internal/paymentsvc"
	"github.com/pendig/rute-bayar/internal/providerfactory"
	"github.com/pendig/rute-bayar/internal/router"
	"github.com/pendig/rute-bayar/internal/routetoken"
	"github.com/pendig/rute-bayar/internal/routingsvc"
	"github.com/pendig/rute-bayar/internal/storage/sqlite"
)

// routingPlane bundles the Wishpull v0.3 components for one process.
type routingPlane struct {
	routing  *routingsvc.Service
	metrics  *metrics.Set
	mcp      *mcpserver.Server
	factory  *providerfactory.Factory
	payments *paymentsvc.Service
}

// accountLoader returns the credential loader: environment variables (fed
// from Secret Manager) first, then the SQLite provider_accounts table.
func accountLoader(store *sqlite.Store) *providerfactory.EnvAccountLoader {
	return providerfactory.NewEnvAccountLoader(store)
}

// envAwareStore lets paymentsvc see env-provided accounts too.
type envAwareStore struct {
	*sqlite.Store
	loader *providerfactory.EnvAccountLoader
}

func (s envAwareStore) GetProviderAccount(ctx context.Context, code domain.ProviderCode, env domain.Environment) (domain.ProviderAccount, error) {
	return s.loader.GetProviderAccount(ctx, code, env)
}

func buildRoutingPlane(cfg config.Config, store *sqlite.Store, env domain.Environment) (*routingPlane, error) {
	if err := validateKeySeparation(cfg); err != nil {
		return nil, err
	}
	routerCfg, err := router.LoadConfig(cfg.RoutingConfigPath)
	if err != nil {
		return nil, err
	}

	loader := accountLoader(store)
	factory := providerfactory.New(loader)
	payments := paymentsvc.New(envAwareStore{Store: store, loader: loader}, factory)

	available := func(ctx context.Context, code domain.ProviderCode, environment domain.Environment) bool {
		_, err := loader.GetProviderAccount(ctx, code, environment)
		return err == nil
	}
	r := router.New(routerCfg, router.NewHealth(routerCfg.Breaker), available)

	var issuer *routetoken.Issuer
	if strings.TrimSpace(cfg.RoutingTokenKeys) != "" {
		signer, err := routetoken.ParseKeys(cfg.RoutingTokenKeys)
		if err != nil {
			return nil, fmt.Errorf("RUTE_BAYAR_ROUTING_TOKEN_KEYS: %w", err)
		}
		issuer = routetoken.NewIssuer(signer, cfg.RoutingTokenTTL)
	}

	m := metrics.NewSet()
	registerGauges(m, r, store)
	routing := routingsvc.New(r, issuer, payments, store, m, routingsvc.Config{Environment: env})
	return &routingPlane{
		routing:  routing,
		metrics:  m,
		mcp:      mcpserver.New(routing, store, m, strings.TrimSpace(build.Version)),
		factory:  factory,
		payments: payments,
	}, nil
}

// validateKeySeparation refuses configurations where one key would hold
// two roles, which would let an agent key execute charges.
func validateKeySeparation(cfg config.Config) error {
	keys := map[string]string{}
	for name, value := range map[string]string{
		"RUTE_BAYAR_API_KEY":          cfg.APIKey,
		"RUTE_BAYAR_AGENT_API_KEY":    cfg.AgentAPIKey,
		"RUTE_BAYAR_EXECUTOR_API_KEY": cfg.ExecutorAPIKey,
	} {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if len(value) < 24 {
			return fmt.Errorf("%s must be at least 24 characters", name)
		}
		if other, dup := keys[value]; dup {
			return fmt.Errorf("%s and %s must be different keys", other, name)
		}
		keys[value] = name
	}
	return nil
}

func registerGauges(m *metrics.Set, r *router.Router, store *sqlite.Store) {
	m.Registry.NewGaugeFunc("rutebayar_gateway_breaker_open", "1 when the gateway circuit is open.", func() []metrics.Sample {
		out := []metrics.Sample{}
		for _, snap := range r.Health().SnapshotAll(domain.SupportedProviders()) {
			v := 0.0
			if snap.Breaker == router.BreakerOpen {
				v = 1
			}
			out = append(out, metrics.Sample{LabelValues: []string{string(snap.Provider)}, Value: v})
		}
		return out
	}, "provider")
	m.Registry.NewGaugeFunc("rutebayar_gateway_p95_latency_seconds", "Observed p95 create-payment latency.", func() []metrics.Sample {
		out := []metrics.Sample{}
		for _, snap := range r.Health().SnapshotAll(domain.SupportedProviders()) {
			out = append(out, metrics.Sample{LabelValues: []string{string(snap.Provider)}, Value: float64(snap.P95LatencyMs) / 1000})
		}
		return out
	}, "provider")
	m.Registry.NewGaugeFunc("rutebayar_outbox_events", "Outbound Wishpull events by status.", func() []metrics.Sample {
		out := []metrics.Sample{}
		for _, status := range []domain.OutboundEventStatus{domain.OutboundEventPending, domain.OutboundEventDead} {
			n, err := store.CountOutboundEvents(context.Background(), status)
			if err != nil {
				continue
			}
			out = append(out, metrics.Sample{LabelValues: []string{string(status)}, Value: float64(n)})
		}
		return out
	}, "status")
}

// productionWarnings prints risky settings for a production instance.
func productionWarnings(w io.Writer, cfg config.Config, env domain.Environment) {
	if env != domain.EnvironmentProduction {
		return
	}
	if !cfg.RequireMandate {
		fmt.Fprintln(w, "WARNING: RUTE_BAYAR_REQUIRE_MANDATE is off; POST /api/v1/payments can charge without a mandate.")
	}
	if strings.TrimSpace(cfg.RoutingTokenKeys) == "" {
		fmt.Fprintln(w, "WARNING: RUTE_BAYAR_ROUTING_TOKEN_KEYS is empty; routing tokens and execution are disabled.")
	}
	if strings.TrimSpace(cfg.EventsURL) == "" {
		fmt.Fprintln(w, "WARNING: RUTE_BAYAR_WISHPULL_EVENTS_URL is empty; Wishpull will not receive payment callbacks.")
	}
	if strings.HasPrefix(cfg.DBPath, "/tmp") {
		fmt.Fprintln(w, "WARNING: the SQLite database is under /tmp and will not survive a restart.")
	}
}

// mcpCommand runs the MCP server over stdio for local agents.
func mcpCommand(ctx context.Context, stdout, stderr io.Writer, args []string) error {
	cfg := config.Load()
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	environment := fs.String("environment", cfg.Environment, "gateway credential environment: sandbox or production")
	dbPath := fs.String("db", cfg.DBPath, "sqlite database path")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	env := strings.TrimSpace(*environment)
	if env == "" {
		env = string(domain.EnvironmentSandbox)
	}
	if err := validateEnvironment(env); err != nil {
		return err
	}
	store, err := sqlite.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	plane, err := buildRoutingPlane(cfg, store, domain.Environment(env))
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "rute-bayar MCP (stdio) environment=%s db=%s\n", env, *dbPath)
	return plane.mcp.ServeStdio(ctx, os.Stdin, stdout)
}
