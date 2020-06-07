package controllers

import (
	"context"
	"fmt"
	"github.com/go-logr/logr"
	clusterv1alpha1 "github.com/nestorsokil/kbg/api/v1alpha1"
	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	kuberrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func Runner(log logr.Logger, client client.Client, deploy *clusterv1alpha1.BlueGreenDeployment) *DeployRunner {
	return &DeployRunner{Client: client, Logger: log, deploy: deploy}
}

// DeployRunner is a stateful B/G deployment helper
type DeployRunner struct {
	client.Client
	logr.Logger

	deploy *clusterv1alpha1.BlueGreenDeployment
}

func (r *DeployRunner) Run(ctx context.Context) error {
	svc, err := r.obtainService(ctx)
	if err != nil {
		return errors.Wrap(err, "unable to obtain Service")
	}
	activeRs, inactiveRs, err := r.obtainReplicaSets(ctx)
	if err != nil {
		return errors.Wrap(err, "unable to obtain ReplicaSets")
	}
	switch {
	case equalIgnoreHash(&activeRs.Spec.Template, &r.deploy.Spec.Template):
		r.Logger.Info("No changes were detected, skipping")
		return nil
	case equalIgnoreHash(&inactiveRs.Spec.Template, &r.deploy.Spec.Template):
		r.Logger.Info("New config matches inactive ReplicaSet, swapping")
		if inactiveRs.Spec.Replicas != r.deploy.Spec.Replicas {
			inactiveRs.Spec.Replicas = r.deploy.Spec.Replicas
			if err := r.Client.Update(ctx, inactiveRs); err != nil {
				return errors.Wrap(err, "unable to scale")
			}
		}
		svc.Spec.Selector[LabelColor] = r.deploy.Status.ActiveColor
		if err = r.Client.Update(ctx, svc); err != nil {
			return errors.Wrap(err, "unable to swap")
		}
		return nil
	default:
		r.Logger.Info("New configuration detected, running B/G deployment")
		newRs, err := r.upgradeReplicaSet(ctx, inactiveRs)
		if err != nil {
			return errors.Wrap(err, "unable to upgrade ReplicaSet")
		}

		// TODO initiate smoke tests here

		svc.Spec.Selector[LabelColor] = r.deploy.Status.ActiveColor
		if err = r.Client.Update(ctx, svc); err != nil {
			return errors.Wrap(err, "unable to scale inactive")
		}

		inactiveRs = activeRs
		activeRs = newRs

		if err := r.scaleInactive(ctx, inactiveRs); err != nil {
			// todo should this be non crit?
			return errors.Wrap(err, "unable to scale inactive")
		}
		return nil
	}
}

func (r *DeployRunner) obtainService(ctx context.Context) (*v1.Service, error) {
	var deploy = r.deploy
	var svc v1.Service
	if err := r.Get(ctx, client.ObjectKey{Namespace: deploy.Namespace, Name: deploy.Name}, &svc); err != nil {
		if kuberrors.IsNotFound(err) {
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

func (r *DeployRunner) obtainReplicaSets(ctx context.Context) (active, inactive *appsv1.ReplicaSet, err error) {
	for _, color := range []string{clusterv1alpha1.ColorBlue, clusterv1alpha1.ColorGreen} {
		rs, err := r.getOrCreateRs(ctx, color)
		if err != nil {
			return nil, nil, err
		}
		if r.deploy.Status.ActiveColor == color {
			active = rs
		} else {
			inactive = rs
		}
	}
	return active, inactive, nil
}

func (r *DeployRunner) getOrCreateRs(ctx context.Context, color string) (*appsv1.ReplicaSet, error) {
	var rs appsv1.ReplicaSet
	coloredName := fmt.Sprintf("%s-%s", r.deploy.Name, color)
	namespacedName := client.ObjectKey{Namespace: r.deploy.Namespace, Name: coloredName}
	if err := r.Get(ctx, namespacedName, &rs); err != nil {
		if kuberrors.IsNotFound(err) {
			return r.createRs(ctx, color)
		}
		return nil, err
	}
	return &rs, nil
}

func (r *DeployRunner) createRs(ctx context.Context, color string) (*appsv1.ReplicaSet, error) {
	coloredName := fmt.Sprintf("%s-%s", r.deploy.Name, color)
	labels := map[string]string{
		LabelName:  coloredName,
		LabelColor: color,
	}
	podTemplate := r.deploy.Spec.Template
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
			Namespace: r.deploy.Namespace,
		},
		Spec: appsv1.ReplicaSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Replicas: r.deploy.Spec.Replicas,
			Template: podTemplate,
		},
	}
	if err := r.Client.Create(ctx, &rs); err != nil {
		return nil, err
	}
	return &rs, nil
}

func (r *DeployRunner) upgradeReplicaSet(ctx context.Context, rs *appsv1.ReplicaSet) (*appsv1.ReplicaSet, error) {
	if err := r.Client.Delete(ctx, rs); err != nil {
		return nil, err
	}
	return r.createRs(ctx, rs.Labels[LabelColor])
}

func (r *DeployRunner) scaleInactive(ctx context.Context, rs *appsv1.ReplicaSet) error {
	scaleDownPercent := *r.deploy.Spec.BackupScaleDownPercent
	activeReplicas := *r.deploy.Spec.Replicas
	inactiveReplicas := activeReplicas * (scaleDownPercent / 100.0)
	rs.Spec.Replicas = &inactiveReplicas

	return r.Client.Update(ctx, rs)
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
