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
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	kuberrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1alpha1 "github.com/nestorsokil/kbg/api/v1alpha1"
)

const (
	LabelColor = "kbg/color"
	LabelName  = "name" // todo k8s.io?
)

// BlueGreenDeploymentReconciler reconciles a BlueGreenDeployment object
type BlueGreenDeploymentReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=cluster.kbg,resources=bluegreendeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cluster.kbg,resources=bluegreendeployments/status,verbs=get;update;patch
func (r *BlueGreenDeploymentReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("bluegreendeployment", req.NamespacedName)

	deploy, err := r.obtainDeployment(ctx, req.NamespacedName)
	if err != nil {
		if kuberrors.IsNotFound(err) {
			log.Info("BlueGreenDeployment was deleted") // probably...
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to obtain BlueGreenDeployment")
		return ctrl.Result{}, err
	}

	if err := Runner(log, r.Client, deploy).Run(ctx); err != nil {
		log.Error(err, "Failed to reconcile")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *BlueGreenDeploymentReconciler) obtainDeployment(ctx context.Context, name types.NamespacedName) (*clusterv1alpha1.BlueGreenDeployment, error) {
	var deploy clusterv1alpha1.BlueGreenDeployment
	if err := r.Get(ctx, name, &deploy); err != nil {
		return nil, errors.Wrap(err, "failed to Get deploy")
	}
	if deploy.Status.ActiveColor == "" {
		deploy.Status.ActiveColor = clusterv1alpha1.ColorBlue
		if err := r.Client.Update(ctx, &deploy); err != nil {
			return nil, errors.Wrap(err, "failed to set color")
		}
	}
	return &deploy, nil
}

func (r *BlueGreenDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1alpha1.BlueGreenDeployment{}).
		Complete(r)
}
