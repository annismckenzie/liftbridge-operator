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
	appsv1 "k8s.io/api/apps/v1"
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
	"github.com/liftbridge-io/liftbridge-operator/pkg/utils/k8s"
	"github.com/liftbridge-io/liftbridge/server/conf"
)

// 'liftbridge-operator/*' labels/annotations are reserved for the operator.
var (
	prefixDomain                        = "liftbridge-operator"
	clusterLabelKey                     = prefixDomain + "/cluster-name"
	configMapHashAnnotationKey          = prefixDomain + "/config-map-hash"
	statefulSetPodSpecHashAnnotationKey = prefixDomain + "/stateful-set-pod-spec-hash"
)

const (
	headlessServiceNameTemplate      = "%s-headless"                       // cluster-name-headless
	statefulSetNameTemplate          = "%s-liftbridge-cluster-statefulset" // cluster-name-liftbridge-cluster-statefulset
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
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list

func (r *LiftbridgeClusterReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	logger := r.log.WithValues("liftbridgecluster", req.NamespacedName)

	logger.Info(fmt.Sprintf("Reconciling LiftbridgeCluster: %+v", req.NamespacedName))

	liftbridgeCluster, response, err := r.fetchLiftbridgeCluster(ctx, req.NamespacedName)
	if err != nil {
		logger.Error(err, "Failed to fetch LiftbridgeCluster CR, might have been deleted, requeuing.")
		return response.result, response.err
	}

	statefulSetName := fmt.Sprintf(statefulSetNameTemplate, liftbridgeCluster.GetName())
	existingStatefulSet := appsv1.StatefulSet{}
	err = r.client.Get(ctx, types.NamespacedName{Name: statefulSetName, Namespace: liftbridgeCluster.GetNamespace()}, &existingStatefulSet)

	if apierrors.IsNotFound(err) {
		liftbridgeCluster.Status.ClusterState = configv1alpha1.ClusterStateCreating
		if err = r.client.Status().Update(ctx, liftbridgeCluster); err != nil {
			logger.Error(err, "Failed to reconcile status of LiftbridgeCluster CR, requeuing.")
			return r.defaultRequeueResponse, nil
		}
	}

	// reconcile headless service
	headlessServiceName := fmt.Sprintf(headlessServiceNameTemplate, liftbridgeCluster.GetName())
	headlessService, response, err := r.reconcileService(ctx, liftbridgeCluster, "/etc/templates/services/headless-service.yaml", headlessServiceName)
	if err != nil {
		logger.Error(err, "Failed to reconcile headless service, requeuing.")
		return response.result, response.err
	}
	logger.Info("Successfully reconciled headless service", "serviceName", headlessService.GetName())

	connectServiceName := liftbridgeCluster.GetName()
	connectService, response, err := r.reconcileService(ctx, liftbridgeCluster, "/etc/templates/services/service.yaml", connectServiceName)
	if err != nil {
		logger.Error(err, "Failed to reconcile connect service, requeuing.")
		return response.result, response.err
	}
	logger.Info("Successfully reconciled connect service", "serviceName", connectService.GetName())

	// reconcile configuration
	configmap, response, err := r.reconcileConfigMap(ctx, liftbridgeCluster)
	if err != nil {
		logger.Error(err, "Failed to reconcile configmap, requeuing.")
		return response.result, response.err
	}
	logger.Info(fmt.Sprintf("Successfully reconciled configmap: %+v", configmap.GetName()))

	// reconcile stateful set
	statefulSet, response, err := r.reconcileStatefulSet(ctx, liftbridgeCluster, headlessService, configmap)
	if err != nil {
		logger.Error(err, "Failed to reconcile stateful set, requeuing.")
		return response.result, response.err
	}

	if !r.statefulSetIsHealthy(ctx, logger, statefulSet) {
		logger.Info("StatefulSet pods are not yet healthy, requeuing.")
		return r.defaultRequeueResponse, nil
	}
	logger.Info("All StatefulSet pods are healthy.")

	liftbridgeCluster.Status.ClusterState = configv1alpha1.ClusterStateStable
	if err := r.client.Status().Update(ctx, liftbridgeCluster); err != nil {
		logger.Error(err, "Failed to reconcile status of LiftbridgeCluster CR")
		return r.defaultRequeueResponse, nil
	}
	logger.Info(fmt.Sprintf("Successfully reconciled stateful set: %+v", statefulSet.GetName()))

	return ctrl.Result{}, nil
}

func (r *LiftbridgeClusterReconciler) fetchLiftbridgeCluster(ctx context.Context, namespacedName types.NamespacedName) (*configv1alpha1.LiftbridgeCluster, *response, error) {
	liftbridgeCluster := configv1alpha1.LiftbridgeCluster{}
	if err := r.client.Get(ctx, namespacedName, &liftbridgeCluster); err != nil {
		if apierrors.IsNotFound(err) { // request object not found, could have been deleted after reconcile request, return and don't requeue
			return nil, &response{result: reconcile.Result{}, err: nil}, err
		}
		return nil, &response{result: r.defaultRequeueResponse, err: nil}, err
	}
	return liftbridgeCluster.DeepCopy(), nil, nil
}

func (r *LiftbridgeClusterReconciler) buildStatefulSet(ctx context.Context, logger logr.Logger, c *configv1alpha1.LiftbridgeCluster, svc *corev1.Service, cm *corev1.ConfigMap, existingStatefulSet *appsv1.StatefulSet) (*appsv1.StatefulSet, *response, error) {
	ownerRef := metav1.NewControllerRef(c, configv1alpha1.GroupVersionKind)
	statefulSetName := fmt.Sprintf(statefulSetNameTemplate, c.GetName())
	statefulSet := appsv1.StatefulSet{}
	statefulsetTemplate, err := ioutil.ReadFile("/etc/templates/statefulset/statefulset.yaml")
	if err != nil {
		logger.Error(err, "Failed to read template for the StatefulSet", "filename", "/etc/templates/statefulset/statefulset.yaml")
		return nil, &response{result: r.defaultRequeueResponse, err: nil}, err
	}
	if err = yaml.Unmarshal([]byte(statefulsetTemplate), &statefulSet); err != nil {
		logger.Error(err, "Failed to unmarshal template")
		return nil, &response{result: r.defaultRequeueResponse, err: nil}, err
	}

	if existingStatefulSet != nil {
		statefulSet.ObjectMeta = existingStatefulSet.ObjectMeta
		statefulSet.Spec.Selector = existingStatefulSet.Spec.Selector
	} else {
		statefulSet.Spec.ServiceName = svc.GetName()
		statefulSet.SetName(statefulSetName)
		statefulSet.SetNamespace(c.GetNamespace())
		statefulSet.ObjectMeta.Labels = addKeyToMap(statefulSet.ObjectMeta.Labels, clusterLabelKey, c.GetName())
		statefulSet.ObjectMeta.Annotations = addKeyToMap(statefulSet.ObjectMeta.Annotations, configMapHashAnnotationKey, cm.GetName())
		metav1.AddLabelToSelector(statefulSet.Spec.Selector, clusterLabelKey, c.GetName())
		statefulSet.Spec.Template.ObjectMeta.Labels = addKeyToMap(statefulSet.Spec.Template.ObjectMeta.Labels, clusterLabelKey, c.GetName())
		statefulSet.SetOwnerReferences([]metav1.OwnerReference{*ownerRef})
	}
	if c.Spec.Image != "" {
		var found bool
		if statefulSet.Spec.Template.Spec, found = setImageOfContainerInPodSpec(statefulSet.Spec.Template.Spec, "liftbridge", c.Spec.Image); !found {
			return nil, &response{result: r.defaultRequeueResponse, err: nil}, errors.New("failed to find liftbridge container to update image tag in")
		}
	}
	if c.Spec.Replicas != nil {
		var replicas = *c.Spec.Replicas
		statefulSet.Spec.Replicas = &replicas
	}
	var found bool
	if statefulSet.Spec.Template.Spec, found = setConfigMapNameInVolume(statefulSet.Spec.Template.Spec, "liftbridge-config", cm.GetName()); !found {
		return nil, &response{result: r.defaultRequeueResponse, err: nil}, errors.New("failed to find liftbridge config map volume to update config map name in")
	}
	statefulSet.Spec.VolumeClaimTemplates = c.Spec.VolumeClaimTemplates
	statefulSet.ObjectMeta.Annotations = addKeyToMap(statefulSet.ObjectMeta.Annotations, statefulSetPodSpecHashAnnotationKey, dataHash(statefulSet.Spec.Template.Spec))

	return &statefulSet, nil, nil
}

func (r *LiftbridgeClusterReconciler) fetchStatefulSet(ctx context.Context, c *configv1alpha1.LiftbridgeCluster) (*appsv1.StatefulSet, error) {
	statefulSetName := fmt.Sprintf(statefulSetNameTemplate, c.GetName())
	statefulSet := appsv1.StatefulSet{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: statefulSetName, Namespace: c.GetNamespace()}, &statefulSet); err != nil {
		return statefulSet.DeepCopy(), err
	}
	return statefulSet.DeepCopy(), nil
}

func (r *LiftbridgeClusterReconciler) reconcileStatefulSet(ctx context.Context, c *configv1alpha1.LiftbridgeCluster, svc *corev1.Service, cm *corev1.ConfigMap) (*appsv1.StatefulSet, *response, error) {
	var (
		logger      = r.log.WithValues("liftbridgecluster", types.NamespacedName{Name: c.GetName(), Namespace: c.GetNamespace()})
		statefulSet = &appsv1.StatefulSet{}
		resp        *response
		err         error
	)

	existingStatefulSet, err := r.fetchStatefulSet(ctx, c)

	switch {
	case apierrors.IsNotFound(err): // create the stateful set
		logger.Info("StatefulSet doesn't exist yet, creating.")

		if statefulSet, resp, err = r.buildStatefulSet(ctx, logger, c, svc, cm, nil); err != nil {
			return nil, resp, err
		}

		if err = r.client.Create(ctx, statefulSet); err != nil {
			return nil, resp, err
		}
	case err == nil:
		if !metav1.IsControlledBy(existingStatefulSet, c) {
			err = fmt.Errorf("StatefulSet %q already exists and is not controlled by Liftbridge Operator (controllee: %+v)", statefulSet.GetName(), metav1.GetControllerOf(existingStatefulSet))
			return nil, &response{result: r.defaultRequeueResponse, err: nil}, err
		}
	default:
		logger.Error(err, "Failed to query the API server for the current Liftbridge cluster StatefulSet")
		return nil, &response{result: r.defaultRequeueResponse, err: nil}, err
	}
	return statefulSet, nil, nil
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

func setImageOfContainerInPodSpec(podSpec corev1.PodSpec, containerName, image string) (corev1.PodSpec, bool) {
	var found bool
	for i, containerSpec := range podSpec.Containers {
		if containerSpec.Name == containerName {
			found = true
			podSpec.Containers[i].Image = image
		}
	}

	return podSpec, found
}

func setConfigMapNameInVolume(podSpec corev1.PodSpec, volumeName, configMapName string) (corev1.PodSpec, bool) {
	var found bool
	for i, volumeSpec := range podSpec.Volumes {
		if volumeSpec.Name == volumeName {
			found = true
			podSpec.Volumes[i].VolumeSource.ConfigMap.LocalObjectReference.Name = configMapName
		}
	}

	return podSpec, found
}

func (r *LiftbridgeClusterReconciler) statefulSetIsHealthy(ctx context.Context, logger logr.Logger, existingStatefulSet *appsv1.StatefulSet) bool {
	// look at statefulset status
	statefulSetStatus := existingStatefulSet.Status
	if *existingStatefulSet.Spec.Replicas == statefulSetStatus.UpdatedReplicas && *existingStatefulSet.Spec.Replicas == statefulSetStatus.CurrentReplicas && *existingStatefulSet.Spec.Replicas == statefulSetStatus.Replicas {
		logger.Info("StatefulSet is not healthy", "reason", "replicas are not up-to-date")
		return true
	}

	podList, err := r.clientset.CoreV1().Pods(existingStatefulSet.GetNamespace()).List(metav1.ListOptions{})
	if err != nil {
		logger.Info("StatefulSet is not healthy", "reason", "failed to fetch pods in namespace")
		return false
	}
	pods := make([]corev1.Pod, 0, len(podList.Items))
	for _, pod := range podList.Items {
		if metav1.IsControlledBy(&pod, existingStatefulSet) {
			pods = append(pods, pod)
		}
	}
	if len(pods) == 0 {
		logger.Info("StatefulSet is not healthy", "reason", "filtered pods are empty")
		return false
	}
	for _, pod := range pods {
		if !k8s.IsPodReady(pod) {
			logger.Info("StatefulSet is not healthy", "reason", "pod is not ready", "podName", pod.GetName())
			return false
		}
	}
	return true
}
