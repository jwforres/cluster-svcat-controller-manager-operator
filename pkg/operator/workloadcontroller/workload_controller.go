package workloadcontroller

import (
	"fmt"
	"time"

	"github.com/openshift/cluster-svcat-apiserver-operator/pkg/operator/operatorclient"

	"github.com/golang/glog"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/client-go/util/workqueue"
	apiregistrationv1client "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset/typed/apiregistration/v1"
	apiregistrationinformers "k8s.io/kube-aggregator/pkg/client/informers/externalversions"

	operatorsv1 "github.com/openshift/api/operator/v1"
	openshiftconfigclientv1 "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1"
	configinformers "github.com/openshift/client-go/config/informers/externalversions"
	operatorv1client "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions/operator/v1"
	clusteroperatorv1helpers "github.com/openshift/library-go/pkg/config/clusteroperator/v1helpers"
	"github.com/openshift/library-go/pkg/operator/events"
)

const (
	workloadFailingCondition = "WorkloadFailing"
	imageImportCAName        = "image-import-ca"
	workQueueKey             = "key"
)

type OpenShiftAPIServerOperator struct {
	targetImagePullSpec string

	operatorConfigClient    operatorv1client.OpenShiftAPIServersGetter
	openshiftConfigClient   openshiftconfigclientv1.ConfigV1Interface
	kubeClient              kubernetes.Interface
	apiregistrationv1Client apiregistrationv1client.ApiregistrationV1Interface
	eventRecorder           events.Recorder

	// queue only ever has one item, but it has nice error handling backoff/retry semantics
	queue workqueue.RateLimitingInterface

	rateLimiter flowcontrol.RateLimiter
}

func NewWorkloadController(
	targetImagePullSpec string,
	operatorConfigInformer operatorv1informers.OpenShiftAPIServerInformer,
	kubeInformersForOpenShiftAPIServerNamespace kubeinformers.SharedInformerFactory,
	kubeInformersForEtcdNamespace kubeinformers.SharedInformerFactory,
	kubeInformersForKubeAPIServerNamespace kubeinformers.SharedInformerFactory,
	kubeInformersForOpenShiftConfigNamespace kubeinformers.SharedInformerFactory,
	apiregistrationInformers apiregistrationinformers.SharedInformerFactory,
	configInformers configinformers.SharedInformerFactory,
	operatorConfigClient operatorv1client.OpenShiftAPIServersGetter,
	openshiftConfigClient openshiftconfigclientv1.ConfigV1Interface,
	kubeClient kubernetes.Interface,
	apiregistrationv1Client apiregistrationv1client.ApiregistrationV1Interface,
	eventRecorder events.Recorder,
) *OpenShiftAPIServerOperator {
	c := &OpenShiftAPIServerOperator{
		targetImagePullSpec:     targetImagePullSpec,
		operatorConfigClient:    operatorConfigClient,
		openshiftConfigClient:   openshiftConfigClient,
		kubeClient:              kubeClient,
		apiregistrationv1Client: apiregistrationv1Client,
		eventRecorder:           eventRecorder,

		queue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "OpenShiftAPIServerOperator"),

		rateLimiter: flowcontrol.NewTokenBucketRateLimiter(0.05 /*3 per minute*/, 4),
	}

	operatorConfigInformer.Informer().AddEventHandler(c.eventHandler())
	kubeInformersForEtcdNamespace.Core().V1().ConfigMaps().Informer().AddEventHandler(c.eventHandler())
	kubeInformersForEtcdNamespace.Core().V1().Secrets().Informer().AddEventHandler(c.eventHandler())
	kubeInformersForKubeAPIServerNamespace.Core().V1().ConfigMaps().Informer().AddEventHandler(c.eventHandler())
	kubeInformersForOpenShiftAPIServerNamespace.Core().V1().ConfigMaps().Informer().AddEventHandler(c.eventHandler())
	kubeInformersForOpenShiftAPIServerNamespace.Core().V1().ServiceAccounts().Informer().AddEventHandler(c.eventHandler())
	kubeInformersForOpenShiftAPIServerNamespace.Core().V1().Services().Informer().AddEventHandler(c.eventHandler())
	kubeInformersForOpenShiftAPIServerNamespace.Apps().V1().DaemonSets().Informer().AddEventHandler(c.eventHandler())
	kubeInformersForOpenShiftConfigNamespace.Core().V1().ConfigMaps().Informer().AddEventHandler(c.eventHandler())
	configInformers.Config().V1().Images().Informer().AddEventHandler(c.eventHandler())
	apiregistrationInformers.Apiregistration().V1().APIServices().Informer().AddEventHandler(c.eventHandler())

	// we only watch some namespaces
	kubeInformersForOpenShiftAPIServerNamespace.Core().V1().Namespaces().Informer().AddEventHandler(c.namespaceEventHandler())

	return c
}

func (c OpenShiftAPIServerOperator) sync() error {
	operatorConfig, err := c.operatorConfigClient.OpenShiftAPIServers().Get("svcat", metav1.GetOptions{})
	if err != nil {
		return err
	}
	switch operatorConfig.Spec.ManagementState {
	case operatorsv1.Unmanaged:
		return nil

	case operatorsv1.Removed:
		// TODO probably need to watch until the NS is really gone
		if err := c.kubeClient.CoreV1().Namespaces().Delete(operatorclient.TargetNamespaceName, nil); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	}

	kubeAPIServerOperator, err := c.openshiftConfigClient.ClusterOperators().Get("kube-apiserver", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		kubeAPIServerOperator, err = c.openshiftConfigClient.ClusterOperators().Get("openshift-kube-apiserver-operator", metav1.GetOptions{})
	}
	if apierrors.IsNotFound(err) {
		message := "clusteroperator/kube-apiserver not found"
		c.eventRecorder.Warning("PrereqNotReady", message)
		return fmt.Errorf(message)
	}
	if err != nil {
		return err
	}
	if !clusteroperatorv1helpers.IsStatusConditionTrue(kubeAPIServerOperator.Status.Conditions, "Available") {
		message := fmt.Sprintf("clusteroperator/%s is not Available", kubeAPIServerOperator.Name)
		c.eventRecorder.Warning("PrereqNotReady", message)
		return fmt.Errorf(message)
	}

	// block until config is obvserved
	if len(operatorConfig.Spec.ObservedConfig.Raw) == 0 {
		glog.Info("Waiting for observed configuration to be available")
		return nil
	}

	forceRequeue, err := syncOpenShiftAPIServer_v311_00_to_latest(c, operatorConfig)
	if forceRequeue && err != nil {
		c.queue.AddRateLimited(workQueueKey)
	}

	return err
}

// Run starts the openshift-apiserver-operator and blocks until stopCh is closed.
func (c *OpenShiftAPIServerOperator) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	glog.Infof("Starting OpenShiftAPIServerOperator")
	defer glog.Infof("Shutting down OpenShiftAPIServerOperator")

	// doesn't matter what workers say, only start one.
	go wait.Until(c.runWorker, time.Second, stopCh)

	<-stopCh
}

func (c *OpenShiftAPIServerOperator) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *OpenShiftAPIServerOperator) processNextWorkItem() bool {
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
func (c *OpenShiftAPIServerOperator) eventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.queue.Add(workQueueKey) },
		UpdateFunc: func(old, new interface{}) { c.queue.Add(workQueueKey) },
		DeleteFunc: func(obj interface{}) { c.queue.Add(workQueueKey) },
	}
}

// this set of namespaces will include things like logging and metrics which are used to drive
var interestingNamespaces = sets.NewString(operatorclient.TargetNamespaceName)

func (c *OpenShiftAPIServerOperator) namespaceEventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ns, ok := obj.(*corev1.Namespace)
			if !ok {
				c.queue.Add(workQueueKey)
			}
			if ns.Name == operatorclient.TargetNamespaceName {
				c.queue.Add(workQueueKey)
			}
		},
		UpdateFunc: func(old, new interface{}) {
			ns, ok := old.(*corev1.Namespace)
			if !ok {
				c.queue.Add(workQueueKey)
			}
			if ns.Name == operatorclient.TargetNamespaceName {
				c.queue.Add(workQueueKey)
			}
		},
		DeleteFunc: func(obj interface{}) {
			ns, ok := obj.(*corev1.Namespace)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
					return
				}
				ns, ok = tombstone.Obj.(*corev1.Namespace)
				if !ok {
					utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a Namespace %#v", obj))
					return
				}
			}
			if ns.Name == operatorclient.TargetNamespaceName {
				c.queue.Add(workQueueKey)
			}
		},
	}
}
