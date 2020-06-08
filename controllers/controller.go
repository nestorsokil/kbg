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
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
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
	log := r.Log.WithValues(
		"bluegreendeployment", req.NamespacedName)

	rn, err := NewEngine(ctx, log, r.Client, req)
	if err != nil {
		if err == ErrDeleted {
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to reconcile")
		return ctrl.Result{}, err
	}

	switch {
	case rn.OverrideColor() != nil:
		override := *rn.OverrideColor()
		if _, ok := clusterv1alpha1.Colors[override]; !ok {
			err := fmt.Errorf("unknown color override value %s", override)
			log.Error(err, err.Error())
			return ctrl.Result{}, err
		} else if rn.IsActive(override) {
			log.Info("Active matches override color, skipping")
		} else if err := rn.Swap(ctx); err != nil {
			log.Error(err, "Failed to swap")
			rn.SetStatus(ctx, clusterv1alpha1.StatusDeployFailed)
			return ctrl.Result{}, err
		}
		if rn.CurrentStatus() != clusterv1alpha1.StatusOverridden {
			rn.SetStatus(ctx, clusterv1alpha1.StatusOverridden)
		}
		return ctrl.Result{}, nil
	case rn.ActiveMatchesSpec():
		log.Info("No changes were detected, skipping")
		if rn.CurrentStatus() != clusterv1alpha1.StatusNominal {
			rn.SetStatus(ctx, clusterv1alpha1.StatusNominal)
		}
		return ctrl.Result{}, nil
	case rn.BackupMatchesSpec():
		log.Info("New config matches backup ReplicaSet, swapping")
		if rn.Backup.Spec.Replicas != rn.Deploy.Spec.Replicas {
			if err := rn.Scale(ctx, rn.Backup, rn.Deploy.Spec.Replicas); err != nil {
				rn.SetStatus(ctx, clusterv1alpha1.StatusUnknown)
				return ctrl.Result{}, errors.Wrap(err, "unable to scale")
			}
		}
	default:
		log.Info("New configuration detected, running B/G deployment")
		rn.SetStatus(ctx, clusterv1alpha1.StatusDeploying)
		if err := rn.UpgradeBackup(ctx); err != nil {
			rn.SetStatus(ctx, clusterv1alpha1.StatusDeployFailed)
			return ctrl.Result{}, errors.Wrap(err, "unable to upgrade ReplicaSet")
		}
	}

	// TODO initiate smoke tests here

	if err := rn.Swap(ctx); err != nil {
		rn.SetStatus(ctx, clusterv1alpha1.StatusDeployFailed)
		return ctrl.Result{}, errors.Wrap(err, "failed to swap service")
	}
	rn.SetStatus(ctx, clusterv1alpha1.StatusNominal)
	return ctrl.Result{}, nil
}

func (r *BlueGreenDeploymentReconciler) obtainDeployment(ctx context.Context, name types.NamespacedName) (*clusterv1alpha1.BlueGreenDeployment, error) {
	var deploy clusterv1alpha1.BlueGreenDeployment
	if err := r.Get(ctx, name, &deploy); err != nil {
		return nil, err
	}
	if deploy.Status.ActiveColor == "" {
		r.Log.Info("No color set for deployment, updating")
		deploy.Status.ActiveColor = clusterv1alpha1.ColorBlue
		if err := r.Client.Status().Update(ctx, &deploy); err != nil {
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
