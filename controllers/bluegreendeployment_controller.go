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
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
		log.Error(err, "unable to obtain BlueGreenDeployment")
		return ctrl.Result{}, err
	}

	svc, err := r.obtainService(ctx, deploy)
	if err != nil {
		log.Error(err, "unable to obtain Service")
		return ctrl.Result{}, err
	}

	rss, err := r.obtainReplicaSets(ctx, deploy)
	if err != nil {
		log.Error(err, "unable to obtain ReplicaSets")
		return ctrl.Result{}, err
	}

	var (
		activeRs   *appsv1.ReplicaSet
		inactiveRs *appsv1.ReplicaSet
	)
	switch deploy.Status.ActiveColor {
	case clusterv1alpha1.ColorBlue:
		activeRs = rss[clusterv1alpha1.ColorBlue]
		inactiveRs = rss[clusterv1alpha1.ColorGreen]
	default:
		activeRs = rss[clusterv1alpha1.ColorGreen]
		inactiveRs = rss[clusterv1alpha1.ColorBlue]
	}

	if equalIgnoreHash(&activeRs.Spec.Template, &deploy.Spec.Template) {
		// no changes
		return ctrl.Result{}, nil
	}
	if equalIgnoreHash(&inactiveRs.Spec.Template, &deploy.Spec.Template) {
		// scale and swap
		if inactiveRs.Spec.Replicas != deploy.Spec.Replicas {
			inactiveRs.Spec.Replicas = deploy.Spec.Replicas
			if err := r.Client.Update(ctx, inactiveRs); err != nil {
				return ctrl.Result{}, err
			}
		}
		svc.Spec.Selector[LabelColor] = deploy.Status.ActiveColor
		if err = r.Client.Update(ctx, svc); err != nil {
			log.Error(err, "unable to swap Service labels")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, nil
	}

	newRs, err := r.upgradeReplicaSet(ctx, deploy, inactiveRs)
	if err != nil {
		log.Error(err, "unable to upgrade inactive ReplicaSet")
		return ctrl.Result{}, err
	}

	// TODO initiate smoke tests here

	svc.Spec.Selector[LabelColor] = deploy.Status.ActiveColor
	if err = r.Client.Update(ctx, svc); err != nil {
		log.Error(err, "unable to swap Service labels")
		return ctrl.Result{}, err
	}

	inactiveRs = activeRs
	activeRs = newRs

	if err := r.scaleInactive(ctx, deploy, inactiveRs); err != nil {
		log.Error(err, "unable to scale down demoted ReplicaSet")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *BlueGreenDeploymentReconciler) obtainDeployment(ctx context.Context, name types.NamespacedName) (*clusterv1alpha1.BlueGreenDeployment, error) {
	var deploy clusterv1alpha1.BlueGreenDeployment
	if err := r.Get(ctx, name, &deploy); err != nil {
		return nil, err
	}
	if deploy.Status.ActiveColor == "" {
		// todo update?
		deploy.Status.ActiveColor = clusterv1alpha1.ColorBlue
	}
	return &deploy, nil
}

func (r *BlueGreenDeploymentReconciler) obtainService(ctx context.Context, deploy *clusterv1alpha1.BlueGreenDeployment) (*v1.Service, error) {
	var svc v1.Service
	if err := r.Get(ctx, client.ObjectKey{Namespace: deploy.Namespace, Name: deploy.Name}, &svc); err != nil {
		if errors.IsNotFound(err) {
			svcSpec := deploy.Spec.Service.DeepCopy()
			svcSpec.Selector = map[string]string{"color": clusterv1alpha1.ColorBlue}
			svc = v1.Service{
				TypeMeta: metav1.TypeMeta{
					Kind:       "Service",
					APIVersion: "v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploy.Name,
					Namespace: deploy.Namespace,
				},
				Spec: *svcSpec,
			}
			if err := r.Client.Create(ctx, &svc); err != nil {
				return nil, err
			}
		}
		return nil, err
	}
	return &svc, nil
}

func (r *BlueGreenDeploymentReconciler) obtainReplicaSets(ctx context.Context, deploy *clusterv1alpha1.BlueGreenDeployment) (map[string]*appsv1.ReplicaSet, error) {
	rss := make(map[string]*appsv1.ReplicaSet, 2)
	for _, color := range []string{clusterv1alpha1.ColorBlue, clusterv1alpha1.ColorGreen} {
		rs, err := r.getOrCreateRs(ctx, deploy, color)
		if err != nil {
			return nil, err
		}
		rss[color] = rs
	}
	return rss, nil
}

func (r *BlueGreenDeploymentReconciler) getOrCreateRs(ctx context.Context, deploy *clusterv1alpha1.BlueGreenDeployment, color string) (*appsv1.ReplicaSet, error) {
	var rs appsv1.ReplicaSet
	coloredName := fmt.Sprintf("%s-%s", deploy.Name, color)
	namespacedName := client.ObjectKey{Namespace: deploy.Namespace, Name: coloredName}
	if err := r.Get(ctx, namespacedName, &rs); err != nil {
		if errors.IsNotFound(err) {
			return r.createRs(ctx, deploy, color)
		}
		return nil, err
	}
	return &rs, nil
}

func (r *BlueGreenDeploymentReconciler) createRs(ctx context.Context, deploy *clusterv1alpha1.BlueGreenDeployment, color string) (*appsv1.ReplicaSet, error) {
	coloredName := fmt.Sprintf("%s-%s", deploy.Name, color)
	labels := map[string]string{
		LabelName:  coloredName,
		LabelColor: color,
	}
	podTemplate := deploy.Spec.Template
	for k, v := range labels {
		podTemplate.ObjectMeta.Labels[k] = v
	}
	rs := appsv1.ReplicaSet{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ReplicaSet",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      coloredName,
			Namespace: deploy.Namespace,
		},
		Spec: appsv1.ReplicaSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Replicas: deploy.Spec.Replicas,
			Template: podTemplate,
		},
	}
	if err := r.Client.Create(ctx, &rs); err != nil {
		return nil, err
	}
	return &rs, nil
}

func (r *BlueGreenDeploymentReconciler) upgradeReplicaSet(ctx context.Context, deploy *clusterv1alpha1.BlueGreenDeployment, rs *appsv1.ReplicaSet) (*appsv1.ReplicaSet, error) {
	if err := r.Client.Delete(ctx, rs); err != nil {
		return nil, err
	}
	return r.createRs(ctx, deploy, rs.Labels[LabelColor])
}

func (r *BlueGreenDeploymentReconciler) scaleInactive(ctx context.Context, deploy *clusterv1alpha1.BlueGreenDeployment, rs *appsv1.ReplicaSet) error {
	var scaleDownPercent int32 = 50 // default
	if deploy.Spec.BackupScaleDownPercent != nil {
		scaleDownPercent = *deploy.Spec.BackupScaleDownPercent
	}
	activeReplicas := *deploy.Spec.Replicas
	inactiveReplicas := activeReplicas * (scaleDownPercent / 100.0)
	rs.Spec.Replicas = &inactiveReplicas

	return r.Client.Update(ctx, rs)
}

func (r *BlueGreenDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1alpha1.BlueGreenDeployment{}).
		Complete(r)
}

// from github.com/kubernetes/kubernetes/staging/src/k8s.io/kubectl/pkg/util/deployment/deployment.go
func equalIgnoreHash(template1, template2 *v1.PodTemplateSpec) bool {
	t1Copy := template1.DeepCopy()
	t2Copy := template2.DeepCopy()
	// Remove hash labels from template.Labels before comparing
	delete(t1Copy.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
	delete(t2Copy.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
	return apiequality.Semantic.DeepEqual(t1Copy, t2Copy)
}
