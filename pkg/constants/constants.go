package constants

import "time"

// Finalizer and annotation names
const (
	FinalizerName         = "infra.894.io/node-cleanup"
	SkipCleanupAnnotation = "infra.894.io/skip-cleanup"
)

// Timeouts and durations
const (
	// POC demonstration delay
	POCCleanupDelay = 15 * time.Second

	// Retry configuration
	DefaultRetryDelay     = 10 * time.Second
	MaxRetryAttempts      = 5
	ExponentialBackoffMax = 5 * time.Minute

	// Finalizer operations
	FinalizerOperationTimeout = 30 * time.Second

	// HTTP server timeouts
	DefaultHTTPReadTimeout  = 10 * time.Second
	DefaultHTTPWriteTimeout = 10 * time.Second
	DefaultShutdownTimeout  = 30 * time.Second

	// Informer and queue configuration
	DefaultInformerResyncPeriod = 30 * time.Second
	DefaultWorkQueueSize        = 100
	InformerCacheSyncTimeout    = 60 * time.Second
)

// Plugin names
const (
	LoggerPluginName   = "logger"
	PortworxPluginName = "portworx"
)

// Portworx labels
const (
	PortworxEnabledLabel         = "px/enabled"
	PortworxStatusLabel          = "px/status"
	PortworxEnabledValue         = "true"
	DefaultPortworxLabelSelector = "px/enabled=true"
)

// LVMO plugin
const (
	LVMOPluginName       = "lvmo"
	LVMODefaultNamespace = "openshift-storage"
	TopolvmCSIDriver     = "topolvm.io"
)

