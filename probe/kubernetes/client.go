package kubernetes

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/weaveworks/common/backoff"

	log "github.com/Sirupsen/logrus"
	apiappsv1beta1 "k8s.io/api/apps/v1beta1"
	apibatchv1 "k8s.io/api/batch/v1"
	apibatchv1beta1 "k8s.io/api/batch/v1beta1"
	apibatchv2alpha1 "k8s.io/api/batch/v2alpha1"
	apiv1 "k8s.io/api/core/v1"
	apiextensionsv1beta1 "k8s.io/api/extensions/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// Client keeps track of running kubernetes pods and services
type Client interface {
	Stop()
	WalkPods(f func(Pod) error) error
	WalkServices(f func(Service) error) error
	WalkDeployments(f func(Deployment) error) error
	WalkDaemonSets(f func(DaemonSet) error) error
	WalkStatefulSets(f func(StatefulSet) error) error
	WalkCronJobs(f func(CronJob) error) error
	WalkNamespaces(f func(NamespaceResource) error) error

	WatchPods(f func(Event, Pod))

	GetLogs(namespaceID, podID string, containerNames []string) (io.ReadCloser, error)
	DeletePod(namespaceID, podID string) error
	ScaleUp(resource, namespaceID, id string) error
	ScaleDown(resource, namespaceID, id string) error
}

type client struct {
	quit             chan struct{}
	client           *kubernetes.Clientset
	podStore         cache.Store
	serviceStore     cache.Store
	deploymentStore  cache.Store
	daemonSetStore   cache.Store
	statefulSetStore cache.Store
	jobStore         cache.Store
	cronJobStore     cache.Store
	nodeStore        cache.Store
	namespaceStore   cache.Store

	podWatchesMutex sync.Mutex
	podWatches      []func(Event, Pod)
}

// ClientConfig establishes the configuration for the kubernetes client
type ClientConfig struct {
	CertificateAuthority string
	ClientCertificate    string
	ClientKey            string
	Cluster              string
	Context              string
	Insecure             bool
	Kubeconfig           string
	Password             string
	Server               string
	Token                string
	User                 string
	Username             string
}

// NewClient returns a usable Client. Don't forget to Stop it.
func NewClient(config ClientConfig) (Client, error) {
	var restConfig *rest.Config
	if config.Server == "" && config.Kubeconfig == "" {
		// If no API server address or kubeconfig was provided, assume we are running
		// inside a pod. Try to connect to the API server through its
		// Service environment variables, using the default Service
		// Account Token.
		var err error
		if restConfig, err = rest.InClusterConfig(); err != nil {
			return nil, err
		}
	} else {
		var err error
		restConfig, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			&clientcmd.ClientConfigLoadingRules{ExplicitPath: config.Kubeconfig},
			&clientcmd.ConfigOverrides{
				AuthInfo: clientcmdapi.AuthInfo{
					ClientCertificate: config.ClientCertificate,
					ClientKey:         config.ClientKey,
					Token:             config.Token,
					Username:          config.Username,
					Password:          config.Password,
				},
				ClusterInfo: clientcmdapi.Cluster{
					Server:                config.Server,
					InsecureSkipTLSVerify: config.Insecure,
					CertificateAuthority:  config.CertificateAuthority,
				},
				Context: clientcmdapi.Context{
					Cluster:  config.Cluster,
					AuthInfo: config.User,
				},
				CurrentContext: config.Context,
			},
		).ClientConfig()
		if err != nil {
			return nil, err
		}
	}
	log.Infof("kubernetes: targeting api server %s", restConfig.Host)

	c, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}

	result := &client{
		quit:   make(chan struct{}),
		client: c,
	}

	result.podStore = NewEventStore(result.triggerPodWatches, cache.MetaNamespaceKeyFunc)
	result.runReflectorUntil("pods", result.podStore)

	result.serviceStore = result.setupStore("services")
	result.nodeStore = result.setupStore("nodes")
	result.namespaceStore = result.setupStore("namespaces")
	result.deploymentStore = result.setupStore("deployments")
	result.daemonSetStore = result.setupStore("daemonsets")
	result.jobStore = result.setupStore("jobs")
	result.statefulSetStore = result.setupStore("statefulsets")
	result.cronJobStore = result.setupStore("cronjobs")

	return result, nil
}

func (c *client) isResourceSupported(groupVersion schema.GroupVersion, resource string) (bool, error) {
	resourceList, err := c.client.Discovery().ServerResourcesForGroupVersion(groupVersion.String())
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	for _, v := range resourceList.APIResources {
		if v.Name == resource {
			return true, nil
		}
	}

	return false, nil
}

func (c *client) setupStore(resource string) cache.Store {
	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	c.runReflectorUntil(resource, store)
	return store
}

func (c *client) clientAndType(resource string) (rest.Interface, interface{}, error) {
	switch resource {
	case "pods":
		return c.client.CoreV1().RESTClient(), &apiv1.Pod{}, nil
	case "services":
		return c.client.CoreV1().RESTClient(), &apiv1.Service{}, nil
	case "nodes":
		return c.client.CoreV1().RESTClient(), &apiv1.Node{}, nil
	case "namespaces":
		return c.client.CoreV1().RESTClient(), &apiv1.Namespace{}, nil
	case "deployments":
		return c.client.ExtensionsV1beta1().RESTClient(), &apiextensionsv1beta1.Deployment{}, nil
	case "daemonsets":
		return c.client.ExtensionsV1beta1().RESTClient(), &apiextensionsv1beta1.DaemonSet{}, nil
	case "jobs":
		return c.client.BatchV1().RESTClient(), &apibatchv1.Job{}, nil
	case "statefulsets":
		return c.client.AppsV1beta1().RESTClient(), &apiappsv1beta1.StatefulSet{}, nil
	case "cronjobs":
		ok, err := c.isResourceSupported(c.client.BatchV1beta1().RESTClient().APIVersion(), resource)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			// kubernetes >= 1.8
			return c.client.BatchV1beta1().RESTClient(), &apibatchv1beta1.CronJob{}, nil
		}
		// kubernetes < 1.8
		return c.client.BatchV2alpha1().RESTClient(), &apibatchv2alpha1.CronJob{}, nil
	}
	return nil, nil, fmt.Errorf("Invalid resource: %v", resource)
}

// runReflectorUntil runs cache.Reflector#ListAndWatch in an endless loop, after checking that the resource is supported by kubernetes.
// Errors are logged and retried with exponential backoff.
func (c *client) runReflectorUntil(resource string, store cache.Store) {
	var r *cache.Reflector
	listAndWatch := func() (bool, error) {
		if r == nil {
			kclient, itemType, err := c.clientAndType(resource)
			if err != nil {
				return false, err
			}
			ok, err := c.isResourceSupported(kclient.APIVersion(), resource)
			if err != nil {
				return false, err
			}
			if !ok {
				log.Infof("%v are not supported by this Kubernetes version", resource)
				return true, nil
			}
			lw := cache.NewListWatchFromClient(kclient, resource, metav1.NamespaceAll, fields.Everything())
			r = cache.NewReflector(lw, itemType, store, 0)
		}

		select {
		case <-c.quit:
			return true, nil
		default:
			err := r.ListAndWatch(c.quit)
			return false, err
		}
	}
	bo := backoff.New(listAndWatch, fmt.Sprintf("Kubernetes reflector (%s)", resource))
	bo.SetMaxBackoff(5 * time.Minute)
	go bo.Start()
}

func (c *client) WatchPods(f func(Event, Pod)) {
	c.podWatchesMutex.Lock()
	defer c.podWatchesMutex.Unlock()
	c.podWatches = append(c.podWatches, f)
}

func (c *client) triggerPodWatches(e Event, pod interface{}) {
	c.podWatchesMutex.Lock()
	defer c.podWatchesMutex.Unlock()
	for _, watch := range c.podWatches {
		watch(e, NewPod(pod.(*apiv1.Pod)))
	}
}

func (c *client) WalkPods(f func(Pod) error) error {
	for _, m := range c.podStore.List() {
		pod := m.(*apiv1.Pod)
		if err := f(NewPod(pod)); err != nil {
			return err
		}
	}
	return nil
}

func (c *client) WalkServices(f func(Service) error) error {
	for _, m := range c.serviceStore.List() {
		s := m.(*apiv1.Service)
		if err := f(NewService(s)); err != nil {
			return err
		}
	}
	return nil
}

func (c *client) WalkDeployments(f func(Deployment) error) error {
	if c.deploymentStore == nil {
		return nil
	}
	for _, m := range c.deploymentStore.List() {
		d := m.(*apiextensionsv1beta1.Deployment)
		if err := f(NewDeployment(d)); err != nil {
			return err
		}
	}
	return nil
}

// WalkDaemonSets calls f for each daemonset
func (c *client) WalkDaemonSets(f func(DaemonSet) error) error {
	if c.daemonSetStore == nil {
		return nil
	}
	for _, m := range c.daemonSetStore.List() {
		ds := m.(*apiextensionsv1beta1.DaemonSet)
		if err := f(NewDaemonSet(ds)); err != nil {
			return err
		}
	}
	return nil
}

// WalkStatefulSets calls f for each statefulset
func (c *client) WalkStatefulSets(f func(StatefulSet) error) error {
	if c.statefulSetStore == nil {
		return nil
	}
	for _, m := range c.statefulSetStore.List() {
		s := m.(*apiappsv1beta1.StatefulSet)
		if err := f(NewStatefulSet(s)); err != nil {
			return err
		}
	}
	return nil
}

// WalkCronJobs calls f for each cronjob
func (c *client) WalkCronJobs(f func(CronJob) error) error {
	if c.cronJobStore == nil {
		return nil
	}
	// We index jobs by id to make lookup for each cronjob more efficient
	jobs := map[types.UID]*apibatchv1.Job{}
	for _, m := range c.jobStore.List() {
		j := m.(*apibatchv1.Job)
		jobs[j.UID] = j
	}
	for _, m := range c.cronJobStore.List() {
		if err := f(NewCronJob(m, jobs)); err != nil {
			return err
		}
	}
	return nil
}

func (c *client) WalkNamespaces(f func(NamespaceResource) error) error {
	for _, m := range c.namespaceStore.List() {
		namespace := m.(*apiv1.Namespace)
		if err := f(NewNamespace(namespace)); err != nil {
			return err
		}
	}
	return nil
}

func (c *client) GetLogs(namespaceID, podID string, containerNames []string) (io.ReadCloser, error) {
	readClosersWithLabel := map[io.ReadCloser]string{}
	for _, container := range containerNames {
		req := c.client.CoreV1().Pods(namespaceID).GetLogs(
			podID,
			&apiv1.PodLogOptions{
				Follow:     true,
				Timestamps: true,
				Container:  container,
			},
		)
		readCloser, err := req.Stream()
		if err != nil {
			for rc := range readClosersWithLabel {
				rc.Close()
			}
			return nil, err
		}
		readClosersWithLabel[readCloser] = container
	}

	return NewLogReadCloser(readClosersWithLabel), nil
}

func (c *client) DeletePod(namespaceID, podID string) error {
	return c.client.CoreV1().Pods(namespaceID).Delete(podID, &metav1.DeleteOptions{})
}

func (c *client) ScaleUp(resource, namespaceID, id string) error {
	return c.modifyScale(resource, namespaceID, id, func(scale *apiextensionsv1beta1.Scale) {
		scale.Spec.Replicas++
	})
}

func (c *client) ScaleDown(resource, namespaceID, id string) error {
	return c.modifyScale(resource, namespaceID, id, func(scale *apiextensionsv1beta1.Scale) {
		scale.Spec.Replicas--
	})
}

func (c *client) modifyScale(resource, namespace, id string, f func(*apiextensionsv1beta1.Scale)) error {
	scaler := c.client.Extensions().Scales(namespace)
	scale, err := scaler.Get(resource, id)
	if err != nil {
		return err
	}
	f(scale)
	_, err = scaler.Update(resource, scale)
	return err
}

func (c *client) Stop() {
	close(c.quit)
}
