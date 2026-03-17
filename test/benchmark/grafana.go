package benchmark

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

const (
	grafanaLocalPort = 3000
	grafanaSvcName   = "kube-prometheus-stack-grafana"
	dashboardUID     = "wva-benchmark-scaleup"
)

// GrafanaClient manages an ephemeral Grafana instance for benchmark snapshots.
type GrafanaClient struct {
	baseURL        string
	httpClient     *http.Client
	portForwardCmd *exec.Cmd
}

// DeployGrafana enables Grafana on the existing kube-prometheus-stack via helm upgrade,
// creates the dashboard ConfigMap, and waits for Grafana to be ready.
func DeployGrafana(ctx context.Context, k8sClient *kubernetes.Clientset, monitoringNS string) error {
	projectDir, err := utils.GetProjectDir()
	if err != nil {
		return fmt.Errorf("get project dir: %w", err)
	}

	valuesFile := filepath.Join(projectDir, "deploy", "grafana", "benchmark-grafana-values.yaml")
	dashboardFile := filepath.Join(projectDir, "deploy", "grafana", "benchmark-dashboard.json")

	// Create the dashboard ConfigMap with the sidecar label so Grafana auto-imports it
	cmCmd := exec.CommandContext(ctx, "kubectl", "create", "configmap", "benchmark-dashboard",
		"--from-file=benchmark-dashboard.json="+dashboardFile,
		"-n", monitoringNS,
		"--dry-run=client", "-o", "yaml")
	cmYaml, err := cmCmd.Output()
	if err != nil {
		return fmt.Errorf("generate dashboard configmap yaml: %w", err)
	}

	// Pipe through kubectl apply with label
	applyCmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", "-")
	applyCmd.Stdin = bytes.NewReader(cmYaml)
	if out, err := applyCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("apply dashboard configmap: %s: %w", string(out), err)
	}

	// Label the ConfigMap for the Grafana sidecar
	labelCmd := exec.CommandContext(ctx, "kubectl", "label", "configmap", "benchmark-dashboard",
		"grafana_dashboard=benchmark",
		"-n", monitoringNS,
		"--overwrite")
	if out, err := labelCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("label dashboard configmap: %s: %w", string(out), err)
	}

	// Helm upgrade to enable Grafana
	helmCmd := exec.CommandContext(ctx, "helm", "upgrade", "kube-prometheus-stack",
		"prometheus-community/kube-prometheus-stack",
		"-n", monitoringNS,
		"-f", valuesFile,
		"--reuse-values",
		"--timeout=5m",
		"--wait",
	)
	if out, err := helmCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("helm upgrade to enable grafana: %s: %w", string(out), err)
	}

	// Wait for Grafana pod to be ready
	err = wait.PollUntilContextTimeout(ctx, 5*time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		pods, listErr := k8sClient.CoreV1().Pods(monitoringNS).List(ctx, labelSelector("app.kubernetes.io/name=grafana"))
		if listErr != nil || len(pods.Items) == 0 {
			return false, nil
		}
		for _, pod := range pods.Items {
			for _, cs := range pod.Status.ContainerStatuses {
				if cs.Ready {
					return true, nil
				}
			}
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("waiting for grafana pod: %w", err)
	}

	return nil
}

// NewGrafanaClient sets up port-forward to Grafana and returns a client.
func NewGrafanaClient(k8sClient *kubernetes.Clientset, ctx context.Context, monitoringNS string) (*GrafanaClient, error) {
	pfCmd := utils.SetUpPortForward(k8sClient, ctx, grafanaSvcName, monitoringNS, grafanaLocalPort, 80)

	baseURL := fmt.Sprintf("http://localhost:%d", grafanaLocalPort)

	// Wait for Grafana to respond
	err := wait.PollUntilContextTimeout(ctx, 1*time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		resp, httpErr := http.Get(baseURL + "/api/health")
		if httpErr != nil {
			return false, nil
		}
		defer resp.Body.Close()
		return resp.StatusCode == 200, nil
	})
	if err != nil {
		if pfCmd.Process != nil {
			_ = pfCmd.Process.Kill()
		}
		return nil, fmt.Errorf("grafana not ready: %w", err)
	}

	return &GrafanaClient{
		baseURL:        baseURL,
		portForwardCmd: pfCmd,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
	}, nil
}

// Close kills the port-forward process.
func (g *GrafanaClient) Close() {
	if g.portForwardCmd != nil && g.portForwardCmd.Process != nil {
		_ = g.portForwardCmd.Process.Kill()
	}
}

// snapshotRequest is the payload for Grafana's POST /api/snapshots endpoint.
type snapshotRequest struct {
	Dashboard json.RawMessage `json:"dashboard"`
	Name      string          `json:"name"`
	Expires   int             `json:"expires"`
}

// snapshotResponse is the response from Grafana's POST /api/snapshots.
type snapshotResponse struct {
	Key     string `json:"key"`
	URL     string `json:"url"`
	DeleteKey string `json:"deleteKey"`
	DeleteURL string `json:"deleteUrl"`
}

// CreateSnapshot creates a Grafana snapshot of the benchmark dashboard
// covering the time range from scenarioStart to now.
// Returns the local snapshot URL.
func (g *GrafanaClient) CreateSnapshot(scenarioStart time.Time) (string, error) {
	// Fetch the dashboard model from Grafana
	dashURL := fmt.Sprintf("%s/api/dashboards/uid/%s", g.baseURL, dashboardUID)
	resp, err := g.httpClient.Get(dashURL)
	if err != nil {
		return "", fmt.Errorf("fetch dashboard: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("fetch dashboard returned %d: %s", resp.StatusCode, string(body))
	}

	var dashResp struct {
		Dashboard json.RawMessage `json:"dashboard"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&dashResp); err != nil {
		return "", fmt.Errorf("decode dashboard: %w", err)
	}

	// Modify the time range in the dashboard to cover the benchmark period
	var dashModel map[string]interface{}
	if err := json.Unmarshal(dashResp.Dashboard, &dashModel); err != nil {
		return "", fmt.Errorf("unmarshal dashboard model: %w", err)
	}
	dashModel["time"] = map[string]string{
		"from": scenarioStart.UTC().Format(time.RFC3339),
		"to":   time.Now().UTC().Format(time.RFC3339),
	}
	modifiedDash, err := json.Marshal(dashModel)
	if err != nil {
		return "", fmt.Errorf("marshal modified dashboard: %w", err)
	}

	// Create snapshot
	snapReq := snapshotRequest{
		Dashboard: modifiedDash,
		Name:      fmt.Sprintf("Benchmark %s", time.Now().UTC().Format("2006-01-02T15:04:05Z")),
		Expires:   0, // ephemeral — dies with the cluster anyway
	}
	payload, err := json.Marshal(snapReq)
	if err != nil {
		return "", fmt.Errorf("marshal snapshot request: %w", err)
	}

	snapResp, err := g.httpClient.Post(g.baseURL+"/api/snapshots", "application/json", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("create snapshot: %w", err)
	}
	defer snapResp.Body.Close()

	if snapResp.StatusCode != 200 {
		body, _ := io.ReadAll(snapResp.Body)
		return "", fmt.Errorf("create snapshot returned %d: %s", snapResp.StatusCode, string(body))
	}

	var result snapshotResponse
	if err := json.NewDecoder(snapResp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode snapshot response: %w", err)
	}

	return result.URL, nil
}

// RenderPanel renders a single Grafana panel to PNG and saves it to outputPath.
// panelID is the numeric panel ID from the dashboard JSON.
// width and height are in pixels.
func (g *GrafanaClient) RenderPanel(panelID int, from, to time.Time, width, height int, outputPath string) error {
	url := fmt.Sprintf("%s/render/d-solo/%s/benchmark?orgId=1&panelId=%d&from=%d&to=%d&width=%d&height=%d",
		g.baseURL, dashboardUID, panelID,
		from.UnixMilli(), to.UnixMilli(),
		width, height,
	)

	resp, err := g.httpClient.Get(url)
	if err != nil {
		return fmt.Errorf("render panel %d: %w", panelID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("render panel %d returned %d: %s", panelID, resp.StatusCode, string(body))
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read render response: %w", err)
	}

	return os.WriteFile(outputPath, data, 0644)
}

// helper to build a ListOptions with a label selector
func labelSelector(sel string) metav1.ListOptions {
	return metav1.ListOptions{LabelSelector: sel}
}
