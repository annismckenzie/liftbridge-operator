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
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	configv1alpha1 "github.com/liftbridge-io/liftbridge-operator/pkg/apis/v1alpha1"
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

func (r *LiftbridgeClusterReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	_ = context.Background()
	logger := r.log.WithValues("liftbridgecluster", req.NamespacedName)

	logger.Info(fmt.Sprintf("Reconciling LiftbridgeCluster: %+v", req.NamespacedName))

	liftbridgeCluster, response, err := r.fetchLiftbridgeCluster(req.NamespacedName)
	if err != nil {
		return response.result, response.err
	}

	logger.Info(fmt.Sprintf("Reconciling LiftbridgeCluster: %+v", liftbridgeCluster))

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

func (r *LiftbridgeClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&configv1alpha1.LiftbridgeCluster{}).
		Complete(r)
}
