package install

import (
	"context"
	"flag"
	"fmt"
	"strings"

	mf "github.com/jcrossley3/manifestival"
	servingv1alpha1 "github.com/openshift-knative/knative-serving-operator/pkg/apis/serving/v1alpha1"
	"github.com/openshift-knative/knative-serving-operator/version"
	configv1 "github.com/openshift/api/config/v1"

	"github.com/operator-framework/operator-sdk/pkg/predicate"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var (
	filename = flag.String("filename", "deploy/resources",
		"The filename containing the YAML resources to apply")
	recursive = flag.Bool("recursive", false,
		"If filename is a directory, process all manifests recursively")
	installNs = flag.String("install-ns", "",
		"The namespace in which to create an Install resource, if none exist")
	log = logf.Log.WithName("controller_install")
)

// Add creates a new Install Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	manifest, err := mf.NewManifest(*filename, *recursive, mgr.GetClient())
	if err != nil {
		return err
	}
	return add(mgr, newReconciler(mgr, manifest))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, man mf.Manifest) reconcile.Reconciler {
	return &ReconcileInstall{client: mgr.GetClient(), scheme: mgr.GetScheme(), config: man}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("install-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource Install
	err = c.Watch(&source.Kind{Type: &servingv1alpha1.Install{}}, &handler.EnqueueRequestForObject{}, predicate.GenerationChangedPredicate{})
	if err != nil {
		return err
	}

	// Make an attempt to create an Install CR, if necessary
	if len(*installNs) > 0 {
		c, _ := client.New(mgr.GetConfig(), client.Options{})
		go autoInstall(c, *installNs)
	}
	return nil
}

var _ reconcile.Reconciler = &ReconcileInstall{}

// ReconcileInstall reconciles a Install object
type ReconcileInstall struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client client.Client
	scheme *runtime.Scheme
	config mf.Manifest
}

// Reconcile reads that state of the cluster for a Install object and makes changes based on the state read
// and what is in the Install.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileInstall) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling Install")

	// Fetch the Install instance
	instance := &servingv1alpha1.Install{}
	err := r.client.Get(context.TODO(), request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			r.config.DeleteAll()
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	stages := []func(*servingv1alpha1.Install) error{
		r.install,
		r.deleteObsoleteResources,
		r.checkForMinikube,
		r.checkForOpenShift,
		r.configure,
	}

	for _, stage := range stages {
		if err := stage(instance); err != nil {
			return reconcile.Result{}, err
		}
	}

	return reconcile.Result{}, nil
}

// Apply the embedded resources
func (r *ReconcileInstall) install(instance *servingv1alpha1.Install) error {
	// Transform resources as appropriate
	fns := []mf.Transformer{mf.InjectOwner(instance), addPolicyRules}
	if len(instance.Spec.Namespace) > 0 {
		fns = append(fns, mf.InjectNamespace(instance.Spec.Namespace))
	}
	r.config.Transform(fns...)

	if instance.Status.Version == version.Version {
		// we've already successfully applied our YAML
		return nil
	}
	// Apply the resources in the YAML file
	if err := r.config.ApplyAll(); err != nil {
		return err
	}

	// Update status
	instance.Status.Resources = r.config.Resources
	instance.Status.Version = version.Version
	if err := r.client.Status().Update(context.TODO(), instance); err != nil {
		return err
	}
	return nil
}

// Set ConfigMap values from Install spec
func (r *ReconcileInstall) configure(instance *servingv1alpha1.Install) error {
	for suffix, config := range instance.Spec.Config {
		name := "config-" + suffix
		cm, err := r.config.Get(r.config.Find("v1", "ConfigMap", name))
		if err != nil {
			return err
		}
		if cm == nil {
			log.Error(fmt.Errorf("ConfigMap '%s' not found", name), "Invalid Install spec")
			continue
		}
		if err := r.updateConfigMap(cm, config); err != nil {
			return err
		}
	}
	return nil
}

// Delete obsolete istio-system resources, if any
func (r *ReconcileInstall) deleteObsoleteResources(instance *servingv1alpha1.Install) error {
	resource := &unstructured.Unstructured{}
	resource.SetNamespace("istio-system")
	resource.SetName("knative-ingressgateway")
	resource.SetAPIVersion("v1")
	resource.SetKind("Service")
	if err := r.config.Delete(resource); err != nil {
		return err
	}
	resource.SetAPIVersion("apps/v1")
	resource.SetKind("Deployment")
	if err := r.config.Delete(resource); err != nil {
		return err
	}
	resource.SetAPIVersion("autoscaling/v1")
	resource.SetKind("HorizontalPodAutoscaler")
	return r.config.Delete(resource)
}

// Set some data in a configmap, only overwriting common keys
func (r *ReconcileInstall) updateConfigMap(cm *unstructured.Unstructured, data map[string]string) error {
	for k, v := range data {
		values := []interface{}{"map", cm.GetName(), k, v}
		if x, found, _ := unstructured.NestedString(cm.Object, "data", k); found {
			if v == x {
				continue
			}
			values = append(values, "previous", x)
		}
		log.Info("Setting", values...)
		unstructured.SetNestedField(cm.Object, v, "data", k)
	}
	return r.config.Apply(cm)

}

// Configure minikube if we're soaking in it
func (r *ReconcileInstall) checkForMinikube(instance *servingv1alpha1.Install) error {
	node := &v1.Node{}
	err := r.client.Get(context.TODO(), types.NamespacedName{Name: "minikube"}, node)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil // not running on minikube!
		}
		return err
	}
	cm, err := r.config.Get(r.config.Find("v1", "ConfigMap", "config-network"))
	if err != nil {
		return err
	}
	if cm == nil {
		log.Error(err, "Missing ConfigMap", "name", "config-network")
		return nil // no sense in trying if the CM is gone
	}
	log.Info("Detected minikube; checking egress")
	data := map[string]string{"istio.sidecar.includeOutboundIPRanges": "10.0.0.1/24"}
	return r.updateConfigMap(cm, data)
}

// Configure OpenShift if we're soaking in it
func (r *ReconcileInstall) checkForOpenShift(instance *servingv1alpha1.Install) error {
	tasks := []func(*servingv1alpha1.Install) error{
		r.configureIngressForOpenShift,
		r.configureEgressForOpenShift,
	}
	for _, task := range tasks {
		if err := task(instance); err != nil {
			return err
		}
	}
	return nil
}

func (r *ReconcileInstall) configureIngressForOpenShift(instance *servingv1alpha1.Install) error {
	ingressConfig := &configv1.Ingress{}
	if err := r.client.Get(context.TODO(), types.NamespacedName{Name: "cluster"}, ingressConfig); err != nil {
		if meta.IsNoMatchError(err) {
			log.V(1).Info("OpenShift Ingress Config is not available.")
			return nil // not on OpenShift
		}
		return err
	}
	domain := ingressConfig.Spec.Domain
	// If domain is available, update config-domain config map
	if len(domain) > 0 {
		log.Info("OpenShift Ingress Config is available", "Domain", domain)
		cm := r.config.Find("v1", "ConfigMap", "config-domain")
		data := map[string]string{domain: ""}
		return r.updateConfigMap(cm, data)
	}
	return nil
}

// Set istio.sidecar.includeOutboundIPRanges property with service network
func (r *ReconcileInstall) configureEgressForOpenShift(instance *servingv1alpha1.Install) error {
	networkConfig := &configv1.Network{}
	if err := r.client.Get(context.TODO(), types.NamespacedName{Name: "cluster"}, networkConfig); err != nil {
		if meta.IsNoMatchError(err) {
			log.V(1).Info("OpenShift Network Config is not available.")
			return nil // not on OpenShift
		}
		return err
	}
	serviceNetwork := strings.Join(networkConfig.Spec.ServiceNetwork, ",")
	// If service network is available, update config-network config map
	if len(serviceNetwork) > 0 {
		log.Info("OpenShift Network Config is available", "ServiceNetwork", serviceNetwork)
		cm := r.config.Find("v1", "ConfigMap", "config-network")
		data := map[string]string{"istio.sidecar.includeOutboundIPRanges": serviceNetwork}
		return r.updateConfigMap(cm, data)
	}
	return nil
}

func autoInstall(c client.Client, ns string) (err error) {
	const path = "deploy/crds/serving_v1alpha1_install_cr.yaml"
	log.Info("Automatic Install requested", "namespace", ns)
	installList := &servingv1alpha1.InstallList{}
	err = c.List(context.TODO(), &client.ListOptions{Namespace: ns}, installList)
	if err != nil {
		log.Error(err, "Unable to list Installs")
		return err
	}
	if len(installList.Items) == 0 {
		if manifest, err := mf.NewManifest(path, false, c); err == nil {
			if err = manifest.Transform(mf.InjectNamespace(ns)).ApplyAll(); err != nil {
				log.Error(err, "Unable to create Install")
			}
		} else {
			log.Error(err, "Unable to create Install manifest")
		}
	} else {
		log.Info("Install found", "name", installList.Items[0].Name)
	}
	return err
}

// TODO: These are addressed in master and shouldn't be required for 0.6.0
func addPolicyRules(u *unstructured.Unstructured) *unstructured.Unstructured {
	if u.GetKind() == "ClusterRole" && u.GetName() == "knative-serving-core" {
		field, _, _ := unstructured.NestedFieldNoCopy(u.Object, "rules")
		var rules []interface{} = field.([]interface{})
		for _, rule := range rules {
			m := rule.(map[string]interface{})
			for _, group := range m["apiGroups"].([]interface{}) {
				if group == "apps" {
					m["resources"] = append(m["resources"].([]interface{}), "deployments/finalizers")
				}
			}
		}
		// Required to open privileged ports in OpenShift
		unstructured.SetNestedField(u.Object, append(rules, map[string]interface{}{
			"apiGroups":     []interface{}{"security.openshift.io"},
			"verbs":         []interface{}{"use"},
			"resources":     []interface{}{"securitycontextconstraints"},
			"resourceNames": []interface{}{"privileged", "anyuid"},
		}), "rules")
	}
	return u
}
