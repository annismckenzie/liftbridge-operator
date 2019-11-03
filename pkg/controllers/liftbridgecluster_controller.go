/*

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"errors"
	"fmt"
	"hash/adler32"
	"io/ioutil"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	hashutil "k8s.io/kubernetes/pkg/util/hash"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	configv1alpha1 "github.com/liftbridge-io/liftbridge-operator/pkg/apis/v1alpha1"
	"github.com/liftbridge-io/liftbridge/server/conf"
)

// 'liftbridge-operator/*' labels/annotations are reserved for the operator.
var (
	prefixDomain                        = "liftbridge-operator"
	clusterLabelKey                     = prefixDomain + "/cluster-name"
	configMapHashAnnotationKey          = prefixDomain + "/config-map-hash"
)

const (
	headlessServiceNameTemplate      = "%s-headless"                       // cluster-name-headless
	configMapNameTemplate            = "%s-config-%s"                      // cluster-name-config-data-hash
	liftbridgeConfigConfigMapDataKey = "liftbridge.conf"
)

const defaultRequeueAfter time.Duration = 30 * time.Second

// response is a helper struct to cut down on the amount of if and switch statements.
type response struct {
	result reconcile.Result
	err    error
}

// LiftbridgeClusterReconciler reconciles a LiftbridgeCluster object
type LiftbridgeClusterReconciler struct {
	client                 client.Client
	clientset              *kubernetes.Clientset
	log                    logr.Logger
	scheme                 *runtime.Scheme
	defaultRequeueResponse reconcile.Result
}

func NewLiftbridgeClusterReconciler(client client.Client, clientset *kubernetes.Clientset, log logr.Logger, scheme *runtime.Scheme) *LiftbridgeClusterReconciler {
	return &LiftbridgeClusterReconciler{
		client:                 client,
		clientset:              clientset,
		log:                    log,
		scheme:                 scheme,
		defaultRequeueResponse: reconcile.Result{RequeueAfter: defaultRequeueAfter},
	}
}

// +kubebuilder:rbac:groups=config.operator.liftbridge.io,resources=liftbridgeclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=config.operator.liftbridge.io,resources=liftbridgeclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete

func (r *LiftbridgeClusterReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	logger := r.log.WithValues("liftbridgecluster", req.NamespacedName)

	logger.Info(fmt.Sprintf("Reconciling LiftbridgeCluster: %+v", req.NamespacedName))

	liftbridgeCluster, response, err := r.fetchLiftbridgeCluster(ctx, req.NamespacedName)
	if err != nil {
		logger.Error(err, "Failed to fetch LiftbridgeCluster CR, might have been deleted, requeuing.")
		return response.result, response.err
	}
	// reconcile headless service
	headlessServiceName := fmt.Sprintf(headlessServiceNameTemplate, liftbridgeCluster.GetName())
	service, response, err := r.reconcileService(ctx, liftbridgeCluster, "/etc/templates/services/headless-service.yaml", headlessServiceName)
	if err != nil {
		logger.Error(err, "Failed to reconcile headless service, requeuing.")
		return response.result, response.err
	}
	logger.Info(fmt.Sprintf("Successfully reconciled headless service: %+v", service.GetName()))

	connectServiceName := liftbridgeCluster.GetName()
	service, response, err = r.reconcileService(ctx, liftbridgeCluster, "/etc/templates/services/service.yaml", connectServiceName)
	if err != nil {
		logger.Error(err, "Failed to reconcile connect service, requeuing.")
		return response.result, response.err
	}
	logger.Info(fmt.Sprintf("Successfully reconciled connect service: %+v", service.GetName()))

	// reconcile configuration
	configmap, response, err := r.reconcileConfigMap(ctx, liftbridgeCluster)
	if err != nil {
		logger.Error(err, "Failed to reconcile configmap, requeuing.")
		return response.result, response.err
	}
	logger.Info(fmt.Sprintf("Successfully reconciled configmap: %+v", configmap.GetName()))

	return ctrl.Result{}, nil
}

func (r *LiftbridgeClusterReconciler) fetchLiftbridgeCluster(namespacedName types.NamespacedName) (*configv1alpha1.LiftbridgeCluster, *response, error) {
	liftbridgeCluster := &configv1alpha1.LiftbridgeCluster{}
	if err := r.client.Get(context.Background(), namespacedName, liftbridgeCluster); err != nil {
		if errors.IsNotFound(err) { // request object not found, could have been deleted after reconcile request, return and don't requeue
			return nil, &response{result: reconcile.Result{}, err: nil}, err
		}
		return nil, &response{result: r.defaultRequeueResponse, err: nil}, err
	}
	return liftbridgeCluster.DeepCopy(), nil, nil
}

func (r *LiftbridgeClusterReconciler) reconcileConfigMap(ctx context.Context, c *configv1alpha1.LiftbridgeCluster) (*corev1.ConfigMap, *response, error) {
	ownerRef := metav1.NewControllerRef(c, configv1alpha1.GroupVersionKind)
	logger := r.log.WithValues("liftbridgecluster", types.NamespacedName{Name: c.GetName(), Namespace: c.GetNamespace()})

	// validate config for correctness
	if _, err := conf.Parse(c.Spec.Config); err != nil {
		logger.Error(err, "Failed to parse configuration correctly")
		return nil, &response{result: r.defaultRequeueResponse, err: nil}, err
	}

	configMap := corev1.ConfigMap{Data: map[string]string{liftbridgeConfigConfigMapDataKey: c.Spec.Config}}
	configMapName := fmt.Sprintf(configMapNameTemplate, c.GetName(), dataHash(configMap.Data))

	err := r.client.Get(ctx, types.NamespacedName{Name: configMapName, Namespace: c.GetNamespace()}, &configMap)
	switch {
	case apierrors.IsNotFound(err): // create the headless service
		logger.Info("Config map doesn't exist yet, creating.")
		configMap.SetName(configMapName)
		configMap.SetNamespace(c.GetNamespace())
		configMap.ObjectMeta.Labels = addKeyToMap(configMap.ObjectMeta.Labels, clusterLabelKey, c.GetName())
		configMap.SetOwnerReferences([]metav1.OwnerReference{*ownerRef})

		if err = r.client.Create(ctx, &configMap); err != nil {
			return nil, &response{result: r.defaultRequeueResponse, err: nil}, err
		}
	case err == nil:
		if !metav1.IsControlledBy(&configMap, c) {
			err = fmt.Errorf("Config map %q is not controlled by Liftbridge Operator (controllee: %+v)", configMap.GetName(), metav1.GetControllerOf(&configMap))
			return nil, &response{result: r.defaultRequeueResponse, err: nil}, err
		}
	default:
		return nil, &response{result: r.defaultRequeueResponse, err: nil}, err
	}
	return &configMap, nil, nil
}

func (r *LiftbridgeClusterReconciler) buildService(ctx context.Context, logger logr.Logger, c *configv1alpha1.LiftbridgeCluster, templatePath, serviceName string) (*corev1.Service, *response, error) {
	ownerRef := metav1.NewControllerRef(c, configv1alpha1.GroupVersionKind)
	service := corev1.Service{}
	serviceTemplate, err := ioutil.ReadFile(templatePath)
	if err != nil {
		logger.Error(err, "Failed to read template for service", "filename", templatePath, "serviceName", serviceName)
		return nil, &response{result: r.defaultRequeueResponse, err: nil}, err
	}
	if err = yaml.Unmarshal([]byte(serviceTemplate), &service); err != nil {
		logger.Error(err, "Failed to unmarshal template")
		return nil, &response{result: r.defaultRequeueResponse, err: nil}, err
	}
	service.SetName(serviceName)
	service.SetNamespace(c.GetNamespace())
	service.ObjectMeta.Labels = addKeyToMap(service.ObjectMeta.Labels, clusterLabelKey, c.GetName())
	service.Spec.Selector = addKeyToMap(service.Spec.Selector, clusterLabelKey, c.GetName())
	service.SetOwnerReferences([]metav1.OwnerReference{*ownerRef})

	return &service, nil, nil
}

func (r *LiftbridgeClusterReconciler) reconcileService(ctx context.Context, c *configv1alpha1.LiftbridgeCluster, templatePath, serviceName string) (*corev1.Service, *response, error) {
	var (
		logger          = r.log.WithValues("liftbridgecluster", types.NamespacedName{Name: c.GetName(), Namespace: c.GetNamespace()})
		service         = &corev1.Service{}
		existingService = corev1.Service{}
		resp            *response
		err             error
	)

	err = r.client.Get(ctx, types.NamespacedName{Name: serviceName, Namespace: c.GetNamespace()}, &existingService)
	switch {
	case apierrors.IsNotFound(err): // create the headless service
		logger.Info("Service doesn't exist yet, creating.", "serviceName", serviceName)
		if service, resp, err = r.buildService(ctx, logger, c, templatePath, serviceName); err != nil {
			return nil, resp, err
		}

		if err = r.client.Create(ctx, service); err != nil {
			return nil, &response{result: r.defaultRequeueResponse, err: nil}, err
		}
	case err == nil:
		if !metav1.IsControlledBy(&existingService, c) {
			err = fmt.Errorf("Service %q is not controlled by Liftbridge Operator (controllee: %+v)", serviceName, metav1.GetControllerOf(&existingService))
			return nil, &response{result: r.defaultRequeueResponse, err: nil}, err
		}
		logger.Info("Service already exist, not updating.", "serviceName", serviceName)
	default:
		return nil, &response{result: r.defaultRequeueResponse, err: nil}, err
	}
	return service, nil, nil
}

func (r *LiftbridgeClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&configv1alpha1.LiftbridgeCluster{}).
		Complete(r)
}

func addKeyToMap(labels map[string]string, key, value string) map[string]string {
	if labels == nil {
		labels = map[string]string{}
	}

	labels[key] = value
	return labels
}

// generates a unique hash for an object
func dataHash(obj interface{}) string {
	hasher := adler32.New()
	hashutil.DeepHashObject(hasher, obj)
	return fmt.Sprintf("%v", hasher.Sum32())
}
