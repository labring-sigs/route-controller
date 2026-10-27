// Package app wires the controller-runtime manager and production adapters.
package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	ciliumv2 "github.com/labring-sigs/route-controller/internal/apis/cilium/v2"
	"github.com/labring-sigs/route-controller/internal/config"
	"github.com/labring-sigs/route-controller/internal/controller"
	"github.com/labring-sigs/route-controller/internal/discovery"
	"github.com/labring-sigs/route-controller/internal/kubecache"
	"github.com/labring-sigs/route-controller/internal/planner"
	"github.com/labring-sigs/route-controller/internal/probe"
	"github.com/labring-sigs/route-controller/internal/route"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

const userAgent = "route-controller"

func Run(ctx context.Context, loaded config.Config) error {
	restConfig, err := clientcmd.BuildConfigFromFlags("", loaded.Kubernetes.Kubeconfig)
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}

	restConfig.UserAgent = userAgent
	restConfig.QPS = loaded.Kubernetes.QPS
	restConfig.Burst = loaded.Kubernetes.Burst

	apiScheme := runtime.NewScheme()
	if err := scheme.AddToScheme(apiScheme); err != nil {
		return fmt.Errorf("register Kubernetes API scheme: %w", err)
	}

	if err := ciliumv2.AddToScheme(apiScheme); err != nil {
		return fmt.Errorf("register Cilium API scheme: %w", err)
	}

	apiReader, err := client.New(restConfig, client.Options{Scheme: apiScheme})
	if err != nil {
		return fmt.Errorf("create discovery client: %w", err)
	}

	cfg, err := resolveConfig(ctx, apiReader, loaded)
	if err != nil {
		if !discovery.IsNoWorkers(err) {
			return fmt.Errorf("resolve route configuration: %w", err)
		}

		if err := awaitWorkers(ctx, apiReader, loaded); err != nil {
			return fmt.Errorf("await first worker: %w", err)
		}

		cfg, err = resolveConfig(ctx, apiReader, loaded)
		if err != nil {
			return fmt.Errorf("resolve route configuration: %w", err)
		}
	}

	log.FromContext(ctx).Info(
		"resolved route configuration",
		"interface",
		cfg.Routes.Interface,
		"podCIDR",
		cfg.Routes.PodCIDR,
		"serviceCIDR",
		cfg.ServiceCIDR,
		"routerCIDR",
		cfg.RouterCIDR,
	)

	backend, err := route.NewNetlinkBackend(
		cfg.Routes.Interface,
		cfg.Routes.Table,
		cfg.Routes.Protocol,
		cfg.PodCIDRs,
		cfg.ServiceCIDR,
	)
	if err != nil {
		return err
	}

	status := controller.NewStatusStore(cfg.Controller.DryRun)

	manager, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme: apiScheme,
		Cache:  kubecache.Options(),
		Metrics: metricsserver.Options{
			BindAddress: cfg.Observability.MetricsBindAddress,
			ExtraHandlers: map[string]http.Handler{
				"/status": status,
			},
		},
		HealthProbeBindAddress: cfg.Observability.HealthProbeBindAddress,
		LeaderElection:         false,
	})
	if err != nil {
		return fmt.Errorf("create controller manager: %w", err)
	}

	reconciler := controller.NewReconciler(
		manager.GetClient(),
		planner.New(cfg.PodCIDRs, cfg.ServiceCIDR, cfg.RouterCIDR),
		backend,
		probe.NewHTTP(cfg.Probe.Timeout, cfg.Probe.Port),
		planner.NewProbeTracker(cfg.Probe.FailureThreshold, cfg.Probe.SuccessThreshold),
		status,
		controller.ReconcilerConfig{
			ReconcilePeriod: cfg.Controller.ReconcilePeriod,
			DryRun:          cfg.Controller.DryRun,
		},
	)
	if err := reconciler.SetupWithManager(manager); err != nil {
		return fmt.Errorf("configure route controller: %w", err)
	}

	if err := manager.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return fmt.Errorf("configure liveness check: %w", err)
	}

	if err := manager.AddReadyzCheck("routes", status.Ready); err != nil {
		return fmt.Errorf("configure readiness check: %w", err)
	}

	if err := manager.Start(ctx); err != nil {
		return fmt.Errorf("run controller manager: %w", err)
	}

	return nil
}

func resolveConfig(
	ctx context.Context,
	apiReader client.Client,
	loaded config.Config,
) (config.Validated, error) {
	return discovery.NewResolver(apiReader, discovery.NewSystemNetwork()).Resolve(ctx, loaded)
}

// awaitWorkers blocks until the first non-control-plane Node registers while
// serving green health endpoints, so orchestrators that gate joins on this
// controller's readiness are not deadlocked by an empty cluster. Configuration
// errors keep failing fast: this path is only entered when discovery failed
// solely because no worker exists yet.
func awaitWorkers(ctx context.Context, apiReader client.Client, loaded config.Config) error {
	logger := log.FromContext(ctx)

	server := startPrelude(ctx, loaded.Observability.HealthProbeBindAddress)
	defer func() {
		_ = server.Shutdown(context.Background())
	}()

	logger.Info("no worker Nodes registered; serving readiness until the first worker joins")

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			has, err := discovery.HasWorkerNodes(ctx, apiReader)
			if err != nil {
				logger.Error(err, "check for worker Nodes")

				continue
			}

			if has {
				logger.Info("first worker Node registered; completing route discovery")

				return nil
			}
		}
	}
}

// startPrelude serves /healthz, /readyz and a pending /status on the health
// probe address. It returns a no-op server when the address is empty.
func startPrelude(ctx context.Context, address string) *http.Server {
	server := &http.Server{
		Handler:           preludeHandler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	if address == "" {
		return server
	}

	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", address)
	if err != nil {
		log.FromContext(ctx).Error(err, "serve readiness prelude", "address", address)

		return server
	}

	go func() {
		serveErr := server.Serve(listener)
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.FromContext(ctx).Error(serveErr, "readiness prelude exited")
		}
	}()

	return server
}

func preludeHandler() http.Handler {
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	}
	mux.HandleFunc("/healthz", ok)
	mux.HandleFunc("/readyz", ok)
	mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"ready":true,"pending":"awaiting first worker Node"}`)
	})

	return mux
}
