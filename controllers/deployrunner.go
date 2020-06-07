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
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"time"
)

func NewRunner(
	ctx context.Context,
	log logr.Logger,
	client client.Client,
	deploy *clusterv1alpha1.BlueGreenDeployment,
) (*DeployRunner, error) {
	activeReplicas := *deploy.Spec.Replicas
	scaleDownPercent := *deploy.Spec.BackupScaleDownPercent
	factor := float32(scaleDownPercent) / float32(100.0)
	backupReplicas := int32(float32(activeReplicas) * factor)
	runner := &DeployRunner{
		Client:                client,
		log:                   log,
		deploy:                deploy,
		activeReplicasDesired: &activeReplicas,
		backupReplicasDesired: &backupReplicas,
	}
	if err := runner.ensureService(ctx); err != nil {
		return nil, err
	}
	if err := runner.ensureReplicaSets(ctx); err != nil {
		return nil, err
	}
	return runner, nil
}

// DeployRunner is a stateful B/G deployment helper
type DeployRunner struct {
	client.Client
	log    logr.Logger
	deploy *clusterv1alpha1.BlueGreenDeployment

	activeReplicasDesired *int32
	backupReplicasDesired *int32

	Svc    *v1.Service
	Active *appsv1.ReplicaSet
	Backup *appsv1.ReplicaSet
}

// ActiveMatches returns true when active ReplicaSet matches desired Spec
func (r *DeployRunner) ActiveMatches() bool {
	return podEquals(&r.Active.Spec.Template, &r.deploy.Spec.Template)
}

// ActiveMatches returns true when backup ReplicaSet matches desired Spec
func (r *DeployRunner) BackupMatches() bool {
	return podEquals(&r.Backup.Spec.Template, &r.deploy.Spec.Template)
}

func (r *DeployRunner) Scale(ctx context.Context, rs *appsv1.ReplicaSet, desired *int32) error {
	rs.Spec.Replicas = desired
	if err := r.Client.Update(ctx, rs); err != nil {
		return errors.Wrap(err, "unable to scale")
	}
	return nil
}

func (r *DeployRunner) Swap(ctx context.Context) error {
	newColor := clusterv1alpha1.ColorBlue
	if r.Svc.Labels[LabelColor] == newColor {
		newColor = clusterv1alpha1.ColorGreen
	}
	r.Svc.Labels[LabelColor] = newColor

	if err := r.Client.Update(ctx, r.Svc); err != nil {
		return errors.Wrap(err, "unable to Swap")
	}

	r.deploy.Status.ActiveColor = r.Active.Labels[LabelColor]
	if err := r.Client.Status().Update(ctx, r.deploy); err != nil {
		return errors.Wrap(err, "unable to update deploy status")
	}
	if r.Active.Spec.Replicas != r.backupReplicasDesired {
		if err := r.Scale(ctx, r.Active, r.backupReplicasDesired); err != nil {
			return errors.Wrap(err, "unable to scale")
		}
	}
	return nil
}

func (r *DeployRunner) UpgradeBackup(ctx context.Context) error {
	color := r.Backup.Labels[LabelColor]
	if err := r.Client.Delete(ctx, r.Backup); err != nil {
		return errors.Wrap(err, "failed to destroy stale ReplicaSet")
	}
	newRs, err := r.createReplicaSet(ctx, r.deploy.Spec.Replicas, color)
	if err != nil {
		return errors.Wrap(err, "failed to create new ReplicaSet")
	}
	r.Backup = newRs
	return nil
}

func (r *DeployRunner) ensureService(ctx context.Context) error {
	var deploy = r.deploy
	if err := r.Get(ctx, client.ObjectKey{Namespace: deploy.Namespace, Name: deploy.Name}, r.Svc); err != nil {
		if !kuberrors.IsNotFound(err) {
			return errors.Wrap(err, "could not get Svc")
		}
		r.log.Info("Service was not found, creating")
		svcSpec := deploy.Spec.Service.DeepCopy()
		svcSpec.Selector = map[string]string{LabelColor: clusterv1alpha1.ColorBlue}
		r.Svc = &v1.Service{
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
		if err := r.Client.Create(ctx, r.Svc); err != nil {
			return err
		}
		r.log.Info(fmt.Sprintf("New service %s/%s was created", r.Svc.Namespace, r.Svc.Name))
	}
	return nil
}

func (r *DeployRunner) ensureReplicaSets(ctx context.Context) error {
	for _, color := range []string{clusterv1alpha1.ColorBlue, clusterv1alpha1.ColorGreen} {
		if rs, err := r.obtainReplicaSet(ctx, color); err != nil {
			return err
		} else {
			if r.deploy.Status.ActiveColor == color {
				r.Active = rs
			} else {
				r.Backup = rs
			}
		}
	}
	return nil
}

func (r *DeployRunner) obtainReplicaSet(ctx context.Context, color string) (*appsv1.ReplicaSet, error) {
	var rs appsv1.ReplicaSet
	coloredName := fmt.Sprintf("%s-%s", r.deploy.Name, color)
	namespacedName := client.ObjectKey{Namespace: r.deploy.Namespace, Name: coloredName}
	if err := r.Get(ctx, namespacedName, &rs); err != nil {
		if !kuberrors.IsNotFound(err) {
			return nil, err
		}
		r.log.Info(fmt.Sprintf("ReplicaSet %s was not found, creating", namespacedName))
		var replicas *int32
		if color == r.deploy.Status.ActiveColor {
			replicas = r.activeReplicasDesired
		}
		replicas = r.backupReplicasDesired
		return r.createReplicaSet(ctx, replicas, color)
	}
	return &rs, nil
}

func (r *DeployRunner) createReplicaSet(ctx context.Context, replicas *int32, color string) (*appsv1.ReplicaSet, error) {
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
	if err := r.awaitAllPods(ctx, &rs); err != nil {
		r.log.Error(err, "Failed waiting for all replicas")
	}
	r.log.Info(fmt.Sprintf("Created ReplicaSet %s/%s", rs.Namespace, rs.Name))
	return &rs, nil
}

func (r *DeployRunner) awaitAllPods(ctx context.Context, replicaSet *appsv1.ReplicaSet) error {
	namespacedName := client.ObjectKey{Namespace: replicaSet.Namespace, Name: replicaSet.Name}
	return wait.PollImmediate(100*time.Millisecond, 30*time.Second, func() (bool, error) {
		var rs appsv1.ReplicaSet
		err := r.Client.Get(ctx, namespacedName, &rs)
		if err != nil {
			return false, errors.Wrap(err, "failed to get ReplicaSet")
		}
		sameGeneration := rs.Status.ObservedGeneration >= replicaSet.Generation
		samePodsNum := rs.Status.AvailableReplicas == *replicaSet.Spec.Replicas
		return sameGeneration && samePodsNum, nil
	})
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
