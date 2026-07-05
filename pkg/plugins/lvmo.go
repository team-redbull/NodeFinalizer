package plugins

import (
	"context"
	"fmt"

	"github.com/894/node-cleanup-webhook/pkg/constants"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

var lvmVGNodeStatusGVR = schema.GroupVersionResource{
	Group:    "lvm.topolvm.io",
	Version:  "v1alpha1",
	Resource: "lvmvolumegroupnodestatuses",
}

// LVMOPlugin cleans up LVM Operator state from a node before deletion.
//
// Physical disk cleanup (vgremove/wipefs) is delegated to Ironic automated
// cleaning (automatedCleaningMode: metadata on BareMetalHost). This plugin
// handles only the Kubernetes-side cleanup:
//  1. Deletes topolvm PVs on the node (strips their finalizers first)
//  2. Removes the LVMVolumeGroupNodeStatus for the node (strips operator finalizers)
type LVMOPlugin struct {
	BasePlugin
	dynamicClient dynamic.Interface
	namespace     string
}

// NewLVMOPlugin creates a new LVMO cleanup plugin
func NewLVMOPlugin(client kubernetes.Interface, dynamicClient dynamic.Interface, namespace string) *LVMOPlugin {
	return &LVMOPlugin{
		BasePlugin: BasePlugin{
			name:   constants.LVMOPluginName,
			client: client,
		},
		dynamicClient: dynamicClient,
		namespace:     namespace,
	}
}

// ShouldRun returns true if LVMO manages this node.
// Checks for LVMVolumeGroupNodeStatus first; falls back to topolvm PV detection.
func (p *LVMOPlugin) ShouldRun(node *corev1.Node) bool {
	_, err := p.dynamicClient.Resource(lvmVGNodeStatusGVR).Namespace(p.namespace).Get(
		context.Background(), node.Name, metav1.GetOptions{},
	)
	if err == nil {
		klog.InfoS("LVMVolumeGroupNodeStatus found - LVMO manages this node", "node", node.Name)
		return true
	}
	if !errors.IsNotFound(err) {
		// CRD may not be installed — fall through to PV check
		klog.V(2).InfoS("Could not check LVMVolumeGroupNodeStatus, trying PV detection", "node", node.Name, "error", err)
	}

	pvList, pvErr := p.client.CoreV1().PersistentVolumes().List(context.Background(), metav1.ListOptions{})
	if pvErr != nil {
		klog.V(2).InfoS("Could not list PVs for LVMO detection", "node", node.Name, "error", pvErr)
		return false
	}
	for _, pv := range pvList.Items {
		if pv.Spec.CSI != nil && pv.Spec.CSI.Driver == constants.TopolvmCSIDriver && pvBelongsToNode(&pv, node.Name) {
			klog.InfoS("Topolvm PV found - LVMO manages this node", "node", node.Name, "pv", pv.Name)
			return true
		}
	}
	return false
}

// Cleanup removes LVMO-related Kubernetes objects for the node.
// Physical disk cleanup is handled by Ironic automated cleaning (automatedCleaningMode: metadata).
func (p *LVMOPlugin) Cleanup(ctx context.Context, node *corev1.Node) error {
	klog.InfoS("Starting LVMO cleanup", "node", node.Name, "namespace", p.namespace)

	// Step 1: delete topolvm PVs so PVCs are unblocked
	if err := p.deleteNodePVs(ctx, node.Name); err != nil {
		klog.ErrorS(err, "Failed to delete topolvm PVs (continuing)", "node", node.Name)
	}

	// Step 2: remove LVMVolumeGroupNodeStatus so the operator stops reconciling the node
	if err := p.deleteLVMVGNodeStatus(ctx, node.Name); err != nil {
		klog.ErrorS(err, "Failed to delete LVMVolumeGroupNodeStatus (continuing)", "node", node.Name)
	}

	klog.InfoS("LVMO cleanup completed successfully", "node", node.Name)
	return nil
}

// deleteNodePVs deletes all PVs backed by topolvm on this node
func (p *LVMOPlugin) deleteNodePVs(ctx context.Context, nodeName string) error {
	pvList, err := p.client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list PVs: %w", err)
	}

	deleted := 0
	for _, pv := range pvList.Items {
		if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != constants.TopolvmCSIDriver {
			continue
		}
		if !pvBelongsToNode(&pv, nodeName) {
			continue
		}

		klog.InfoS("Removing topolvm PV", "pv", pv.Name, "node", nodeName)

		// Strip finalizers so deletion isn't blocked by topolvm controller
		if len(pv.Finalizers) > 0 {
			patch := []byte(`{"metadata":{"finalizers":[]}}`)
			if _, pErr := p.client.CoreV1().PersistentVolumes().Patch(
				ctx, pv.Name, types.MergePatchType, patch, metav1.PatchOptions{},
			); pErr != nil && !errors.IsNotFound(pErr) {
				klog.ErrorS(pErr, "Failed to remove PV finalizers", "pv", pv.Name)
			}
		}

		if dErr := p.client.CoreV1().PersistentVolumes().Delete(ctx, pv.Name, metav1.DeleteOptions{}); dErr != nil && !errors.IsNotFound(dErr) {
			klog.ErrorS(dErr, "Failed to delete PV", "pv", pv.Name)
			continue
		}
		deleted++
	}

	klog.InfoS("Topolvm PVs removed", "node", nodeName, "count", deleted)
	return nil
}

// deleteLVMVGNodeStatus removes the operator finalizers from and deletes the node's LVMVolumeGroupNodeStatus
func (p *LVMOPlugin) deleteLVMVGNodeStatus(ctx context.Context, nodeName string) error {
	resource := p.dynamicClient.Resource(lvmVGNodeStatusGVR).Namespace(p.namespace)

	existing, err := resource.Get(ctx, nodeName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to get LVMVolumeGroupNodeStatus: %w", err)
	}

	if finalizers := existing.GetFinalizers(); len(finalizers) > 0 {
		klog.InfoS("Stripping finalizers from LVMVolumeGroupNodeStatus", "node", nodeName, "finalizers", finalizers)
		existing.SetFinalizers([]string{})
		if _, uErr := resource.Update(ctx, existing, metav1.UpdateOptions{}); uErr != nil && !errors.IsNotFound(uErr) {
			klog.ErrorS(uErr, "Failed to strip finalizers from LVMVolumeGroupNodeStatus", "node", nodeName)
		}
	}

	if dErr := resource.Delete(ctx, nodeName, metav1.DeleteOptions{}); dErr != nil && !errors.IsNotFound(dErr) {
		return fmt.Errorf("failed to delete LVMVolumeGroupNodeStatus: %w", dErr)
	}

	klog.InfoS("Deleted LVMVolumeGroupNodeStatus", "node", nodeName)
	return nil
}

// pvBelongsToNode checks if the PV has node affinity for the given node
func pvBelongsToNode(pv *corev1.PersistentVolume, nodeName string) bool {
	if pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil {
		return false
	}
	for _, term := range pv.Spec.NodeAffinity.Required.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key == "kubernetes.io/hostname" {
				for _, v := range expr.Values {
					if v == nodeName {
						return true
					}
				}
			}
		}
	}
	return false
}
