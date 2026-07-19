package kube

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

type Client struct {
	clientset kubernetes.Interface
}

type PodResizeCapability struct {
	Supported   bool
	Subresource string
	Verbs       []string
	Reason      string
}

func NewClient(kubeconfigPath, contextName string) (*Client, error) {
	restConfig, err := buildRESTConfig(kubeconfigPath, contextName)
	if err != nil {
		return nil, err
	}
	return NewClientForConfig(restConfig)
}

func NewClientForConfig(restConfig *rest.Config) (*Client, error) {
	if restConfig == nil {
		return nil, fmt.Errorf("kubernetes REST config is required")
	}
	restConfig = rest.CopyConfig(restConfig)
	restConfig.QPS = 50
	restConfig.Burst = 100
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}
	return &Client{clientset: clientset}, nil
}

func buildRESTConfig(kubeconfigPath, contextName string) (*rest.Config, error) {
	if kubeconfigPath != "" {
		loadingRules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: filepath.Clean(kubeconfigPath)}
		overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
		config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
		if err == nil {
			return config, nil
		}
	}
	config, err := rest.InClusterConfig()
	if err == nil {
		return config, nil
	}
	return nil, fmt.Errorf("build kubernetes config: %w", err)
}

func NewFakeClient(clientset kubernetes.Interface) *Client {
	return &Client{clientset: clientset}
}

func (c *Client) CurrentContext(kubeconfigPath string) string {
	if kubeconfigPath == "" {
		return ""
	}
	cfg, err := clientcmd.LoadFromFile(kubeconfigPath)
	if err != nil {
		return ""
	}
	return cfg.CurrentContext
}

func (c *Client) RawConfig(kubeconfigPath string) (*clientcmdapi.Config, error) {
	return clientcmd.LoadFromFile(kubeconfigPath)
}

func (c *Client) GetDeployment(ctx context.Context, namespace, name string) (*appsv1.Deployment, error) {
	return c.clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (c *Client) GetStatefulSet(ctx context.Context, namespace, name string) (*appsv1.StatefulSet, error) {
	return c.clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (c *Client) ListHPAs(ctx context.Context, namespace string) (*autoscalingv2.HorizontalPodAutoscalerList, error) {
	return c.clientset.AutoscalingV2().HorizontalPodAutoscalers(namespace).List(ctx, metav1.ListOptions{})
}

func (c *Client) ListPDBs(ctx context.Context, namespace string) (*policyv1.PodDisruptionBudgetList, error) {
	return c.clientset.PolicyV1().PodDisruptionBudgets(namespace).List(ctx, metav1.ListOptions{})
}

func (c *Client) ListServices(ctx context.Context, namespace string) (*corev1.ServiceList, error) {
	return c.clientset.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{})
}

func (c *Client) ListPods(ctx context.Context, namespace, selector string) (*corev1.PodList, error) {
	return c.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
}

func (c *Client) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	return c.clientset.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (c *Client) DeletePod(ctx context.Context, namespace, name, uid, resourceVersion string) error {
	options := metav1.DeleteOptions{}
	if uid != "" || resourceVersion != "" {
		options.Preconditions = &metav1.Preconditions{}
	}
	if uid != "" {
		podUID := types.UID(uid)
		options.Preconditions.UID = &podUID
	}
	if resourceVersion != "" {
		options.Preconditions.ResourceVersion = &resourceVersion
	}
	return c.clientset.CoreV1().Pods(namespace).Delete(ctx, name, options)
}

func (c *Client) PodResizeCapability() PodResizeCapability {
	resources, err := c.clientset.Discovery().ServerResourcesForGroupVersion("v1")
	if err != nil {
		return PodResizeCapability{Reason: fmt.Sprintf("discover v1 resources: %v", err)}
	}
	for _, resource := range resources.APIResources {
		if resource.Name != "pods/resize" {
			continue
		}
		verbs := append([]string(nil), resource.Verbs...)
		sort.Strings(verbs)
		capability := PodResizeCapability{
			Subresource: resource.Name,
			Verbs:       verbs,
		}
		if hasVerb(resource.Verbs, "get") && hasVerb(resource.Verbs, "patch") && hasVerb(resource.Verbs, "update") {
			capability.Supported = true
			return capability
		}
		capability.Reason = "pods/resize exists but required get, patch, and update verbs are not all advertised"
		return capability
	}
	return PodResizeCapability{Reason: "pods/resize subresource is not advertised"}
}

func hasVerb(verbs []string, want string) bool {
	for _, verb := range verbs {
		if verb == want {
			return true
		}
	}
	return false
}
