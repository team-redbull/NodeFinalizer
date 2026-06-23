# LVMO Plugin — Deep Explanation

## The Problem

LVM Operator (LVMO) manages LVM Volume Groups (VGs) on Kubernetes nodes.
When you try to delete a node that LVMO is managing, two things block you:

1. **The LVMO operator holds finalizers** on `LVMVolumeGroupNodeStatus` resources.
   As long as those finalizers exist, Kubernetes will not allow the resource to be
   deleted, and the operator will keep reconciling (recreating) VGs on the node.

2. **topolvm PVs have finalizers** held by the topolvm CSI controller.
   If you try to delete a PV manually, the CSI controller blocks it until it has
   cleaned up the underlying logical volume — but the logical volume is on the node
   being deleted, so the CSI controller may hang forever.

The result: the node gets stuck in `Terminating` indefinitely.

---

## The Solution

The LVMO plugin breaks the deadlock in three ordered steps:

```
Step 1: Delete topolvm PVs (strip CSI finalizers first)
         └─> Unblocks PVC deletion, removes Kubernetes references to the storage

Step 2: Delete LVMVolumeGroupNodeStatus (strip operator finalizers first)
         └─> Stops LVMO operator from reconciling / re-creating the VG on this node

Step 3: Run a privileged Job ON the node via nsenter
         └─> Enters host namespaces, runs vgremove / pvremove / wipefs
         └─> Physically wipes all LVM state and disk signatures
```

---

## Files Changed

### 1. `pkg/constants/constants.go`

Added a new constants block for LVMO. Nothing existing was changed.

```go
const (
    LVMOPluginName            = "lvmo"
    LVMODefaultNamespace      = "openshift-storage"
    LVMODefaultCleanupImage   = "registry.redhat.io/ubi9/ubi-minimal:latest"
    LVMODefaultCleanupTimeout = 5 * time.Minute
    TopolvmCSIDriver          = "topolvm.io"
)
```

**Why each constant:**

- `LVMOPluginName` — the key used in `ENABLED_PLUGINS=lvmo`. Keeping it in constants
  avoids magic strings scattered across files.

- `LVMODefaultNamespace` — LVMO (Red Hat's LVM Operator) installs into
  `openshift-storage` by default. This is where the `LVMVolumeGroupNodeStatus` CRs
  live and where the cleanup Job will be created. Overridable via `LVMO_NAMESPACE`.

- `LVMODefaultCleanupImage` — the container image for the force-cleanup Job.
  It needs to be a minimal Linux image that can run `nsenter` (which is in
  `util-linux`, included in UBI minimal). The actual LVM commands (`vgremove`,
  `pvremove`, `wipefs`) come from the **host** via `nsenter`, not from this image.

- `LVMODefaultCleanupTimeout` — how long the plugin will wait for the cleanup Job
  to finish before declaring failure. 5 minutes is generous for disk wiping.

- `TopolvmCSIDriver` — the CSI driver name that topolvm registers with Kubernetes.
  Used to identify which PVs on the node are topolvm-managed vs. other storage.

```go
const LVMOForceCleanupScript = `set -e
echo "=== Starting LVM force cleanup ==="
for vg in $(vgs --noheadings -o vg_name 2>/dev/null | tr -d ' '); do
  echo "Removing LVs in VG: $vg"
  lvremove -f "$vg" 2>/dev/null || true
  echo "Removing VG: $vg"
  vgremove -f "$vg" 2>/dev/null || true
done
for pv in $(pvs --noheadings -o pv_name 2>/dev/null | tr -d ' '); do
  echo "Removing PV: $pv"
  pvremove -f "$pv" 2>/dev/null || true
  echo "Wiping signatures on: $pv"
  wipefs -a "$pv" 2>/dev/null || true
done
echo "=== LVM force cleanup completed ==="`
```

**Why this script is structured this way:**

- `set -e` — exit immediately on any unhandled error so the Job fails cleanly
  instead of silently skipping steps.

- `lvremove -f "$vg"` before `vgremove` — you must remove Logical Volumes before
  removing the Volume Group that contains them. Skipping this makes `vgremove` fail.

- `2>/dev/null || true` on every command — if a VG or PV is already gone (e.g.,
  the operator partially cleaned it), the command would exit non-zero and abort the
  script due to `set -e`. The `|| true` prevents that without hiding real errors.

- `wipefs -a "$pv"` — removes all filesystem and partition table signatures from
  the raw disk. Without this, the disk still looks like an LVM PV to any future
  tool that scans it, which can cause problems when the disk is reused.

- The script runs on the **host** via `nsenter`. The container image just needs to
  provide a shell and `nsenter`. The LVM binaries (`vgs`, `pvs`, `vgremove`, etc.)
  are read from the host's filesystem because `nsenter --mount` enters the host
  mount namespace.

---

### 2. `pkg/config/config.go`

Added an LVMO config block inside `loadPluginConfigs()`. Nothing existing was changed.

```go
c.PluginConfigs["lvmo"] = PluginConfig{
    Enabled: c.isPluginEnabled("lvmo"),
    Options: map[string]string{
        "namespace":      getEnv("LVMO_NAMESPACE", "openshift-storage"),
        "cleanupImage":   getEnv("LVMO_CLEANUP_IMAGE", "registry.redhat.io/ubi9/ubi-minimal:latest"),
        "cleanupTimeout": getEnv("LVMO_CLEANUP_TIMEOUT", "300s"),
    },
}
```

**Why each env var:**

- `LVMO_NAMESPACE` — if LVMO is installed in a custom namespace (e.g., `lvm-operator`
  in upstream Kubernetes), you can override this without recompiling.

- `LVMO_CLEANUP_IMAGE` — in air-gapped environments, you cannot pull from
  `registry.redhat.io`. Override this with an internal mirror image.
  The image only needs `nsenter` (from `util-linux`) — a very minimal requirement.

- `LVMO_CLEANUP_TIMEOUT` — controls both how long we wait for the cleanup Job
  to complete AND how long we poll `LVMVolumeGroupNodeStatus` for operator cleanup.
  Tune this up on large disks.

These values flow into `main.go` via `cfg.GetPluginOption("lvmo", ...)` and
`cfg.GetPluginOptionDuration("lvmo", ...)` — the same pattern used by portworx.

---

### 3. `pkg/plugins/lvmo.go` (new file)

#### Struct

```go
type LVMOPlugin struct {
    BasePlugin
    dynamicClient  dynamic.Interface
    namespace      string
    cleanupImage   string
    cleanupTimeout time.Duration
}
```

`BasePlugin` is embedded (same as logger and portworx) and provides `Name()` and
the `kubernetes.Interface` client. The LVMO plugin needs two extras:

- `dynamicClient` — the standard `kubernetes.Interface` only knows about built-in
  Kubernetes resources (Pods, Nodes, PVs, etc.). `LVMVolumeGroupNodeStatus` is a
  Custom Resource, so we need `dynamic.Interface` which can talk to any CRD using
  its Group/Version/Resource (GVR) tuple.

- `namespace` — unlike Nodes or PVs (which are cluster-scoped), LVMO CRs are
  namespace-scoped. We need to know which namespace to query.

#### `ShouldRun`

```go
func (p *LVMOPlugin) ShouldRun(node *corev1.Node) bool {
    // Primary check: LVMVolumeGroupNodeStatus named after the node
    _, err := p.dynamicClient.Resource(lvmVGNodeStatusGVR).Namespace(p.namespace).Get(...)
    if err == nil { return true }

    // Fallback: check for topolvm PVs with node affinity for this node
    for _, pv := range pvList.Items {
        if pv.Spec.CSI.Driver == constants.TopolvmCSIDriver && pvBelongsToNode(&pv, node.Name) {
            return true
        }
    }
    return false
}
```

**Why two detection methods:**

The `LVMVolumeGroupNodeStatus` check is the most reliable signal — LVMO creates one
per node it manages. But if the CRD is not installed (i.e., LVMO is not in this
cluster), the API call returns a "resource not found" error (not `IsNotFound` on the
object, but a different API error). In that case we fall back to scanning PVs for
the topolvm CSI driver. If neither check finds evidence of LVMO, `ShouldRun` returns
`false` and the plugin is entirely skipped for that node.

**Why this matters:** `ShouldRun` is called for every node deletion. On clusters
without LVMO, this plugin should be a no-op with minimal overhead.

#### `deleteNodePVs`

```go
func (p *LVMOPlugin) deleteNodePVs(ctx context.Context, nodeName string) error {
    // List all PVs, filter by CSI driver and node affinity
    // For each matching PV:
    //   1. PATCH metadata.finalizers = [] (strip finalizers)
    //   2. DELETE the PV
}
```

**Why strip finalizers before deleting:**

topolvm adds a finalizer (e.g., `topolvm.io/pvc-protection`) to its PVs. When you
issue a DELETE, Kubernetes marks it for deletion but won't actually remove it while
any finalizer remains. The topolvm CSI controller is supposed to clean up the
underlying logical volume and then remove the finalizer — but the logical volume is
on the node that's being deleted and the node may be unreachable, so the controller
hangs. Patching `finalizers: []` bypasses this and lets the DELETE go through.

**Why it is non-fatal:**

The step is best-effort. Even if a PV deletion fails, the force-cleanup Job in
Step 3 will physically wipe the underlying disk anyway. The PV deletion is
primarily to keep the Kubernetes API state clean.

**How `pvBelongsToNode` works:**

topolvm PVs have `spec.nodeAffinity.required.nodeSelectorTerms` with
`kubernetes.io/hostname: <node-name>`. This is the standard way Kubernetes
local-storage PVs are pinned to a node. We walk that structure to check if the
node name appears as a value.

#### `deleteLVMVGNodeStatus`

```go
func (p *LVMOPlugin) deleteLVMVGNodeStatus(ctx context.Context, nodeName string) error {
    // GET the resource
    // PATCH finalizers = []
    // DELETE the resource
}
```

**Why this is the key step:**

`LVMVolumeGroupNodeStatus` is what the LVMO operator watches to know which nodes it
manages. As long as this resource exists, the operator's reconcile loop will keep
trying to ensure the VG is present on the node. By deleting it (after stripping
the operator's own finalizers), we remove the node from LVMO's view of the world.

**Why we strip finalizers with `Update` not `Patch`:**

We already have the full object from the `Get` call. We modify `Finalizers` on
the in-memory object and call `Update`. This is simpler and avoids constructing
a JSON Merge Patch for an unstructured object. A conflict (409) here is unlikely
because the node is being deleted and the operator is not actively reconciling it.

#### `runForceCleanupJob`

This creates the following Job:

```yaml
spec:
  template:
    spec:
      nodeSelector:
        kubernetes.io/hostname: <node-name>   # pin to the exact node being deleted
      tolerations:
        - operator: Exists                     # tolerate ALL taints (node may be cordoned/tainted)
      hostPID: true                            # required: lets nsenter find PID 1 on the host
      restartPolicy: Never
      containers:
        - name: lvm-cleanup
          image: <cleanupImage>
          securityContext:
            privileged: true                   # required: nsenter needs CAP_SYS_ADMIN
          command:
            - nsenter
            - --target
            - "1"
            - --mount
            - --uts
            - --ipc
            - --net
            - --pid
            - --
            - sh
            - -c
            - <LVMOForceCleanupScript>
          volumeMounts:
            - name: host-dev
              mountPath: /dev
      volumes:
        - name: host-dev
          hostPath:
            path: /dev
```

**Why `hostPID: true`:**

`nsenter --target 1` means "enter the namespaces of PID 1". PID 1 inside the
container is the container's init process, not the host's. `hostPID: true` makes
the host's PID namespace visible inside the container, so PID 1 inside the
container is the host's init process (systemd or similar). Without this,
`nsenter --target 1` would enter the container's own namespaces — which is a no-op.

**Why `privileged: true`:**

`nsenter` with `--mount` requires `CAP_SYS_CHROOT` and `CAP_SYS_ADMIN`. These are
only available in a privileged container. Without privilege, the `nsenter` call
will fail with `Operation not permitted`.

**Why `tolerations: - operator: Exists`:**

When a node is being deleted, it is typically:
- Cordoned (taint: `node.kubernetes.io/unschedulable`)
- May have NoSchedule taints from the cloud provider

Without tolerating all taints, the cleanup Job's pod would never be scheduled on
the node — it would just sit in `Pending` forever.

**Why mount `/dev` from the host:**

The cleanup script runs `wipefs -a <disk>` on raw block devices (e.g., `/dev/sdb`).
Even though `nsenter --mount` enters the host mount namespace, the container's
`/dev` is still the container's device tree unless we explicitly mount the host's.
The `hostPath: /dev` volume ensures the container (and therefore the host commands
via nsenter) can see the actual block devices.

**Why `TTLSecondsAfterFinished: 300`:**

After the Job completes (success or failure), Kubernetes will automatically delete
it after 5 minutes. Without TTL, completed Jobs pile up in the namespace. We still
wait for completion synchronously in `waitForJob`, so TTL is just housekeeping.

**Why `BackoffLimit: 0`:**

If the script fails (e.g., `vgremove` returns an error that isn't caught by
`|| true`), we do not want Kubernetes to retry. A retry would attempt to wipe
already-wiped disks, and would delay the overall cleanup. If it fails, we return
an error from the plugin and the watcher's retry logic will re-enqueue the node.

#### `waitForJob`

```go
func (p *LVMOPlugin) waitForJob(ctx context.Context, jobName string) error {
    deadline := time.Now().Add(p.cleanupTimeout)
    for time.Now().Before(deadline) {
        // sleep 10s
        // GET the Job
        // if Succeeded > 0: return nil
        // if Failed > 0: return error
    }
    return fmt.Errorf("timed out")
}
```

**Why poll every 10 seconds instead of using a watch:**

A `Watch` on the Job would be more efficient, but it adds complexity (reconnect
logic, initial state handling). Given that disk wiping typically takes 10-60 seconds
and the timeout is 5 minutes, polling every 10 seconds means at most 30 API calls.
This is acceptable and the code stays simple.

---

### 4. `cmd/webhook/main.go`

#### `createK8sClient` → `buildRestConfig`

**Before:**
```go
func createK8sClient(kubeconfig string, insecureSkipTLSVerify bool) (kubernetes.Interface, error) {
    // builds restConfig, creates kubernetes client, returns it
}
```

**After:**
```go
func buildRestConfig(kubeconfig string, insecureSkipTLSVerify bool) (*rest.Config, error) {
    // builds restConfig, returns it
}
// In main():
restConfig, _ := buildRestConfig(...)
client, _     := kubernetes.NewForConfig(restConfig)
dynamicClient, _ := dynamic.NewForConfig(restConfig)
```

**Why:** `dynamic.NewForConfig` needs the same `*rest.Config` object. The original
function built the config internally and threw it away after creating the kubernetes
client. Exposing it lets us create both clients from a single config, which means
a single auth handshake and a single set of connection settings (TLS, timeout, QPS).

#### Plugin registration

```go
pluginRegistry.Register(plugins.NewLVMOPlugin(
    client,
    dynamicClient,
    cfg.GetPluginOption("lvmo", "namespace", constants.LVMODefaultNamespace),
    cfg.GetPluginOption("lvmo", "cleanupImage", constants.LVMODefaultCleanupImage),
    cfg.GetPluginOptionDuration("lvmo", "cleanupTimeout", constants.LVMODefaultCleanupTimeout),
))
```

This follows the exact same pattern as logger and portworx. The plugin is
**registered** (available) but not **enabled** until `ENABLED_PLUGINS=lvmo` is set.
If `lvmo` is not in `ENABLED_PLUGINS`, this line has zero runtime cost.

---

## RBAC Requirements

The webhook's ServiceAccount needs additional permissions to run this plugin.
Add these rules to the ClusterRole in `deploy/helm/node-cleanup-webhook/templates/`:

```yaml
# For topolvm PV management (already exists for nodes, add PVs)
- apiGroups: [""]
  resources: ["persistentvolumes"]
  verbs: ["get", "list", "patch", "delete"]

# For creating/watching the cleanup Job
- apiGroups: ["batch"]
  resources: ["jobs"]
  verbs: ["get", "create", "delete"]

# For reading/updating/deleting LVMVolumeGroupNodeStatus
- apiGroups: ["lvm.topolvm.io"]
  resources: ["lvmvolumegroupnodestatuses"]
  verbs: ["get", "list", "update", "delete"]
```

---

## Enabling the Plugin

```bash
# Minimal — just LVMO cleanup
export ENABLED_PLUGINS=logger,lvmo

# With overrides for a custom LVMO installation
export ENABLED_PLUGINS=logger,lvmo
export LVMO_NAMESPACE=lvm-operator          # if not using openshift-storage
export LVMO_CLEANUP_IMAGE=myrepo/ubi-minimal:9  # air-gapped mirror
export LVMO_CLEANUP_TIMEOUT=600s            # 10 min for large disks
```

In Helm values (once the chart is updated to pass ENABLED_PLUGINS as env):

```yaml
plugins:
  enabled:
    - logger
    - lvmo

env:
  LVMO_NAMESPACE: openshift-storage
  LVMO_CLEANUP_TIMEOUT: 300s
```

---

## Full Deletion Flow With LVMO Plugin

```
kubectl delete node worker-1
  │
  └─> Kubernetes sets DeletionTimestamp on worker-1
      (blocked by finalizer: infra.894.io/node-cleanup)
        │
        └─> Watcher detects DeletionTimestamp
            └─> Enqueues "worker-1" to workqueue
                └─> processNode("worker-1")
                    └─> runCleanup()
                        └─> Registry.RunAll()
                            │
                            ├─> logger plugin runs (prints banner, 15s delay for POC)
                            │
                            └─> lvmo plugin runs
                                │
                                ├─> Step 1: List PVs, find topolvm PVs for worker-1
                                │           Strip finalizers, delete PVs
                                │
                                ├─> Step 2: GET LVMVolumeGroupNodeStatus/worker-1
                                │           Strip operator finalizers (Update)
                                │           DELETE LVMVolumeGroupNodeStatus/worker-1
                                │
                                └─> Step 3: CREATE Job lvmo-cleanup-worker-1
                                            (privileged, hostPID, pinned to worker-1)
                                             │
                                             └─> Pod runs on worker-1
                                                 nsenter --target 1 --mount ...
                                                   vgremove -f <all VGs>
                                                   pvremove -f <all PVs>
                                                   wipefs -a <all disks>
                                             │
                                             └─> Job Succeeded
                                                 │
                                                 └─> Plugin returns nil
                                                     Watcher removes finalizer
                                                     Kubernetes deletes worker-1
```
