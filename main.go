// kube-plex-gpu: A Plex Transcoder shim that offloads transcode jobs to
// Kubernetes pods with GPU (NVIDIA) hardware acceleration.
//
// This binary replaces /usr/lib/plexmediaserver/Plex Transcoder inside the
// PMS container. When Plex calls the transcoder, this shim creates a K8s pod
// that runs the real transcoder with GPU access, waits for completion, then
// exits with the pod's exit code.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	log.SetPrefix("[kube-plex-gpu] ")
	log.SetFlags(log.Ltime)

	// Required env vars
	namespace := envOrDie("KUBE_NAMESPACE")
	pmsImage := envOrDie("PMS_IMAGE")
	pmsAddr := envOrDie("PMS_INTERNAL_ADDRESS")
	// Media mount configuration — these must match PMS container mounts
	// so file paths in transcoder args resolve correctly.
	dataPVC := envOrDie("DATA_PVC")
	configPVC := envOrDie("CONFIG_PVC")
	transcodePVC := envOrDie("TRANSCODE_PVC")

	// Media mount paths (must match PMS container mounts)
	dataMount := envOr("DATA_MOUNT", "/data")
	configMount := envOr("CONFIG_MOUNT", "/config")
	transcodeMount := envOr("TRANSCODE_MOUNT", "/transcode")

	// Optional sub-path mounts for media (comma-separated mount:subPath pairs)
	// e.g. "/media/movies:movies,/media/tv:tv"
	mediaSubPaths := envOr("MEDIA_SUB_PATHS", "")

	// Optional subPath for the transcode volume (e.g. "transcode" to use a
	// subdirectory of the PVC rather than its root)
	transcodeSubPath := envOr("TRANSCODE_SUB_PATH", "")

	// Optional GPU config
	gpuCount := envOr("GPU_COUNT", "1")
	runtimeClass := envOr("RUNTIME_CLASS", "nvidia")

	// Rewrite transcoder args: replace localhost URLs with the internal
	// service address so the pod can report progress back to PMS.
	args := rewriteArgs(os.Args[1:], pmsAddr)

	log.Printf("Creating transcode pod (gpu=%s, image=%s)", gpuCount, pmsImage)
	log.Printf("Transcoder args: %s", strings.Join(args, " "))

	// In-cluster K8s client
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("Failed to get in-cluster config: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create K8s client: %v", err)
	}

	ctx := context.Background()

	// Build the transcode pod spec
	pod := buildPod(pmsImage, namespace, args, dataPVC, configPVC, transcodePVC, dataMount, configMount, transcodeMount, transcodeSubPath, mediaSubPaths, gpuCount, runtimeClass)

	// Create the pod
	created, err := clientset.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		log.Fatalf("Failed to create pod: %v", err)
	}
	podName := created.Name
	log.Printf("Created pod %s", podName)

	// Ensure cleanup on exit
	defer func() {
		log.Printf("Deleting pod %s", podName)
		_ = clientset.CoreV1().Pods(namespace).Delete(ctx, podName, metav1.DeleteOptions{})
	}()

	// Watch for pod completion
	exitCode, err := waitForCompletion(ctx, clientset, namespace, podName)
	if err != nil {
		log.Fatalf("Error waiting for pod: %v", err)
	}

	log.Printf("Pod %s finished with exit code %d", podName, exitCode)
	os.Exit(exitCode)
}

func buildPod(image, namespace string, args []string, dataPVC, configPVC, transcodePVC, dataMount, configMount, transcodeMount, transcodeSubPath, mediaSubPaths, gpuCount, runtimeClass string) *corev1.Pod {
	// Build the full command: the real transcoder binary path + args
	command := append([]string{"/usr/lib/plexmediaserver/Plex Transcoder"}, args...)

	// GPU resource request
	gpuQty := resource.MustParse(gpuCount)

	// Pass through all current env vars so the transcoder has PMS context
	var envVars []corev1.EnvVar
	for _, e := range os.Environ() {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envVars = append(envVars, corev1.EnvVar{Name: parts[0], Value: parts[1]})
		}
	}

	// Add NVIDIA visible devices
	envVars = append(envVars, corev1.EnvVar{Name: "NVIDIA_VISIBLE_DEVICES", Value: "all"})
	envVars = append(envVars, corev1.EnvVar{Name: "NVIDIA_DRIVER_CAPABILITIES", Value: "compute,video,utility"})

	cwd, _ := os.Getwd()

	// Base volume mounts
	transcodeVM := corev1.VolumeMount{Name: "transcode", MountPath: transcodeMount}
	if transcodeSubPath != "" {
		transcodeVM.SubPath = transcodeSubPath
	}
	volumeMounts := []corev1.VolumeMount{
		{Name: "data", MountPath: dataMount, ReadOnly: true},
		{Name: "config", MountPath: configMount, ReadOnly: true},
		transcodeVM,
	}

	// Add sub-path media mounts (e.g. "/media/movies:movies,/media/tv:tv")
	// These mount the data PVC at additional paths with subPath so that
	// file paths in transcoder args (like /media/movies/...) resolve correctly.
	if mediaSubPaths != "" {
		for _, pair := range strings.Split(mediaSubPaths, ",") {
			parts := strings.SplitN(strings.TrimSpace(pair), ":", 2)
			if len(parts) == 2 {
				volumeMounts = append(volumeMounts, corev1.VolumeMount{
					Name:      "data",
					MountPath: parts[0],
					SubPath:   parts[1],
					ReadOnly:  true,
				})
			}
		}
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "plex-transcode-gpu-",
			Namespace:    namespace,
			Labels: map[string]string{
				"app":                          "plex-transcode",
				"app.kubernetes.io/component":  "transcoder",
				"app.kubernetes.io/managed-by": "kube-plex-gpu",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:    corev1.RestartPolicyNever,
			RuntimeClassName: &runtimeClass,
			// Tolerate GPU taint so we schedule to hp nodes
			Tolerations: []corev1.Toleration{
				{
					Key:      "nvidia.com/gpu",
					Operator: corev1.TolerationOpExists,
					Effect:   corev1.TaintEffectNoSchedule,
				},
			},
			NodeSelector: map[string]string{
				"nvidia.com/gpu.present": "true",
			},
			Containers: []corev1.Container{
				{
					Name:       "transcode",
					Image:      image,
					Command:    command,
					Env:        envVars,
					WorkingDir: cwd,
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							"nvidia.com/gpu": gpuQty,
						},
					},
					VolumeMounts: volumeMounts,
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: dataPVC,
							ReadOnly:  true,
						},
					},
				},
				{
					Name: "config",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: configPVC,
							ReadOnly:  true,
						},
					},
				},
				{
					Name: "transcode",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: transcodePVC,
						},
					},
				},
			},
		},
	}
}

// rewriteArgs replaces localhost/127.0.0.1 URLs in transcoder arguments with
// the PMS internal service address so pods on other nodes can reach PMS.
func rewriteArgs(args []string, pmsAddr string) []string {
	out := make([]string, len(args))
	for i, arg := range args {
		arg = strings.ReplaceAll(arg, "http://127.0.0.1:32400", pmsAddr)
		arg = strings.ReplaceAll(arg, "http://localhost:32400", pmsAddr)
		out[i] = arg
	}
	return out
}

// waitForCompletion watches the pod until it succeeds or fails.
func waitForCompletion(ctx context.Context, clientset *kubernetes.Clientset, namespace, podName string) (int, error) {
	timeout := int64(7200) // 2 hour max transcode
	watcher, err := clientset.CoreV1().Pods(namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector:  fmt.Sprintf("metadata.name=%s", podName),
		TimeoutSeconds: &timeout,
	})
	if err != nil {
		return 1, fmt.Errorf("failed to watch pod: %w", err)
	}
	defer watcher.Stop()

	for event := range watcher.ResultChan() {
		if event.Type == watch.Deleted {
			return 1, fmt.Errorf("pod was deleted")
		}

		pod, ok := event.Object.(*corev1.Pod)
		if !ok {
			continue
		}

		switch pod.Status.Phase {
		case corev1.PodSucceeded:
			return 0, nil
		case corev1.PodFailed:
			// Try to get container exit code
			for _, cs := range pod.Status.ContainerStatuses {
				if cs.Name == "transcode" && cs.State.Terminated != nil {
					return int(cs.State.Terminated.ExitCode), nil
				}
			}
			return 1, nil
		case corev1.PodPending:
			// Log if waiting for GPU node
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse {
					log.Printf("Pod pending: %s", cond.Message)
				}
			}
		}
	}

	return 1, fmt.Errorf("watch channel closed unexpectedly")
}

func envOrDie(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("Required env var %s not set", key)
	}
	return v
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
