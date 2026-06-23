package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/894/node-cleanup-webhook/pkg/config"
	"github.com/894/node-cleanup-webhook/pkg/constants"
	"github.com/894/node-cleanup-webhook/pkg/plugins"
	"github.com/894/node-cleanup-webhook/pkg/watcher"
	"github.com/894/node-cleanup-webhook/pkg/webhook"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

func main() {
	klog.InitFlags(nil)

	// Parse command-line flags
	var kubeconfig string
	var tlsCert string
	var tlsKey string
	var port int

	flag.StringVar(&tlsCert, "tls-cert", "", "TLS certificate file (overrides env)")
	flag.StringVar(&tlsKey, "tls-key", "", "TLS key file (overrides env)")
	flag.IntVar(&port, "port", 0, "Webhook server port (overrides env)")
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig (uses in-cluster config if empty)")
	flag.Parse()

	// Load configuration from environment
	cfg := config.LoadFromEnv()

	// Override with command-line flags if provided
	if tlsCert != "" {
		cfg.TLSCertFile = tlsCert
	}
	if tlsKey != "" {
		cfg.TLSKeyFile = tlsKey
	}
	if port != 0 {
		cfg.Port = port
	}
	if kubeconfig != "" {
		cfg.Kubeconfig = kubeconfig
	}

	// Print configuration
	klog.Info("===========================================")
	klog.Info("Node Cleanup Webhook Starting")
	klog.Info("===========================================")
	cfg.Print()
	klog.Info("===========================================")

	// Create Kubernetes clients
	restConfig, err := buildRestConfig(cfg.Kubeconfig, cfg.InsecureSkipTLSVerify)
	if err != nil {
		klog.Fatalf("Failed to build REST config: %v", err)
	}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		klog.Fatalf("Failed to create Kubernetes client: %v", err)
	}
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		klog.Fatalf("Failed to create dynamic client: %v", err)
	}

	// Initialize plugin registry
	pluginRegistry := plugins.NewRegistry()

	// Register available plugins
	klog.Info("Registering cleanup plugins...")
	pluginRegistry.Register(plugins.NewLoggerPlugin(client))
	pluginRegistry.Register(plugins.NewPortworxPlugin(client, cfg.GetPluginOption("portworx", "labelSelector", constants.DefaultPortworxLabelSelector)))
	pluginRegistry.Register(plugins.NewLVMOPlugin(
		client,
		dynamicClient,
		cfg.GetPluginOption("lvmo", "namespace", constants.LVMODefaultNamespace),
		cfg.GetPluginOption("lvmo", "cleanupImage", constants.LVMODefaultCleanupImage),
		cfg.GetPluginOptionDuration("lvmo", "cleanupTimeout", constants.LVMODefaultCleanupTimeout),
	))

	// Enable configured plugins
	klog.Info("Enabling plugins based on configuration...")
	for _, pluginName := range cfg.EnabledPlugins {
		if err := pluginRegistry.Enable(pluginName); err != nil {
			klog.Warningf("Failed to enable plugin %s: %v", pluginName, err)
		}
	}

	// Show enabled plugins
	enabledPlugins := pluginRegistry.GetEnabledPlugins()
	if len(enabledPlugins) == 0 {
		klog.Warning("⚠️  No plugins enabled! Node cleanup will do nothing.")
	} else {
		klog.Infof("📦 Enabled plugins: %v", enabledPlugins)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown gracefully
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Start cleanup watcher with plugin registry
	nodeWatcher := watcher.New(ctx, client, pluginRegistry)
	go nodeWatcher.Run()

	// Start webhook server
	webhookServer := webhook.NewServer()
	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		ReadTimeout:  constants.DefaultHTTPReadTimeout,
		WriteTimeout: constants.DefaultHTTPWriteTimeout,
	}

	http.HandleFunc("/mutate-node", webhookServer.HandleMutateNode)
	http.HandleFunc("/healthz", handleHealthz)
	http.HandleFunc("/readyz", handleReadyz)

	go func() {
		klog.Infof("🚀 Starting webhook server on port %d", cfg.Port)
		if err := server.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile); err != nil && err != http.ErrServerClosed {
			klog.Fatalf("Webhook server failed: %v", err)
		}
	}()

	// Wait for shutdown signal
	<-sigCh
	klog.Info("⏹️  Shutting down...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), constants.DefaultShutdownTimeout)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		klog.Errorf("Webhook server shutdown error: %v", err)
	}

	cancel() // Stop the watcher
	klog.Info("✅ Shutdown complete")
}

func buildRestConfig(kubeconfig string, insecureSkipTLSVerify bool) (*rest.Config, error) {
	var restConfig *rest.Config
	var err error

	if kubeconfig != "" {
		restConfig, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		restConfig, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create config: %w", err)
	}

	if insecureSkipTLSVerify {
		klog.Warning("⚠️  TLS verification disabled for kube-apiserver - NOT RECOMMENDED for production!")
		restConfig.TLSClientConfig.Insecure = true
		restConfig.TLSClientConfig.CAData = nil
		restConfig.TLSClientConfig.CAFile = ""
	}

	return restConfig, nil
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func handleReadyz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}
