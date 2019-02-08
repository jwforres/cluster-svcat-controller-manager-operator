// Code generated by informer-gen. DO NOT EDIT.

package v1

import (
	internalinterfaces "github.com/openshift/client-go/operator/informers/externalversions/internalinterfaces"
)

// Interface provides access to all the informers in this group version.
type Interface interface {
	// KubeAPIServers returns a KubeAPIServerInformer.
	KubeAPIServers() KubeAPIServerInformer
	// KubeControllerManagers returns a KubeControllerManagerInformer.
	KubeControllerManagers() KubeControllerManagerInformer
	// OpenShiftAPIServers returns a OpenShiftAPIServerInformer.
	OpenShiftAPIServers() OpenShiftAPIServerInformer
}

type version struct {
	factory          internalinterfaces.SharedInformerFactory
	namespace        string
	tweakListOptions internalinterfaces.TweakListOptionsFunc
}

// New returns a new Interface.
func New(f internalinterfaces.SharedInformerFactory, namespace string, tweakListOptions internalinterfaces.TweakListOptionsFunc) Interface {
	return &version{factory: f, namespace: namespace, tweakListOptions: tweakListOptions}
}

// KubeAPIServers returns a KubeAPIServerInformer.
func (v *version) KubeAPIServers() KubeAPIServerInformer {
	return &kubeAPIServerInformer{factory: v.factory, tweakListOptions: v.tweakListOptions}
}

// KubeControllerManagers returns a KubeControllerManagerInformer.
func (v *version) KubeControllerManagers() KubeControllerManagerInformer {
	return &kubeControllerManagerInformer{factory: v.factory, tweakListOptions: v.tweakListOptions}
}

// OpenShiftAPIServers returns a OpenShiftAPIServerInformer.
func (v *version) OpenShiftAPIServers() OpenShiftAPIServerInformer {
	return &openShiftAPIServerInformer{factory: v.factory, tweakListOptions: v.tweakListOptions}
}
