package sandbox

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gaurasha/agent-orch/backend/internal/k8s"
)

// kubernetesDriver runs each execution as a Pod.
//
// This is the production driver. The security properties come from the PodSpec
// below plus cluster-level policy that this driver assumes is in place and
// which deploy/k8s/base/ installs:
//
//   - RuntimeClass "gvisor": the container's syscalls are serviced by a
//     user-space kernel, so a Linux kernel LPE is not automatically a host
//     compromise. This is the single most important line in the file.
//   - A default-deny NetworkPolicy in the sandbox namespace, so the empty
//     egress list below is enforced by the CNI and not merely requested.
//   - A dedicated, tainted node pool, so a successful escape lands on a node
//     with no control-plane credentials and no other tenants' long-lived data.
//   - No ServiceAccount token mounted, so an escape does not immediately get
//     an API-server identity.
//
// Deliberate non-goal: this driver does NOT stream logs incrementally or reuse
// pods. Pod-per-call costs roughly 1-3 s of scheduling and image-pull-cache
// time, which is why the design uses warm per-run sandboxes for interactive
// agents. See DEEP_DIVE.md "D3 - sandbox lifecycle".
type kubernetesDriver struct {
	cfg    Config
	client *k8s.Client
}

func NewKubernetesDriver(cfg Config) (Driver, error) {
	cfg = cfg.withDefaults()
	cl, err := k8s.InCluster()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	return &kubernetesDriver{cfg: cfg, client: cl}, nil
}

func (d *kubernetesDriver) Name() string { return "kubernetes" }
func (d *kubernetesDriver) Close() error { return nil }

func (d *kubernetesDriver) Run(ctx context.Context, spec Spec) (Result, error) {
	started := time.Now()
	lim := spec.Limits.WithDefaults()
	if len(spec.Argv) == 0 {
		return Result{}, fmt.Errorf("%w: empty argv", ErrSetup)
	}
	name := "sbx-" + sanitizeID(strings.ToLower(spec.RunID)) + "-" + shortRand()
	if len(name) > 63 {
		name = name[:63]
	}
	ns := d.cfg.Namespace

	env := spec.Env
	if len(env) == 0 {
		env = SafeEnv()
	}
	envList := make([]map[string]any, 0, len(env))
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		envList = append(envList, map[string]any{"name": k, "value": v})
	}

	trueVal, falseVal := true, false
	pod := map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
			// Labels carry tenancy so NetworkPolicy, quota and forensic queries
			// can all select on it.
			"labels": map[string]string{
				"app.kubernetes.io/name":      "agentorch-sandbox",
				"agentorch.io/tenant":         sanitizeID(spec.TenantID),
				"agentorch.io/run":            sanitizeID(spec.RunID),
				"agentorch.io/workload-class": "untrusted",
			},
		},
		"spec": map[string]any{
			"restartPolicy": "Never",
			// gVisor. Without this the whole design rests on the host kernel
			// being free of privilege-escalation bugs, which it never is.
			"runtimeClassName": d.cfg.RuntimeClass,
			// An escaped payload should not find an API-server credential.
			"automountServiceAccountToken": false,
			"activeDeadlineSeconds":        int(lim.Wall.Seconds()) + 5,
			// Schedule only onto the untrusted-workload pool, and tolerate the
			// taint that keeps everything else off it.
			"nodeSelector": map[string]string{"agentorch.io/workload": "sandbox"},
			"tolerations": []map[string]any{{
				"key": "agentorch.io/workload", "operator": "Equal",
				"value": "sandbox", "effect": "NoSchedule",
			}},
			"securityContext": map[string]any{
				"runAsNonRoot":   true,
				"runAsUser":      containerUID,
				"runAsGroup":     containerUID,
				"fsGroup":        containerUID,
				"seccompProfile": map[string]any{"type": "RuntimeDefault"},
			},
			"containers": []map[string]any{{
				"name":       "payload",
				"image":      d.cfg.Image,
				"command":    spec.Argv,
				"workingDir": "/work",
				"env":        envList,
				"securityContext": map[string]any{
					"allowPrivilegeEscalation": falseVal,
					"readOnlyRootFilesystem":   trueVal,
					"privileged":               falseVal,
					"capabilities":             map[string]any{"drop": []string{"ALL"}},
				},
				"resources": map[string]any{
					// requests == limits puts the pod in the Guaranteed QoS
					// class, so a noisy neighbour cannot steal its CPU and it is
					// the last thing evicted under node pressure.
					"requests": map[string]string{
						"cpu":               fmt.Sprintf("%dm", lim.CPUMillis),
						"memory":            fmt.Sprintf("%d", lim.MemoryBytes),
						"ephemeral-storage": fmt.Sprintf("%d", lim.WorkspaceBytes+lim.TmpfsBytes),
					},
					"limits": map[string]string{
						"cpu":               fmt.Sprintf("%dm", lim.CPUMillis),
						"memory":            fmt.Sprintf("%d", lim.MemoryBytes),
						"ephemeral-storage": fmt.Sprintf("%d", lim.WorkspaceBytes+lim.TmpfsBytes),
					},
				},
				"volumeMounts": []map[string]any{
					{"name": "work", "mountPath": "/work"},
					{"name": "tmp", "mountPath": "/tmp"},
				},
			}},
			"volumes": []map[string]any{
				{"name": "work", "emptyDir": map[string]any{
					"sizeLimit": fmt.Sprintf("%d", lim.WorkspaceBytes)}},
				{"name": "tmp", "emptyDir": map[string]any{
					"medium":    "Memory",
					"sizeLimit": fmt.Sprintf("%d", lim.TmpfsBytes)}},
			},
		},
	}

	base := "/api/v1/namespaces/" + ns + "/pods"
	if err := d.client.Post(ctx, base, pod, nil); err != nil {
		return Result{}, fmt.Errorf("%w: create sandbox pod: %v", ErrSetup, err)
	}
	// Always clean up, including on context cancellation, or a cancelled run
	// leaks a pod that keeps consuming its resource request.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = d.client.Delete(cleanup, base+"/"+name)
	}()

	res, err := d.awaitCompletion(ctx, ns, name, lim, started)
	res.Driver = d.Name()
	return res, err
}

type podStatus struct {
	Status struct {
		Phase             string `json:"phase"`
		ContainerStatuses []struct {
			State struct {
				Terminated *struct {
					ExitCode   int32  `json:"exitCode"`
					Reason     string `json:"reason"`
					StartedAt  string `json:"startedAt"`
					FinishedAt string `json:"finishedAt"`
				} `json:"terminated"`
				Waiting *struct {
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"waiting"`
			} `json:"state"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

func (d *kubernetesDriver) awaitCompletion(ctx context.Context, ns, name string, lim Limits, started time.Time) (Result, error) {
	base := "/api/v1/namespaces/" + ns + "/pods/" + name
	// Allow the pod's own deadline to fire before we give up, plus scheduling
	// headroom, so a slow node does not look like a payload timeout.
	deadline := time.Now().Add(lim.Wall + 60*time.Second)
	poll := 200 * time.Millisecond
	var firstRunning time.Time

	for {
		if time.Now().After(deadline) {
			return Result{TimedOut: true, Duration: time.Since(started),
				Stderr: "sandbox pod did not reach a terminal state before the deadline"}, nil
		}
		select {
		case <-ctx.Done():
			return Result{Duration: time.Since(started)}, ctx.Err()
		case <-time.After(poll):
		}
		// Back off gently: a tight poll loop across hundreds of sandboxes is a
		// real load source on the API server.
		if poll < 2*time.Second {
			poll += 200 * time.Millisecond
		}

		var st podStatus
		if err := d.client.Get(ctx, base, &st); err != nil {
			return Result{Duration: time.Since(started)}, fmt.Errorf("poll sandbox pod: %w", err)
		}
		if firstRunning.IsZero() && st.Status.Phase == "Running" {
			firstRunning = time.Now()
		}
		// Fail fast on states that will never resolve on their own.
		for _, cs := range st.Status.ContainerStatuses {
			if w := cs.State.Waiting; w != nil {
				switch w.Reason {
				case "ImagePullBackOff", "ErrImagePull", "CreateContainerConfigError", "InvalidImageName":
					return Result{Duration: time.Since(started)},
						fmt.Errorf("%w: sandbox pod cannot start: %s: %s", ErrSetup, w.Reason, w.Message)
				}
			}
		}
		if st.Status.Phase != "Succeeded" && st.Status.Phase != "Failed" {
			continue
		}

		logs, _ := d.client.GetRaw(ctx, base+"/log?container=payload",
			int64(lim.MaxOutputBytes))
		res := Result{
			Stdout:   string(logs),
			Duration: time.Since(started),
		}
		if !firstRunning.IsZero() {
			res.ColdStartMS = firstRunning.Sub(started).Milliseconds()
		}
		if len(logs) >= lim.MaxOutputBytes {
			res.Truncated = true
			res.Stdout += "\n...[output truncated by the sandbox output cap]"
		}
		for _, cs := range st.Status.ContainerStatuses {
			if t := cs.State.Terminated; t != nil {
				res.ExitCode = int(t.ExitCode)
				res.OOMKilled = t.Reason == "OOMKilled"
				res.TimedOut = t.Reason == "DeadlineExceeded"
			}
		}
		if st.Status.Phase == "Failed" && res.ExitCode == 0 {
			res.ExitCode = 1
		}
		return res, nil
	}
}
