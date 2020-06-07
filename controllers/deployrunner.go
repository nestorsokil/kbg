package controllers

import (
	"context"
	"fmt"
	"github.com/go-logr/logr"
	clusterv1alpha1 "github.com/nestorsokil/kbg/api/v1alpha1"
	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
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
	case podEquals(&activeRs.Spec.Template, &r.deploy.Spec.Template):
		r.Logger.Info("No changes were detected, skipping")
		return nil
	// todo fix this case, gets triggered after regular BG and swaps back
	//case podEquals(&inactiveRs.Spec.Template, &r.deploy.Spec.Template):
	//	// todo swap & scale can be a method
	//	r.Logger.Info("New config matches inactive ReplicaSet, swapping")
	//	if inactiveRs.Spec.Replicas != r.deploy.Spec.Replicas {
	//		inactiveRs.Spec.Replicas = r.deploy.Spec.Replicas
	//		if err := r.Client.Update(ctx, inactiveRs); err != nil {
	//			return errors.Wrap(err, "unable to scale")
	//		}
	//	}
	//	desiredInactive := r.inactiveReplicas(ctx)
	//	if activeRs.Spec.Replicas != desiredInactive {
	//		activeRs.Spec.Replicas = desiredInactive
	//		if err := r.Client.Update(ctx, inactiveRs); err != nil {
	//			return errors.Wrap(err, "unable to scale")
	//		}
	//	}
	//
	//	svc.Spec.Selector[LabelColor] = r.deploy.Status.ActiveColor
	//	if err = r.Client.Update(ctx, svc); err != nil {
	//		return errors.Wrap(err, "unable to swap")
	//	}
	//	return nil
	default:
		r.Logger.Info("New configuration detected, running B/G deployment")
		newRs, err := r.upgradeReplicaSet(ctx, inactiveRs)
		if err != nil {
			return errors.Wrap(err, "unable to upgrade ReplicaSet")
		}

		// TODO initiate smoke tests here

		svc.Spec.Selector[LabelColor] = newRs.Labels[LabelColor]
		if err = r.Client.Update(ctx, svc); err != nil {
			return errors.Wrap(err, "unable to scale inactive")
		}

		inactiveRs = activeRs
		activeRs = newRs

		r.deploy.Status.ActiveColor = newRs.Labels[LabelColor]
		if err := r.Client.Update(ctx, r.deploy); err != nil {
			return errors.Wrap(err, "unable to update deploy status")
		}

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
		if !kuberrors.IsNotFound(err) {
			return nil, err
		}
		svcSpec := deploy.Spec.Service.DeepCopy()
		svcSpec.Selector = map[string]string{LabelColor: clusterv1alpha1.ColorBlue}
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
	if podTemplate.ObjectMeta.Labels == nil {
		podTemplate.ObjectMeta.Labels = make(map[string]string)
	}
	for k, v := range labels {
		podTemplate.ObjectMeta.Labels[k] = v
	}
	replicas := r.desiredReplicas(ctx, color)
	rs := appsv1.ReplicaSet{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ReplicaSet",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      coloredName,
			Namespace: r.deploy.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.ReplicaSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Replicas: replicas,
			Template: podTemplate,
		},
	}
	if err := r.Client.Create(ctx, &rs); err != nil {
		return nil, err
	}
	return &rs, nil
}

func (r *DeployRunner) upgradeReplicaSet(ctx context.Context, rs *appsv1.ReplicaSet) (*appsv1.ReplicaSet, error) {
	color := rs.Labels[LabelColor]
	if err := r.Client.Delete(ctx, rs); err != nil {
		return nil, err
	}
	return r.createRs(ctx, color)
}

func (r *DeployRunner) scaleInactive(ctx context.Context, rs *appsv1.ReplicaSet) error {
	rs.Spec.Replicas = r.inactiveReplicas(ctx)
	return r.Client.Update(ctx, rs)
}

// todo i dont like the signature for these 2
func (r *DeployRunner) desiredReplicas(ctx context.Context, color string) *int32 {
	if color == r.deploy.Status.ActiveColor {
		return r.deploy.Spec.Replicas
	}
	return r.inactiveReplicas(ctx)
}
func (r *DeployRunner) inactiveReplicas(ctx context.Context) *int32 {
	scaleDownPercent := *r.deploy.Spec.BackupScaleDownPercent
	activeReplicas := *r.deploy.Spec.Replicas
	factor := float32(scaleDownPercent) / float32(100.0)
	inactiveReplicas := int32(float32(activeReplicas) * factor)
	return &inactiveReplicas
}

// from github.com/kubernetes/kubernetes/staging/src/k8s.io/kubectl/pkg/util/deployment/deployment.go
func podEquals(template1, template2 *v1.PodTemplateSpec) bool {
	return template1.Spec.Containers[0].Image == template2.Spec.Containers[0].Image
	// todo these are hacks, will figure out later

	//for i := range template1.Spec.Containers {
	//	template1.Spec.Containers[i].TerminationMessagePath = "/dev/termination-log"
	//	template1.Spec.Containers[i].TerminationMessagePolicy = "File"
	//}
	//for i := range template2.Spec.Containers {
	//	template2.Spec.Containers[i].TerminationMessagePath = "/dev/termination-log"
	//	template2.Spec.Containers[i].TerminationMessagePolicy = "File"
	//}
	//return apiequality.Semantic.DeepEqual(template1.Spec.Containers, template2.Spec.Containers)
}
