package benchmark

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	variantautoscalingv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

var (
	benchCfg       BenchmarkConfig
	k8sClient      *kubernetes.Clientset
	crClient       client.Client
	restConfig     *rest.Config
	ctx            context.Context
	cancel         context.CancelFunc
	promClient     *utils.PrometheusClient
	portForwardCmd *exec.Cmd
	grafanaClient  *GrafanaClient
)

func TestBenchmark(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Benchmark Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	By("Loading benchmark configuration from environment")
	benchCfg = LoadConfigFromEnv()

	GinkgoWriter.Printf("=== Benchmark Configuration ===\n")
	GinkgoWriter.Printf("Environment: %s\n", benchCfg.Environment)
	GinkgoWriter.Printf("WVA Namespace: %s\n", benchCfg.WVANamespace)
	GinkgoWriter.Printf("LLMD Namespace: %s\n", benchCfg.LLMDNamespace)
	GinkgoWriter.Printf("Results File: %s\n", benchCfg.BenchmarkResultsFile)
	GinkgoWriter.Printf("===============================\n\n")

	By("Initializing Kubernetes client")
	var err error
	if _, statErr := os.Stat(benchCfg.Kubeconfig); statErr == nil {
		restConfig, err = clientcmd.BuildConfigFromFlags("", benchCfg.Kubeconfig)
		Expect(err).NotTo(HaveOccurred(), "Failed to load kubeconfig")
	} else {
		restConfig, err = rest.InClusterConfig()
		Expect(err).NotTo(HaveOccurred(), "Failed to load in-cluster config")
	}

	k8sClient, err = kubernetes.NewForConfig(restConfig)
	Expect(err).NotTo(HaveOccurred(), "Failed to create Kubernetes clientset")

	s := runtime.NewScheme()
	err = clientgoscheme.AddToScheme(s)
	Expect(err).NotTo(HaveOccurred())
	err = variantautoscalingv1alpha1.AddToScheme(s)
	Expect(err).NotTo(HaveOccurred())
	err = promoperator.AddToScheme(s)
	Expect(err).NotTo(HaveOccurred())

	crClient, err = client.New(restConfig, client.Options{Scheme: s})
	Expect(err).NotTo(HaveOccurred(), "Failed to create controller-runtime client")

	ctx, cancel = context.WithCancel(context.Background())

	By("Verifying WVA controller is running")
	Eventually(func(g Gomega) {
		pods, err := k8sClient.CoreV1().Pods(benchCfg.WVANamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "control-plane=controller-manager",
		})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(pods.Items).NotTo(BeEmpty(), "WVA controller pod not found")

		runningPods := 0
		for _, pod := range pods.Items {
			if pod.Status.Phase == corev1.PodRunning {
				runningPods++
			}
		}
		g.Expect(runningPods).To(BeNumerically(">", 0), "No running WVA controller pods")
	}, 2*time.Minute, 5*time.Second).Should(Succeed(), "WVA controller should be running")

	By("Verifying Prometheus is available")
	Eventually(func(g Gomega) {
		pods, err := k8sClient.CoreV1().Pods(benchCfg.MonitoringNS).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/name=prometheus",
		})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(pods.Items).NotTo(BeEmpty(), "Prometheus pod not found")
	}, 2*time.Minute, 5*time.Second).Should(Succeed(), "Prometheus should be running")

	By("Setting up port-forward to Prometheus")
	portForwardCmd = utils.SetUpPortForward(k8sClient, ctx, "kube-prometheus-stack-prometheus", benchCfg.MonitoringNS, 9090, 9090)

	By("Verifying Prometheus port-forward is ready")
	err = utils.VerifyPortForwardReadiness(ctx, 9090, "https://localhost:9090/-/ready")
	Expect(err).NotTo(HaveOccurred(), "Prometheus port-forward should be ready")

	By("Creating Prometheus client")
	promClient, err = utils.NewPrometheusClient("https://localhost:9090", true)
	Expect(err).NotTo(HaveOccurred(), "Failed to create Prometheus client")

	if benchCfg.GrafanaEnabled {
		By("Deploying ephemeral Grafana for benchmark snapshots")
		if grafanaErr := DeployGrafana(ctx, k8sClient, benchCfg.MonitoringNS); grafanaErr != nil {
			GinkgoWriter.Printf("WARNING: Grafana deployment failed (non-fatal): %v\n", grafanaErr)
		} else {
			By("Setting up Grafana client with port-forward")
			var clientErr error
			grafanaClient, clientErr = NewGrafanaClient(k8sClient, ctx, benchCfg.MonitoringNS)
			if clientErr != nil {
				GinkgoWriter.Printf("WARNING: Grafana client setup failed (non-fatal): %v\n", clientErr)
				grafanaClient = nil
			} else {
				GinkgoWriter.Println("Grafana deployed and ready for snapshot capture")
			}
		}
	}

	GinkgoWriter.Println("BeforeSuite completed — infrastructure ready for benchmarks")
})

var _ = AfterSuite(func() {
	if grafanaClient != nil {
		By("Closing Grafana port-forward")
		grafanaClient.Close()
	}

	if portForwardCmd != nil && portForwardCmd.Process != nil {
		By("Killing Prometheus port-forward")
		_ = portForwardCmd.Process.Kill()
	}

	if cancel != nil {
		cancel()
	}
})
