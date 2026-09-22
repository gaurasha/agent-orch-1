package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gaurasha/agent-orch/backend/internal/api"
	"github.com/gaurasha/agent-orch/backend/internal/creds"
	"github.com/gaurasha/agent-orch/backend/internal/fairness"
	"github.com/gaurasha/agent-orch/backend/internal/fakegithub"
	"github.com/gaurasha/agent-orch/backend/internal/gateway"
	"github.com/gaurasha/agent-orch/backend/internal/llm"
	"github.com/gaurasha/agent-orch/backend/internal/obs"
	"github.com/gaurasha/agent-orch/backend/internal/runtime"
	"github.com/gaurasha/agent-orch/backend/internal/sandbox"
	"github.com/gaurasha/agent-orch/backend/internal/store"
	"github.com/gaurasha/agent-orch/backend/internal/tools"
	"github.com/gaurasha/agent-orch/backend/internal/types"
)

// Options are the knobs every service shares. Defaults are chosen so that
// `agentorch serve` with no flags works on a laptop.
type Options struct {
	Role         string
	DSN          string
	Addr         string
	GatewayAddr  string
	GatewayURL   string
	GitHubAPI    string
	WorkspaceDir string
	SandboxDir   string
	SandboxDrv   string
	SandboxImage string
	RuntimeClass string
	Workers      int
	ProviderTPM  int64
	LogLevel     string
	Secret       string
	ModelLatency time.Duration
	FailEvery    int
	Seed         bool
	UIDir        string
}

func bindFlags(fs *flag.FlagSet) *Options {
	o := &Options{}
	fs.StringVar(&o.Role, "role", env("AGENTORCH_ROLE", "all"),
		"which service to run: all|controlplane|worker|gateway")
	fs.StringVar(&o.DSN, "dsn", env("AGENTORCH_DSN", ""),
		"Postgres DSN. Empty uses the in-memory store (no durability across restarts)")
	fs.StringVar(&o.Addr, "addr", env("AGENTORCH_ADDR", ":8080"), "control plane listen address")
	fs.StringVar(&o.GatewayAddr, "gateway-addr", env("AGENTORCH_GATEWAY_ADDR", ":8081"), "tool gateway listen address")
	fs.StringVar(&o.GatewayURL, "gateway-url", env("AGENTORCH_GATEWAY_URL", ""),
		"tool gateway URL for workers. Empty means call the gateway in-process")
	fs.StringVar(&o.GitHubAPI, "github-api", env("AGENTORCH_GITHUB_API", ""),
		"GitHub API base. Empty starts the built-in fake API")
	fs.StringVar(&o.WorkspaceDir, "workspace-dir", env("AGENTORCH_WORKSPACE_DIR", "/var/lib/agentorch/workspaces"), "run workspace root")
	fs.StringVar(&o.SandboxDir, "sandbox-dir", env("AGENTORCH_SANDBOX_DIR", "/var/lib/agentorch/sandboxes"), "sandbox scratch root")
	fs.StringVar(&o.SandboxDrv, "sandbox-driver", env("AGENTORCH_SANDBOX_DRIVER", "namespace"), "namespace|docker|kubernetes")
	fs.StringVar(&o.SandboxImage, "sandbox-image", env("AGENTORCH_SANDBOX_IMAGE", "agentorch/sandbox:dev"), "sandbox image (docker/kubernetes drivers)")
	fs.StringVar(&o.RuntimeClass, "runtime-class", env("AGENTORCH_RUNTIME_CLASS", ""), "RuntimeClass for sandbox pods, e.g. gvisor")
	fs.IntVar(&o.Workers, "workers", envInt("AGENTORCH_WORKERS", 4), "agent worker goroutines")
	fs.Int64Var(&o.ProviderTPM, "provider-tpm", int64(envInt("AGENTORCH_PROVIDER_TPM", 400000)), "total LLM tokens per minute across all tenants")
	fs.StringVar(&o.LogLevel, "log-level", env("AGENTORCH_LOG_LEVEL", "info"), "debug|info|warn|error")
	fs.StringVar(&o.Secret, "secret", env("AGENTORCH_SECRET", ""), "HMAC secret for run tokens. Generated if empty (single-process only)")
	fs.DurationVar(&o.ModelLatency, "model-latency", 300*time.Millisecond, "simulated LLM latency")
	fs.IntVar(&o.FailEvery, "model-fail-every", 0, "make the fake model fail every Nth call, to exercise backoff")
	fs.BoolVar(&o.Seed, "seed", true, "seed demo tenants and agents on startup")
	fs.StringVar(&o.UIDir, "ui-dir", env("AGENTORCH_UI_DIR", ""), "directory of built UI assets to serve at /")
	return o
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

// Platform holds the wired-up components.
type Platform struct {
	Opts     *Options
	Log      *obs.Logger
	Metrics  *obs.Metrics
	Store    store.Store
	Registry *tools.Registry
	Broker   *creds.DerivedBroker
	Limiter  *fairness.Limiter
	Model    *llm.Gateway
	Fake     *llm.FakeProvider
	Gateway  *gateway.Gateway
	API      *api.Server
	GitHub   *fakegithub.Server
	Workspace *tools.WorkspaceManager
	Sandbox  sandbox.Driver
	Secret   []byte
	closers  []func()
}

// Build assembles the platform. Every dependency is explicit and injected,
// which is what lets the tests construct the same object graph with a fake
// clock, an in-memory store or a stub provider.
func Build(ctx context.Context, o *Options) (*Platform, error) {
	log := obs.NewLogger(o.Role, o.LogLevel)
	metrics := obs.NewMetrics()

	var st store.Store
	if o.DSN == "" {
		log.Warn("no --dsn given: using the in-memory store. " +
			"Runs will NOT survive a restart, which defeats the durability guarantee. " +
			"Use Postgres for anything you intend to believe.")
		st = store.NewMem()
	} else {
		pg, err := store.OpenPostgres(ctx, o.DSN, 25)
		if err != nil {
			return nil, fmt.Errorf("open store: %w", err)
		}
		st = pg
	}

	secret := []byte(o.Secret)
	if len(secret) == 0 {
		// Fine for a single process; catastrophic across replicas, because each
		// would mint tokens the others reject. Say so loudly.
		secret = []byte(randomSecret())
		if o.Role != "all" {
			log.Error("AGENTORCH_SECRET is not set. Each replica will generate a different " +
				"signing key and reject the others' run tokens. Set it explicitly.")
		}
	}

	githubAPI := o.GitHubAPI
	var ghServer *fakegithub.Server
	broker := creds.NewDerivedBroker()

	drv, err := sandbox.New(o.SandboxDrv, sandbox.Config{
		StateDir: o.SandboxDir, Image: o.SandboxImage,
		RuntimeClass: o.RuntimeClass, Namespace: "agentorch-sandboxes",
	})
	if err != nil {
		return nil, fmt.Errorf("sandbox driver %q: %w", o.SandboxDrv, err)
	}
	log.Info("sandbox driver ready", "driver", drv.Name(),
		"runtime_class", orNone(o.RuntimeClass),
		"blocked_syscalls", len(sandbox.DeniedSyscallNames()))

	ws, err := tools.NewWorkspaceManager(o.WorkspaceDir)
	if err != nil {
		return nil, fmt.Errorf("workspace manager: %w", err)
	}

	if githubAPI == "" {
		ghServer = fakegithub.New(broker, log.With("component", "fake-github"))
	}
	registry := tools.DefaultRegistry(githubAPI)
	invoker := tools.NewInvoker(drv, broker, ws, githubAPI)

	limiter := fairness.New(fairness.Config{
		ProviderTokensPerMinute: o.ProviderTPM,
		BurstSeconds:            10,
		InteractiveReservePct:   20,
	})
	fake := llm.NewFakeProvider(o.ModelLatency, o.ModelLatency/2)
	if o.FailEvery > 0 {
		fake.FailEvery(o.FailEvery)
	}
	modelGW := llm.NewGateway(fake, limiter, metrics, log.With("component", "model-gateway"))

	gw := gateway.New(st, registry, invoker, ws, secret, metrics, log.With("component", "tool-gateway"))
	apiSrv := api.New(st, registry, limiter, metrics, log.With("component", "control-plane"))

	return &Platform{
		Opts: o, Log: log, Metrics: metrics, Store: st, Registry: registry,
		Broker: broker, Limiter: limiter, Model: modelGW, Fake: fake,
		Gateway: gw, API: apiSrv, GitHub: ghServer, Workspace: ws,
		Sandbox: drv, Secret: secret,
	}, nil
}

func (p *Platform) Close() {
	for i := len(p.closers) - 1; i >= 0; i-- {
		p.closers[i]()
	}
	_ = p.Sandbox.Close()
	_ = p.Store.Close()
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	o := bindFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	p, err := Build(ctx, o)
	if err != nil {
		fmt.Fprintln(os.Stderr, "startup failed:", err)
		return 1
	}
	defer p.Close()

	if o.Seed {
		if err := Seed(ctx, p); err != nil {
			p.Log.Error("seeding failed", "err", err)
			return 1
		}
	}

	var servers []*http.Server

	// The tool gateway. In Kubernetes this is its own Deployment with its own
	// ServiceAccount and the only egress path to tenant credentials.
	if o.Role == "all" || o.Role == "gateway" {
		mux := http.NewServeMux()
		p.Gateway.Routes(mux)
		if p.GitHub != nil {
			p.GitHub.Routes(mux)
		}
		mux.Handle("GET /metrics", p.Metrics.Handler())
		srv := &http.Server{Addr: o.GatewayAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, srv)
		go func() {
			p.Log.Info("tool gateway listening", "addr", o.GatewayAddr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				p.Log.Error("tool gateway failed", "err", err)
			}
		}()
	}

	// The control plane and the UI.
	if o.Role == "all" || o.Role == "controlplane" {
		mux := http.NewServeMux()
		p.API.Routes(mux)
		p.API.AddAPIKey("demo-operator-key", api.Principal{
			TenantID: "acme", User: "operator@agentorch.dev", Operator: true})
		p.API.AddAPIKey("acme-key", api.Principal{TenantID: "acme", User: "alice@acme.example"})
		p.API.AddAPIKey("globex-key", api.Principal{TenantID: "globex", User: "bob@globex.example"})
		if o.UIDir != "" {
			mux.Handle("/", uiHandler(o.UIDir))
		}
		srv := &http.Server{Addr: o.Addr, Handler: withCORS(mux), ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, srv)
		go func() {
			p.Log.Info("control plane listening", "addr", o.Addr, "ui", orNone(o.UIDir))
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				p.Log.Error("control plane failed", "err", err)
			}
		}()
	}

	// Agent workers and the reaper.
	if o.Role == "all" || o.Role == "worker" {
		StartWorkers(ctx, p)
	}

	<-ctx.Done()
	p.Log.Info("shutdown signal received; draining")
	shutCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(shutCtx)
	}
	// Workers drain by returning from their loop; anything they had leased
	// simply expires and is picked up elsewhere. That is the whole deploy story.
	time.Sleep(300 * time.Millisecond)
	return 0
}

// StartWorkers launches the worker pool and the reaper.
func StartWorkers(ctx context.Context, p *Platform) {
	var caller runtime.ToolCaller
	if p.Opts.GatewayURL != "" {
		caller = runtime.NewHTTPToolCaller(p.Opts.GatewayURL, p.Secret, p.Store)
	} else {
		caller = runtime.NewLocalToolCaller(p.Gateway, p.Secret, p.Store)
	}
	for i := 0; i < p.Opts.Workers; i++ {
		w := runtime.NewWorker(runtime.Config{
			WorkerID: fmt.Sprintf("worker-%d", i),
		}, p.Store, p.Model, caller, p.Registry, p.Limiter.Eligible, p.Metrics,
			p.Log.With("component", "agentd"))
		go w.Run(ctx)
	}
	go runtime.NewReaper(p.Store, 3*time.Second, 2*time.Minute, p.Metrics,
		p.Log.With("component", "reaper")).Run(ctx)
	// Keep the limiter's tenant set fresh so a newly added tenant starts
	// getting its guaranteed share without a restart.
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			if ts, err := p.Store.ListTenants(ctx); err == nil {
				p.Limiter.SetTenants(ts)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	p.Log.Info("agent workers started", "count", p.Opts.Workers)
}

// uiHandler serves the built single-page app, falling back to index.html so
// client-side routes survive a refresh.
func uiHandler(dir string) http.Handler {
	fileServer := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if _, err := os.Stat(dir + "/" + path); err != nil {
			http.ServeFile(w, r, dir+"/index.html")
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

// withCORS lets the Vite dev server talk to the API during development.
func withCORS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func randomSecret() string {
	return fmt.Sprintf("dev-%d-%s", time.Now().UnixNano(), types.StateQueued)
}

// RebindGitHub points the tool registry and invoker at a GitHub API base that
// was only known after a listener was bound (the demo picks a free port).
func (p *Platform) RebindGitHub(base string) {
	p.Registry = tools.DefaultRegistry(base)
	p.Workspace, _ = tools.NewWorkspaceManager(p.Opts.WorkspaceDir)
	invoker := tools.NewInvoker(p.Sandbox, p.Broker, p.Workspace, base)
	p.Gateway = gateway.New(p.Store, p.Registry, invoker, p.Workspace, p.Secret,
		p.Metrics, p.Log.With("component", "tool-gateway"))
}

// sandboxDenied exposes the seccomp denylist for the demo banner.
func sandboxDenied() []string { return sandbox.DeniedSyscallNames() }
