package webhook

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/894/node-cleanup-webhook/pkg/constants"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

// Server handles admission webhook requests
type Server struct{}

// NewServer creates a new webhook server
func NewServer() *Server {
	return &Server{}
}

// HandleMutateNode handles the /mutate-node webhook endpoint
func (s *Server) HandleMutateNode(w http.ResponseWriter, r *http.Request) {
	klog.V(2).Info("Received mutate request")

	// Read request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		klog.Errorf("Failed to read request body: %v", err)
		http.Error(w, "failed to read request", http.StatusBadRequest)
		return
	}

	// Parse AdmissionReview
	var admissionReview admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &admissionReview); err != nil {
		klog.Errorf("Failed to unmarshal admission review: %v", err)
		http.Error(w, "failed to unmarshal request", http.StatusBadRequest)
		return
	}

	// Process the request
	response := s.mutateNode(admissionReview.Request)

	// Build response
	admissionReview.Response = response
	admissionReview.Response.UID = admissionReview.Request.UID

	respBytes, err := json.Marshal(admissionReview)
	if err != nil {
		klog.Errorf("Failed to marshal response: %v", err)
		http.Error(w, "failed to marshal response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(respBytes)
}

func (s *Server) mutateNode(req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	// Only handle CREATE operations
	if req.Operation != admissionv1.Create {
		klog.V(2).Infof("Skipping non-CREATE operation: %s", req.Operation)
		return &admissionv1.AdmissionResponse{Allowed: true}
	}

	// Parse the node object
	var node corev1.Node
	if err := json.Unmarshal(req.Object.Raw, &node); err != nil {
		klog.Errorf("Failed to unmarshal node: %v", err)
		return &admissionv1.AdmissionResponse{
			Allowed: false,
			Result: &metav1.Status{
				Message: fmt.Sprintf("failed to unmarshal node: %v", err),
			},
		}
	}

	klog.Infof("Adding finalizer to node %s", node.Name)

	// Check if finalizer already exists
	for _, f := range node.Finalizers {
		if f == constants.FinalizerName {
			klog.V(2).Infof("Finalizer already present on node %s", node.Name)
			return &admissionv1.AdmissionResponse{Allowed: true}
		}
	}

	// Create JSON patch to add finalizer
	patch := []map[string]interface{}{}

	if len(node.Finalizers) == 0 {
		// Finalizers array doesn't exist, create it
		patch = append(patch, map[string]interface{}{
			"op":    "add",
			"path":  "/metadata/finalizers",
			"value": []string{constants.FinalizerName},
		})
	} else {
		// Append to existing finalizers
		patch = append(patch, map[string]interface{}{
			"op":    "add",
			"path":  "/metadata/finalizers/-",
			"value": constants.FinalizerName,
		})
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		klog.Errorf("Failed to marshal patch: %v", err)
		return &admissionv1.AdmissionResponse{
			Allowed: false,
			Result: &metav1.Status{
				Message: fmt.Sprintf("failed to create patch: %v", err),
			},
		}
	}

	klog.V(2).Infof("Patch for node %s: %s", node.Name, string(patchBytes))

	patchType := admissionv1.PatchTypeJSONPatch
	return &admissionv1.AdmissionResponse{
		Allowed:   true,
		Patch:     patchBytes,
		PatchType: &patchType,
	}
}
