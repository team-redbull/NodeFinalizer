package plugins

import (
	"context"
	"fmt"
	"time"

	"github.com/894/node-cleanup-webhook/pkg/constants"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// LoggerPlugin logs node deletion information
type LoggerPlugin struct {
	BasePlugin
}

// NewLoggerPlugin creates a new logger plugin
func NewLoggerPlugin(client kubernetes.Interface) *LoggerPlugin {
	return &LoggerPlugin{
		BasePlugin: BasePlugin{
			name:   constants.LoggerPluginName,
			client: client,
		},
	}
}

// ShouldRun always returns true - log all node deletions
func (p *LoggerPlugin) ShouldRun(node *corev1.Node) bool {
	return true
}

// Cleanup logs node information using structured logging
func (p *LoggerPlugin) Cleanup(ctx context.Context, node *corev1.Node) error {
	// Print banner showing cleanup started
	fmt.Printf("\n╔═══════════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║  🔄 CLEANUP STARTED FOR NODE: %-30s ║\n", node.Name)
	fmt.Printf("╚═══════════════════════════════════════════════════════════════╝\n")

	// Structured log with all node metadata
	klog.InfoS("Node deletion initiated - starting cleanup delay",
		"node", node.Name,
		"createdAt", node.CreationTimestamp.Time,
		"deletionTimestamp", node.DeletionTimestamp.Time,
		"uid", node.UID,
		"labelCount", len(node.Labels),
		"conditionCount", len(node.Status.Conditions),
		"cleanupDelay", constants.POCCleanupDelay.String(),
	)

	// POC: Sleep for 15 seconds to demonstrate that deletion waits for cleanup
	fmt.Printf("⏳ Waiting 15 seconds to demonstrate finalizer blocking deletion...\n")
	klog.InfoS("Simulating cleanup work - sleeping to demonstrate finalizer", "node", node.Name, "duration", "15s")

	// Use select with context to respect cancellation
	select {
	case <-time.After(constants.POCCleanupDelay):
		// Continue with cleanup
		klog.InfoS("Cleanup delay completed", "node", node.Name)
		fmt.Printf("✅ 15 second delay completed!\n\n")
	case <-ctx.Done():
		// Context cancelled during sleep
		klog.InfoS("Cleanup cancelled during delay", "node", node.Name)
		return ctx.Err()
	}

	// Print summary after delay
	fmt.Printf("╔═══════════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║  🗑️  FINALIZING DELETION FOR NODE: %-26s ║\n", node.Name)
	fmt.Printf("╠═══════════════════════════════════════════════════════════════╣\n")
	fmt.Printf("║  Created:     %-47s ║\n", node.CreationTimestamp.Time.Format(time.RFC3339))
	fmt.Printf("║  Deleting At: %-47s ║\n", node.DeletionTimestamp.Time.Format(time.RFC3339))
	fmt.Printf("║  UID:         %-47s ║\n", node.UID)
	fmt.Printf("╚═══════════════════════════════════════════════════════════════╝\n")

	// Log labels as structured data
	if len(node.Labels) > 0 {
		klog.V(1).InfoS("Node labels", "node", node.Name, "labels", node.Labels)
		fmt.Printf("\n📋 Labels (%d):\n", len(node.Labels))
		for k, v := range node.Labels {
			fmt.Printf("   • %s: %s\n", k, v)
		}
	}

	// Log conditions as structured data
	if len(node.Status.Conditions) > 0 {
		conditions := make(map[string]string)
		for _, cond := range node.Status.Conditions {
			conditions[string(cond.Type)] = string(cond.Status)
		}
		klog.V(1).InfoS("Node conditions", "node", node.Name, "conditions", conditions)

		fmt.Printf("\n🏥 Conditions (%d):\n", len(node.Status.Conditions))
		for _, cond := range node.Status.Conditions {
			fmt.Printf("   • %s: %s (reason: %s)\n", cond.Type, cond.Status, cond.Reason)
		}
	}

	fmt.Printf("\n╔═══════════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║  ✅ CLEANUP COMPLETED - Node can now be deleted              ║\n")
	fmt.Printf("╚═══════════════════════════════════════════════════════════════╝\n\n")

	return nil
}
