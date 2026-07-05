package config

import (
	"os"
	"strconv"
	"strings"
	"time"

	"k8s.io/klog/v2"
)

// Config holds the application configuration
type Config struct {
	// Webhook configuration
	TLSCertFile string
	TLSKeyFile  string
	Port        int
	Kubeconfig  string

	// Kubernetes client configuration
	InsecureSkipTLSVerify bool // Skip TLS verification for kube-apiserver (insecure environments)

	// Plugin configuration
	EnabledPlugins []string
	PluginConfigs  map[string]PluginConfig
}

// PluginConfig holds configuration for a specific plugin
type PluginConfig struct {
	Enabled bool
	Options map[string]string
}

// LoadFromEnv loads configuration from environment variables
func LoadFromEnv() *Config {
	cfg := &Config{
		TLSCertFile:           getEnv("TLS_CERT_FILE", "/etc/webhook/certs/tls.crt"),
		TLSKeyFile:            getEnv("TLS_KEY_FILE", "/etc/webhook/certs/tls.key"),
		Port:                  getEnvInt("PORT", 8443),
		Kubeconfig:            getEnv("KUBECONFIG", ""),
		InsecureSkipTLSVerify: getEnvBool("INSECURE_SKIP_TLS_VERIFY", false),
		PluginConfigs:         make(map[string]PluginConfig),
		EnabledPlugins:        []string{},
	}

	// Load enabled plugins from ENABLED_PLUGINS env var
	// Format: "portworx,drain,logger,slack"
	if plugins := getEnv("ENABLED_PLUGINS", "logger"); plugins != "" {
		cfg.EnabledPlugins = strings.Split(plugins, ",")
		for i := range cfg.EnabledPlugins {
			cfg.EnabledPlugins[i] = strings.TrimSpace(cfg.EnabledPlugins[i])
		}
	}

	// Load plugin-specific configurations
	cfg.loadPluginConfigs()

	return cfg
}

// loadPluginConfigs loads configuration for each plugin
func (c *Config) loadPluginConfigs() {
	// Logger plugin configuration
	c.PluginConfigs["logger"] = PluginConfig{
		Enabled: c.isPluginEnabled("logger"),
		Options: map[string]string{
			"format":    getEnv("LOGGER_FORMAT", "pretty"),
			"verbosity": getEnv("LOGGER_VERBOSITY", "info"),
		},
	}

	// Portworx plugin configuration
	c.PluginConfigs["portworx"] = PluginConfig{
		Enabled: c.isPluginEnabled("portworx"),
		Options: map[string]string{
			"labelSelector": getEnv("PORTWORX_LABEL_SELECTOR", "px/enabled=true"),
			"apiEndpoint":   getEnv("PORTWORX_API_ENDPOINT", "http://portworx-api:9001"),
			"timeout":       getEnv("PORTWORX_TIMEOUT", "300s"),
		},
	}

	// LVMO plugin configuration
	c.PluginConfigs["lvmo"] = PluginConfig{
		Enabled: c.isPluginEnabled("lvmo"),
		Options: map[string]string{
			"namespace": getEnv("LVMO_NAMESPACE", "openshift-storage"),
		},
	}
}

// isPluginEnabled checks if a plugin is in the enabled list
func (c *Config) isPluginEnabled(pluginName string) bool {
	for _, name := range c.EnabledPlugins {
		if name == pluginName {
			return true
		}
	}
	return false
}

// GetPluginOption gets a configuration option for a plugin
func (c *Config) GetPluginOption(pluginName, optionName, defaultValue string) string {
	if cfg, ok := c.PluginConfigs[pluginName]; ok {
		if val, ok := cfg.Options[optionName]; ok && val != "" {
			return val
		}
	}
	return defaultValue
}

// GetPluginOptionDuration gets a duration configuration option for a plugin
func (c *Config) GetPluginOptionDuration(pluginName, optionName string, defaultValue time.Duration) time.Duration {
	val := c.GetPluginOption(pluginName, optionName, "")
	if val == "" {
		return defaultValue
	}

	duration, err := time.ParseDuration(val)
	if err != nil {
		klog.Warningf("Invalid duration for %s.%s: %s, using default %v", pluginName, optionName, val, defaultValue)
		return defaultValue
	}
	return duration
}

// Print prints the configuration
func (c *Config) Print() {
	klog.Info("Configuration:")
	klog.Infof("  TLS Cert: %s", c.TLSCertFile)
	klog.Infof("  TLS Key: %s", c.TLSKeyFile)
	klog.Infof("  Port: %d", c.Port)
	klog.Infof("  Insecure Skip TLS Verify: %t", c.InsecureSkipTLSVerify)
	klog.Infof("  Enabled Plugins: %v", c.EnabledPlugins)

	for _, pluginName := range c.EnabledPlugins {
		if cfg, ok := c.PluginConfigs[pluginName]; ok {
			klog.Infof("  Plugin [%s]:", pluginName)
			for key, val := range cfg.Options {
				// Hide sensitive values
				if strings.Contains(strings.ToLower(key), "webhook") || strings.Contains(strings.ToLower(key), "token") {
					klog.Infof("    %s: ***REDACTED***", key)
				} else {
					klog.Infof("    %s: %s", key, val)
				}
			}
		}
	}
}

// Helper functions

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intVal, err := strconv.Atoi(value); err == nil {
			return intVal
		}
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		if boolVal, err := strconv.ParseBool(value); err == nil {
			return boolVal
		}
	}
	return defaultValue
}

// Example .env file format:
//
// # Webhook configuration
// PORT=8443
// TLS_CERT_FILE=/etc/webhook/certs/tls.crt
// TLS_KEY_FILE=/etc/webhook/certs/tls.key
//
// # Kubernetes client configuration
// INSECURE_SKIP_TLS_VERIFY=false  # Set to true for insecure kube-apiserver (not recommended for production)
//
// # Plugin configuration
// ENABLED_PLUGINS=logger,drain,portworx,slack
//
// # Portworx plugin
// PORTWORX_LABEL_SELECTOR=px/enabled=true
// PORTWORX_API_ENDPOINT=http://portworx-api:9001
// PORTWORX_TIMEOUT=300s
//
// # Drain plugin
// DRAIN_TIMEOUT=300s
// DRAIN_GRACE_PERIOD=30s
//
// # Slack plugin
// SLACK_WEBHOOK_URL=https://hooks.slack.com/services/YOUR/WEBHOOK/URL
// SLACK_CHANNEL=#infrastructure
