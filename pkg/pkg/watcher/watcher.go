package watcher

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/894/node-cleanup-webhook/pkg/constants"
	"github.com/894/node-cleanup-webhook/pkg/plugins"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// Watcher watches for nodes being deleted and runs cleanup
type Watcher struct {
	client         kubernetes.Interface
	informer       cache.SharedIndexInformer
	workqueue      chan string
	pluginRegistry *plugins.Registry
	// Track nodes being processed to avoid duplicate work
	processing sync.Map
	// Context for background operations
	ctx context.Context
}

// New creates a new cleanup watcher
func New(ctx context.Context, client kubernetes.Interface, pluginRegistry *plugins.Registry) *Watcher {
	// Create informer factory
	factory := informers.NewSharedInformerFactory(client, constants.DefaultInformerResyncPeriod)
	nodeInformer := factory.Core().V1().Nodes().Informer()

	watcher := &Watcher{
		client:         client,
		informer:       nodeInformer,
		workqueue:      make(chan string, constants.DefaultWorkQueueSize),
		pluginRegistry: pluginRegistry,
		ctx:            ctx,
	}

	// Add event handlers
	nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node := obj.(*corev1.Node)
			klog.V(2).InfoS("Node added event", "node", node.Name)
			watcher.ensureFinalizer(node)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			node := newObj.(*corev1.Node)
			klog.V(3).InfoS("Node updated event", "node", node.Name, "isDeleting", node.DeletionTimestamp != nil)
			watcher.ensureFinalizer(node)
			watcher.enqueueIfDeleting(node)
		},
		DeleteFunc: func(obj interface{}) {
			// Node is already gone, just log
			if node, ok := obj.(*corev1.Node); ok {
				klog.InfoS("Node deleted from cache", "node", node.Name)
			}
		},
	})

	return watcher
}

// ensureFinalizer adds the finalizer to a node if it doesn't have it
func (w *Watcher) ensureFinalizer(node *corev1.Node) {
	// Skip if node is being deleted
	if node.DeletionTimestamp != nil {
		return
	}

	// Skip if finalizer already exists
	if containsFinalizer(node.Finalizers, constants.FinalizerName) {
		return
	}

	// Add finalizer in the background
	go func() {
		if err := w.addFinalizer(w.ctx, node); err != nil {
			klog.ErrorS(err, "Failed to add finalizer", "node", node.Name, "finalizer", constants.FinalizerName)
		} else {
			klog.InfoS("Finalizer added successfully", "node", node.Name, "finalizer", constants.FinalizerName)
		}
	}()
}

func (w *Watcher) enqueueIfDeleting(node *corev1.Node) {
	// Only process nodes that are being deleted
	if node.DeletionTimestamp == nil {
		return
	}

	// Only process if our finalizer is present
	if !containsFinalizer(node.Finalizers, constants.FinalizerName) {
		return
	}

	// Check if already being processed
	if _, loaded := w.processing.LoadOrStore(node.Name, true); loaded {
		klog.V(2).Infof("Node %s already being processed", node.Name)
		return
	}

	klog.InfoS("Node enqueued for cleanup", "node", node.Name, "deletionTimestamp", node.DeletionTimestamp.Time)
	w.workqueue <- node.Name
}

// Run starts the watcher
func (w *Watcher) Run() {
	klog.InfoS("Starting cleanup watcher", "finalizerName", constants.FinalizerName)

	// Start the informer
	go w.informer.Run(w.ctx.Done())

	// Wait for cache sync
	if !cache.WaitForCacheSync(w.ctx.Done(), w.informer.HasSynced) {
		klog.Fatal("Failed to sync informer cache")
	}
	klog.InfoS("Informer cache synced successfully")

	// Initialize finalizers on existing nodes
	if err := w.initializeExistingNodes(w.ctx); err != nil {
		klog.ErrorS(err, "Failed to initialize existing nodes")
	}

	// Process work queue
	for {
		select {
		case nodeName := <-w.workqueue:
			w.processNode(w.ctx, nodeName)
		case <-w.ctx.Done():
			klog.InfoS("Cleanup watcher stopping gracefully")
			return
		}
	}
}

func (w *Watcher) processNode(ctx context.Context, nodeName string) {
	defer w.processing.Delete(nodeName)

	klog.InfoS("Processing node cleanup", "node", nodeName)

	// Get current node state
	node, err := w.client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		klog.ErrorS(err, "Failed to get node", "node", nodeName)
		return
	}

	// Double-check it's still being deleted with our finalizer
	if node.DeletionTimestamp == nil || !containsFinalizer(node.Finalizers, constants.FinalizerName) {
		klog.V(2).InfoS("Node no longer needs cleanup", "node", nodeName,
			"isDeleting", node.DeletionTimestamp != nil,
			"hasFinalizer", containsFinalizer(node.Finalizers, constants.FinalizerName))
		return
	}

	// Check for skip annotation
	if node.Annotations[constants.SkipCleanupAnnotation] == "true" {
		klog.InfoS("Skip cleanup annotation detected - bypassing cleanup",
			"node", nodeName,
			"annotation", constants.SkipCleanupAnnotation)
		if err := w.removeFinalizer(ctx, node); err != nil {
			klog.ErrorS(err, "Failed to remove finalizer after skip", "node", nodeName)
		}
		return
	}

	// Run cleanup
	cleanupErr := w.runCleanup(ctx, node)
	if cleanupErr != nil {
		klog.ErrorS(cleanupErr, "Cleanup failed - will retry", "node", nodeName, "retryDelay", "10s")

		// Re-enqueue for retry after backoff (respects context cancellation)
		go func() {
			select {
			case <-time.After(constants.DefaultRetryDelay):
				w.processing.Delete(nodeName)
				// Re-fetch and re-enqueue
				if n, err := w.client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{}); err == nil {
					w.enqueueIfDeleting(n)
				}
			case <-ctx.Done():
				// Context cancelled, stop retry
				klog.V(2).InfoS("Retry cancelled due to context cancellation", "node", nodeName)
				w.processing.Delete(nodeName)
				return
			}
		}()
		return
	}

	// Cleanup succeeded - remove finalizer
	klog.InfoS("Cleanup completed successfully - removing finalizer", "node", nodeName)

	// Re-fetch node to get latest version
	node, err = w.client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		klog.ErrorS(err, "Failed to get node for finalizer removal", "node", nodeName)
		return
	}

	if err := w.removeFinalizer(ctx, node); err != nil {
		klog.ErrorS(err, "Failed to remove finalizer", "node", nodeName)
		return
	}

	klog.InfoS("Node cleanup completed successfully", "node", nodeName, "finalizer", "removed")
}

func (w *Watcher) runCleanup(ctx context.Context, node *corev1.Node) error {
	klog.InfoS("Running cleanup plugins", "node", node.Name)

	// Run all enabled plugins in order
	if err := w.pluginRegistry.RunAll(ctx, node); err != nil {
		klog.ErrorS(err, "Plugin execution failed", "node", node.Name)
		return fmt.Errorf("plugin execution failed: %w", err)
	}

	klog.InfoS("All cleanup plugins completed", "node", node.Name)
	return nil
}

func (w *Watcher) removeFinalizer(ctx context.Context, node *corev1.Node) error {
	// Build new finalizers list without our finalizer
	newFinalizers := []string{}
	for _, f := range node.Finalizers {
		if f != constants.FinalizerName {
			newFinalizers = append(newFinalizers, f)
		}
	}

	// Create patch
	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"finalizers": newFinalizers,
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("failed to marshal patch: %w", err)
	}

	_, err = w.client.CoreV1().Nodes().Patch(
		ctx,
		node.Name,
		types.MergePatchType,
		patchBytes,
		metav1.PatchOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to patch node: %w", err)
	}

	klog.InfoS("Finalizer removed successfully", "node", node.Name, "finalizer", constants.FinalizerName)
	return nil
}

// initializeExistingNodes adds finalizers to all existing nodes that don't have them
func (w *Watcher) initializeExistingNodes(ctx context.Context) error {
	klog.InfoS("Initializing finalizers on existing nodes", "finalizer", constants.FinalizerName)

	// List all nodes
	nodes, err := w.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	klog.InfoS("Found existing nodes", "count", len(nodes.Items))

	addedCount := 0
	skippedCount := 0

	for _, node := range nodes.Items {
		// Skip if node is already being deleted
		if node.DeletionTimestamp != nil {
			klog.V(2).InfoS("Skipping node - already being deleted", "node", node.Name)
			skippedCount++
			continue
		}

		// Check if finalizer already exists
		if containsFinalizer(node.Finalizers, constants.FinalizerName) {
			klog.V(2).InfoS("Skipping node - already has finalizer", "node", node.Name)
			skippedCount++
			continue
		}

		// Add finalizer
		if err := w.addFinalizer(ctx, &node); err != nil {
			klog.ErrorS(err, "Failed to add finalizer to existing node", "node", node.Name)
			continue
		}

		klog.InfoS("Finalizer added to existing node", "node", node.Name)
		addedCount++
	}

	klog.InfoS("Initialization complete", "finalizersAdded", addedCount, "nodesSkipped", skippedCount, "totalNodes", len(nodes.Items))
	return nil
}

// addFinalizer adds the cleanup finalizer to a node
func (w *Watcher) addFinalizer(ctx context.Context, node *corev1.Node) error {
	// Build new finalizers list with our finalizer
	newFinalizers := append([]string{}, node.Finalizers...)
	newFinalizers = append(newFinalizers, constants.FinalizerName)

	// Create patch
	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"finalizers": newFinalizers,
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("failed to marshal patch: %w", err)
	}

	_, err = w.client.CoreV1().Nodes().Patch(
		ctx,
		node.Name,
		types.MergePatchType,
		patchBytes,
		metav1.PatchOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to patch node: %w", err)
	}

	return nil
}

func containsFinalizer(finalizers []string, target string) bool {
	for _, f := range finalizers {
		if f == target {
			return true
		}
	}
	return false
}
