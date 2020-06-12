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
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1alpha1 "github.com/nestorsokil/kbg/api/v1alpha1"
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

	eng, err := NewEngine(ctx, log, r.Client, req)
	if err != nil {
		if err == ErrDeleted {
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to reconcile")
		return ctrl.Result{}, err
	}

	switch {
	case eng.Deploy.Status.StatusName == clusterv1alpha1.StatusDeployFailed:
		if err := eng.updateDeploy(ctx, func(d *clusterv1alpha1.BlueGreenDeployment) {
			d.Spec.Template = eng.Active.Spec.Template
		}); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to restore deployment")
		}
		eng.SetStatus(ctx, clusterv1alpha1.StatusNominal)
		return ctrl.Result{}, nil
	case eng.OverrideColor() != nil:
		override := *eng.OverrideColor()
		if _, ok := clusterv1alpha1.Colors[override]; !ok {
			err := fmt.Errorf("unknown color override value %s", override)
			log.Error(err, err.Error())
			return ctrl.Result{}, err
		} else if eng.IsActive(override) {
			log.Info("Active matches override color, skipping")
			return ctrl.Result{}, nil
		} else {
			log.Info("Backup matches override color, swapping")
			if err := eng.Swap(ctx); err != nil {
				log.Error(err, "Failed to swap")
				eng.SetStatus(ctx, clusterv1alpha1.StatusDeployFailed)
				return ctrl.Result{}, err
			}
		}
		if eng.CurrentStatus() != clusterv1alpha1.StatusOverridden {
			eng.SetStatus(ctx, clusterv1alpha1.StatusOverridden)
		}
		return ctrl.Result{}, nil
	case eng.ActiveMatchesSpec():
		log.Info("No changes were detected, skipping")
		if eng.CurrentStatus() != clusterv1alpha1.StatusNominal {
			eng.SetStatus(ctx, clusterv1alpha1.StatusNominal)
		}
		return ctrl.Result{}, nil
	}
	eng.SetStatus(ctx, clusterv1alpha1.StatusDeploying)
	var prev *v1.PodTemplateSpec
	if eng.BackupMatchesSpec() {
		log.Info("New config matches backup ReplicaSet, swapping")
	} else {
		log.Info("New configuration detected, running deployment")
		if prev, err = eng.UpgradeBackup(ctx); err != nil {
			eng.SetStatus(ctx, clusterv1alpha1.StatusDeployFailed)
			return ctrl.Result{}, errors.Wrap(err, "unable to upgrade ReplicaSet")
		}
	}
	log.Info("Initiating backup tests")
	if err := eng.RunTestsOnBackup(ctx); err != nil {
		log.Error(err, "Failed to test, rolling back")
		// TODO ping slack?
		if prev != nil {
			if _, err := eng.DemoteBackup(ctx, prev); err != nil {
				log.Error(err, "Failed to demote backup to previous state")
			}
		}
		eng.SetStatus(ctx, clusterv1alpha1.StatusDeployFailed)
		return ctrl.Result{}, nil
	}

	if err := eng.Swap(ctx); err != nil {
		eng.SetStatus(ctx, clusterv1alpha1.StatusDeployFailed)
		return ctrl.Result{}, errors.Wrap(err, "failed to swap service")
	}
	eng.SetStatus(ctx, clusterv1alpha1.StatusNominal)
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
