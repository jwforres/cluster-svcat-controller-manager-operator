package operator

import (
	"fmt"
	"reflect"
	"time"

	"github.com/openshift/cluster-svcat-apiserver-operator/pkg/operator/operatorclient"

	"github.com/golang/glog"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	appsv1lister "k8s.io/client-go/listers/apps/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/client-go/util/workqueue"

	"github.com/openshift/library-go/pkg/operator/events"
)

type finalizerController struct {
	kubeClient    kubernetes.Interface
	podLister     corev1listers.PodLister
	dsLister      appsv1lister.DaemonSetLister
	eventRecorder events.Recorder

	preRunHasSynced []cache.InformerSynced

	// queue only ever has one item, but it has nice error handling backoff/retry semantics
	queue workqueue.RateLimitingInterface

	rateLimiter flowcontrol.RateLimiter
}

// NewFinalizerController is here because
// When running an aggregated API on the platform, you delete the namespace hosting the aggregated API. Doing that the
// namespace controller starts by doing complete discovery and then deleting all objects, but pods have a grace period,
// so it deletes the rest and requeues. The ns controller starts again and does a complete discovery and.... fails. The
// failure means it refuses to complete the cleanup. Now, we don't actually want to delete the resoruces from our
// aggregated API, only the server plus config if we remove the apiservices to unstick it, GC will start cleaning
// everything. For now, we can unbork 4.0, but clearing the finalizer after the pod and daemonset we created are gone.
func NewFinalizerController(
	kubeInformersForTargetNamespace kubeinformers.SharedInformerFactory,
	kubeClient kubernetes.Interface,
	eventRecorder events.Recorder,
) *finalizerController {
	c := &finalizerController{
		kubeClient:    kubeClient,
		podLister:     kubeInformersForTargetNamespace.Core().V1().Pods().Lister(),
		dsLister:      kubeInformersForTargetNamespace.Apps().V1().DaemonSets().Lister(),
		eventRecorder: eventRecorder,

		preRunHasSynced: []cache.InformerSynced{
			kubeInformersForTargetNamespace.Core().V1().Pods().Informer().HasSynced,
			kubeInformersForTargetNamespace.Apps().V1().DaemonSets().Informer().HasSynced,
		},
		queue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "FinalizerController"),

		rateLimiter: flowcontrol.NewTokenBucketRateLimiter(0.05 /*3 per minute*/, 4),
	}

	kubeInformersForTargetNamespace.Core().V1().Pods().Informer().AddEventHandler(c.eventHandler())
	kubeInformersForTargetNamespace.Apps().V1().DaemonSets().Informer().AddEventHandler(c.eventHandler())

	return c
}

func (c finalizerController) sync() error {
	ns, err := c.kubeClient.CoreV1().Namespaces().Get(operatorclient.TargetNamespaceName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if ns.DeletionTimestamp == nil {
		return nil
	}

	pods, err := c.podLister.Pods(operatorclient.TargetNamespaceName).List(labels.Everything())
	if err != nil {
		return err
	}
	if len(pods) > 0 {
		return nil
	}
	dses, err := c.dsLister.DaemonSets(operatorclient.TargetNamespaceName).List(labels.Everything())
	if err != nil {
		return err
	}
	if len(dses) > 0 {
		return nil
	}

	newFinalizers := []corev1.FinalizerName{}
	for _, curr := range ns.Spec.Finalizers {
		if curr == corev1.FinalizerKubernetes {
			continue
		}
		newFinalizers = append(newFinalizers, curr)
	}
	if reflect.DeepEqual(newFinalizers, ns.Spec.Finalizers) {
		return nil
	}
	ns.Spec.Finalizers = newFinalizers

	c.eventRecorder.Event("NamespaceFinalization", fmt.Sprintf("clearing namespace finalizer on %q", operatorclient.TargetNamespaceName))
	_, err = c.kubeClient.CoreV1().Namespaces().Finalize(ns)
	return err
}

// Run starts the server and blocks until stopCh is closed.
func (c *finalizerController) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	glog.Infof("Starting FinalizerController")
	defer glog.Infof("Shutting down FinalizerController")

	if !cache.WaitForCacheSync(stopCh, c.preRunHasSynced...) {
		utilruntime.HandleError(fmt.Errorf("caches did not sync"))
		return
	}

	// always kick at least once in case we started after the namespace was cleared
	c.queue.Add(operatorclient.TargetNamespaceName)

	// doesn't matter what workers say, only start one.
	go wait.Until(c.runWorker, time.Second, stopCh)

	<-stopCh
}

func (c *finalizerController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *finalizerController) processNextWorkItem() bool {
	dsKey, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(dsKey)

	// before we call sync, we want to wait for token.  We do this to avoid hot looping.
	c.rateLimiter.Accept()

	err := c.sync()
	if err == nil {
		c.queue.Forget(dsKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", dsKey, err))
	c.queue.AddRateLimited(dsKey)

	return true
}

// eventHandler queues the operator to check spec and status
func (c *finalizerController) eventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.queue.Add(operatorclient.TargetNamespaceName) },
		UpdateFunc: func(old, new interface{}) { c.queue.Add(operatorclient.TargetNamespaceName) },
		DeleteFunc: func(obj interface{}) { c.queue.Add(operatorclient.TargetNamespaceName) },
	}
}
