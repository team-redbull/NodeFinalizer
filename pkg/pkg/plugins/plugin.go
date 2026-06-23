package plugins

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// Plugin defines the interface for cleanup plugins
type Plugin interface {
	// Name returns the plugin name
	Name() string

	// ShouldRun determines if this plugin should run for the given node
	ShouldRun(node *corev1.Node) bool

	// Cleanup performs the cleanup operation
	Cleanup(ctx context.Context, node *corev1.Node) error
}

// Registry manages all available cleanup plugins
type Registry struct {
	plugins      map[string]Plugin
	enabled      map[string]bool
	pluginOrder  []string // Execution order from ENABLED_PLUGINS env var
}

// NewRegistry creates a new plugin registry
func NewRegistry() *Registry {
	return &Registry{
		plugins:     make(map[string]Plugin),
		enabled:     make(map[string]bool),
		pluginOrder: []string{},
	}
}

// Register registers a new plugin
func (r *Registry) Register(plugin Plugin) {
	name := plugin.Name()
	r.plugins[name] = plugin
	klog.V(2).Infof("Registered plugin: %s", name)
}

// Enable enables a plugin by name and records the execution order
func (r *Registry) Enable(name string) error {
	if _, exists := r.plugins[name]; !exists {
		return fmt.Errorf("plugin %s not found", name)
	}
	r.enabled[name] = true
	r.pluginOrder = append(r.pluginOrder, name)
	klog.Infof("✅ Enabled cleanup plugin: %s (position %d)", name, len(r.pluginOrder))
	return nil
}

// Disable disables a plugin by name
func (r *Registry) Disable(name string) {
	r.enabled[name] = false
	klog.Infof("Disabled cleanup plugin: %s", name)
}

// RunAll runs all enabled plugins in the order they were enabled (from ENABLED_PLUGINS env var)
func (r *Registry) RunAll(ctx context.Context, node *corev1.Node) error {
	klog.InfoS("Starting cleanup plugins", "node", node.Name, "pluginOrder", r.pluginOrder)

	ranCount := 0

	// Execute plugins in the order they were enabled
	for i, name := range r.pluginOrder {
		plugin, exists := r.plugins[name]
		if !exists {
			klog.ErrorS(nil, "Plugin not found in registry", "plugin", name)
			continue
		}

		// Skip if plugin should not run for this node
		if !plugin.ShouldRun(node) {
			klog.V(2).InfoS("Plugin skipped - conditions not met", "plugin", name, "node", node.Name)
			continue
		}

		klog.InfoS("Running plugin", "plugin", name, "position", i+1, "total", len(r.pluginOrder), "node", node.Name)

		if err := plugin.Cleanup(ctx, node); err != nil {
			klog.ErrorS(err, "Plugin execution failed", "plugin", name, "node", node.Name)
			return fmt.Errorf("plugin %s failed: %w", name, err)
		}

		klog.InfoS("Plugin completed successfully", "plugin", name, "node", node.Name)
		ranCount++
	}

	klog.InfoS("Cleanup completed", "node", node.Name, "executedPlugins", ranCount, "totalPlugins", len(r.pluginOrder))
	return nil
}

// GetEnabledPlugins returns a list of enabled plugin names
func (r *Registry) GetEnabledPlugins() []string {
	var enabled []string
	for name, isEnabled := range r.enabled {
		if isEnabled {
			enabled = append(enabled, name)
		}
	}
	return enabled
}

// BasePlugin provides common functionality for plugins
type BasePlugin struct {
	name   string
	client kubernetes.Interface
}

// Name returns the plugin name
func (b *BasePlugin) Name() string {
	return b.name
}
