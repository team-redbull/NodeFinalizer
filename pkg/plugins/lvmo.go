package plugins

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/894/node-cleanup-webhook/pkg/constants"
	batchv1 "k8s.io/api/batch/v1"
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
// The LVMO operator blocks manual VG deletion via finalizers, so this plugin:
//  1. Deletes topolvm PVs on the node (strips their finalizers first)
//  2. Removes the LVMVolumeGroupNodeStatus for the node (strips operator finalizers)
//  3. Runs a privileged Job on the node using nsenter to wipe VGs, PVs, and disk signatures
type LVMOPlugin struct {
	BasePlugin
	dynamicClient  dynamic.Interface
	namespace      string
	cleanupImage   string
	cleanupTimeout time.Duration
}

// NewLVMOPlugin creates a new LVMO cleanup plugin
func NewLVMOPlugin(client kubernetes.Interface, dynamicClient dynamic.Interface, namespace, cleanupImage string, cleanupTimeout time.Duration) *LVMOPlugin {
	return &LVMOPlugin{
		BasePlugin: BasePlugin{
			name:   constants.LVMOPluginName,
			client: client,
		},
		dynamicClient:  dynamicClient,
		namespace:      namespace,
		cleanupImage:   cleanupImage,
		cleanupTimeout: cleanupTimeout,
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

// Cleanup performs the full LVMO cleanup sequence for the node
func (p *LVMOPlugin) Cleanup(ctx context.Context, node *corev1.Node) error {
	klog.InfoS("Starting LVMO cleanup", "node", node.Name, "namespace", p.namespace)

	// Step 1: delete topolvm PVs so PVCs are unblocked
	if err := p.deleteNodePVs(ctx, node.Name); err != nil {
		// non-fatal: log and continue — the force job will wipe storage anyway
		klog.ErrorS(err, "Failed to delete topolvm PVs (continuing)", "node", node.Name)
	}

	// Step 2: remove LVMVolumeGroupNodeStatus so the operator stops reconciling the node
	if err := p.deleteLVMVGNodeStatus(ctx, node.Name); err != nil {
		klog.ErrorS(err, "Failed to delete LVMVolumeGroupNodeStatus (continuing)", "node", node.Name)
	}

	// Step 3: privileged job on the node to wipe VGs, PVs, and disk signatures
	if err := p.runForceCleanupJob(ctx, node.Name); err != nil {
		return fmt.Errorf("LVMO force cleanup failed: %w", err)
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

// runForceCleanupJob creates a privileged Job on the target node that enters the host
// namespaces via nsenter and runs vgremove / pvremove / wipefs to wipe all LVM state
func (p *LVMOPlugin) runForceCleanupJob(ctx context.Context, nodeName string) error {
	jobName := "lvmo-cleanup-" + sanitizeName(nodeName)

	// Delete any leftover job from a previous attempt
	foreground := metav1.DeletePropagationForeground
	_ = p.client.BatchV1().Jobs(p.namespace).Delete(ctx, jobName, metav1.DeleteOptions{
		PropagationPolicy: &foreground,
	})
	// Brief wait for the old job's pods to terminate
	select {
	case <-time.After(3 * time.Second):
	case <-ctx.Done():
		return ctx.Err()
	}

	klog.InfoS("Creating LVM force-cleanup Job", "job", jobName, "node", nodeName, "image", p.cleanupImage)

	privileged := true
	ttl := int32(300)
	backoffLimit := int32(0)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: p.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "node-cleanup-webhook",
				"node-cleanup/node":            nodeName,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			BackoffLimit:            &backoffLimit,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					// ServiceAccount must have the privileged SCC in OpenShift
					ServiceAccountName: constants.LVMOCleanupJobSA,
					// Pin the Job to exactly the node being cleaned up
					NodeSelector: map[string]string{
						"kubernetes.io/hostname": nodeName,
					},
					// Tolerate all taints so we land on a tainted/draining node
					Tolerations: []corev1.Toleration{
						{Operator: corev1.TolerationOpExists},
					},
					HostPID:       true, // required for nsenter to find host PID 1
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:  "lvm-cleanup",
							Image: p.cleanupImage,
							// nsenter enters all host namespaces of PID 1, then runs the cleanup script
							// using the host's own binaries (vgremove, pvremove, wipefs)
							Command: []string{
								"nsenter", "--target", "1",
								"--mount", "--uts", "--ipc", "--net", "--pid", "--",
								"sh", "-c", constants.LVMOForceCleanupScript,
							},
							SecurityContext: &corev1.SecurityContext{
								Privileged: &privileged,
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "host-dev", MountPath: "/dev"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "host-dev",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{Path: "/dev"},
							},
						},
					},
				},
			},
		},
	}

	if _, err := p.client.BatchV1().Jobs(p.namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("failed to create cleanup job: %w", err)
	}

	return p.waitForJob(ctx, jobName)
}

// waitForJob polls until the Job succeeds or fails
func (p *LVMOPlugin) waitForJob(ctx context.Context, jobName string) error {
	deadline := time.Now().Add(p.cleanupTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}

		job, err := p.client.BatchV1().Jobs(p.namespace).Get(ctx, jobName, metav1.GetOptions{})
		if err != nil {
			klog.ErrorS(err, "Failed to poll Job status", "job", jobName)
			continue
		}
		if job.Status.Succeeded > 0 {
			klog.InfoS("LVM force-cleanup Job succeeded", "job", jobName)
			return nil
		}
		if job.Status.Failed > 0 {
			return fmt.Errorf("LVM force-cleanup job %s failed", jobName)
		}
		klog.V(2).InfoS("LVM force-cleanup Job still running", "job", jobName)
	}
	return fmt.Errorf("LVM force-cleanup job %s timed out after %v", jobName, p.cleanupTimeout)
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

// sanitizeName converts a node name into a string valid for use in a Job name
func sanitizeName(name string) string {
	var sb strings.Builder
	for _, c := range strings.ToLower(name) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			sb.WriteRune(c)
		} else {
			sb.WriteRune('-')
		}
	}
	result := strings.Trim(sb.String(), "-")
	if len(result) > 40 {
		result = result[:40]
	}
	return result
}
