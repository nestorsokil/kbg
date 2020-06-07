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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"time"
)

var (
	ErrDeleted = errors.New("deployment was deleted")
)

func NewEngine(
	ctx context.Context,
	log logr.Logger,
	client client.Client,
	req ctrl.Request,
) (*DeployEngine, error) {
	engine := &DeployEngine{
		Client: client,
		log:    log,
	}
	if err := engine.ensureDeployment(ctx, req.NamespacedName); err != nil {
		if kuberrors.IsNotFound(err) {
			log.Info(fmt.Sprintf("Deployment %s was deleted, cleaning up", req.NamespacedName))
			engine.cleanup(ctx, req.NamespacedName)
			return nil, ErrDeleted
		}

		return nil, errors.Wrap(err, "failed to obtain deployment")
	}

	activeReplicas := *engine.Deploy.Spec.Replicas
	scaleDownPercent := *engine.Deploy.Spec.BackupScaleDownPercent
	factor := float32(scaleDownPercent) / float32(100.0)
	backupReplicas := int32(float32(activeReplicas) * factor)

	engine.activeReplicasDesired = &activeReplicas
	engine.backupReplicasDesired = &backupReplicas

	if err := engine.ensureService(ctx); err != nil {
		return nil, err
	}
	if err := engine.ensureReplicaSets(ctx); err != nil {
		return nil, err
	}
	return engine, nil
}

// DeployEngine is a stateful B/G deployment helper
type DeployEngine struct {
	client.Client
	log logr.Logger

	activeReplicasDesired *int32
	backupReplicasDesired *int32

	Svc    *v1.Service
	Active *appsv1.ReplicaSet
	Backup *appsv1.ReplicaSet
	Deploy *clusterv1alpha1.BlueGreenDeployment
}

// ActiveMatches returns true when active ReplicaSet matches desired Spec
func (e *DeployEngine) ActiveMatches() bool {
	return podEquals(&e.Active.Spec.Template, &e.Deploy.Spec.Template)
}

// ActiveMatches returns true when backup ReplicaSet matches desired Spec
func (e *DeployEngine) BackupMatches() bool {
	return podEquals(&e.Backup.Spec.Template, &e.Deploy.Spec.Template)
}

func (e *DeployEngine) Scale(ctx context.Context, rs *appsv1.ReplicaSet, desired *int32) error {
	rs.Spec.Replicas = desired
	if err := e.Client.Update(ctx, rs); err != nil {
		return errors.Wrap(err, "unable to scale")
	}
	return nil
}

func (e *DeployEngine) Swap(ctx context.Context) error {
	newColor := clusterv1alpha1.ColorBlue
	if e.Svc.Spec.Selector[LabelColor] == newColor {
		newColor = clusterv1alpha1.ColorGreen
	}
	e.Svc.Spec.Selector[LabelColor] = newColor

	if err := e.Client.Update(ctx, e.Svc); err != nil {
		return errors.Wrap(err, "unable to Swap")
	}

	e.Deploy.Status.ActiveColor = e.Active.Labels[LabelColor]
	if err := e.Client.Status().Update(ctx, e.Deploy); err != nil {
		return errors.Wrap(err, "unable to update deploy status")
	}
	if e.Active.Spec.Replicas != e.backupReplicasDesired {
		if err := e.Scale(ctx, e.Active, e.backupReplicasDesired); err != nil {
			return errors.Wrap(err, "unable to scale")
		}
	}
	return nil
}

func (e *DeployEngine) UpgradeBackup(ctx context.Context) error {
	color := e.Backup.Labels[LabelColor]
	if err := e.Client.Delete(ctx, e.Backup); err != nil {
		return errors.Wrap(err, "failed to destroy stale ReplicaSet")
	}
	newRs, err := e.createReplicaSet(ctx, e.Deploy.Spec.Replicas, color)
	if err != nil {
		return errors.Wrap(err, "failed to create new ReplicaSet")
	}
	e.Backup = newRs
	return nil
}

func (e *DeployEngine) ensureDeployment(ctx context.Context, name types.NamespacedName) error {
	var deploy clusterv1alpha1.BlueGreenDeployment
	if err := e.Client.Get(ctx, name, &deploy); err != nil {
		return err
	}
	if deploy.Status.ActiveColor == "" {
		e.log.Info("No color set for deployment, updating")
		deploy.Status.ActiveColor = clusterv1alpha1.ColorBlue
		if err := e.Client.Status().Update(ctx, &deploy); err != nil {
			return errors.Wrap(err, "failed to set color")
		}
	}
	e.Deploy = &deploy
	return nil
}

func (e *DeployEngine) ensureService(ctx context.Context) error {
	var svc v1.Service
	key := client.ObjectKey{Namespace: e.Deploy.Namespace, Name: e.Deploy.Name}
	if err := e.Get(ctx, key, &svc); err != nil {
		if !kuberrors.IsNotFound(err) {
			return errors.Wrap(err, "could not get Svc")
		}
		e.log.Info("Service was not found, creating")
		svcSpec := e.Deploy.Spec.Service.DeepCopy()
		svcSpec.Selector = map[string]string{LabelColor: clusterv1alpha1.ColorBlue}
		svc = v1.Service{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Service",
				APIVersion: "v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      e.Deploy.Name,
				Namespace: e.Deploy.Namespace,
			},
			Spec: *svcSpec,
		}
		if err := e.Client.Create(ctx, &svc); err != nil {
			return err
		}
		e.log.Info(fmt.Sprintf("New service %s/%s was created", svc.Namespace, svc.Name))
	}
	e.Svc = &svc
	return nil
}

func (e *DeployEngine) ensureReplicaSets(ctx context.Context) error {
	for _, color := range clusterv1alpha1.Colors {
		if rs, err := e.obtainReplicaSet(ctx, color); err != nil {
			return err
		} else {
			if e.Deploy.Status.ActiveColor == color {
				e.Active = rs
			} else {
				e.Backup = rs
			}
		}
	}
	return nil
}

func (e *DeployEngine) obtainReplicaSet(ctx context.Context, color string) (*appsv1.ReplicaSet, error) {
	var rs appsv1.ReplicaSet
	coloredName := fmt.Sprintf("%s-%s", e.Deploy.Name, color)
	namespacedName := client.ObjectKey{Namespace: e.Deploy.Namespace, Name: coloredName}
	if err := e.Get(ctx, namespacedName, &rs); err != nil {
		if !kuberrors.IsNotFound(err) {
			return nil, err
		}
		e.log.Info(fmt.Sprintf("ReplicaSet %s was not found, creating", namespacedName))
		var replicas *int32
		if color == e.Deploy.Status.ActiveColor {
			replicas = e.activeReplicasDesired
		} else {
			replicas = e.backupReplicasDesired
		}
		return e.createReplicaSet(ctx, replicas, color)
	}
	return &rs, nil
}

func (e *DeployEngine) createReplicaSet(ctx context.Context, replicas *int32, color string) (*appsv1.ReplicaSet, error) {
	coloredName := fmt.Sprintf("%s-%s", e.Deploy.Name, color)
	labels := map[string]string{
		LabelName:  coloredName,
		LabelColor: color,
	}
	podTemplate := e.Deploy.Spec.Template
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
			Namespace: e.Deploy.Namespace,
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
	if err := e.Client.Create(ctx, &rs); err != nil {
		return nil, err
	}
	if err := e.awaitAllPods(ctx, &rs); err != nil {
		// todo this happened a couple of times, need to fix
		//ERROR   controllers.BlueGreenDeployment Failed waiting for all replicas {"bluegreendeployment":
		//"test/myserver", "error": "failed to get ReplicaSet: ReplicaSet.apps \"myserver-green\" not found",
		//"errorVerbose": "ReplicaSet.apps \"myserver-green\" not found\nfailed to get ReplicaSet
		e.log.Error(err, "Failed waiting for all replicas")
	}
	e.log.Info(fmt.Sprintf("Created ReplicaSet %s/%s", rs.Namespace, rs.Name))
	return &rs, nil
}

// meh, hacks
func (e *DeployEngine) cleanup(ctx context.Context, name types.NamespacedName) {
	var svc v1.Service
	if err := e.Client.Get(ctx, name, &svc); err != nil {
		e.log.Error(err, "Failed to lookup service")
	} else if err := e.Client.Delete(ctx, &svc); err != nil {
		e.log.Error(err, "Failed to delete service")
	}
	for _, color := range clusterv1alpha1.Colors {
		var rs appsv1.ReplicaSet
		coloredName := fmt.Sprintf("%s-%s", name.Name, color)
		namespacedName := client.ObjectKey{Namespace: name.Namespace, Name: coloredName}
		if err := e.Client.Get(ctx, namespacedName, &rs); err != nil {
			e.log.Error(err, "Failed to lookup replicaset")
		} else if err := e.Client.Delete(ctx, &rs); err != nil {
			e.log.Error(err, "Failed to delete replicaset")
		}
	}
}

func (e *DeployEngine) awaitAllPods(ctx context.Context, replicaSet *appsv1.ReplicaSet) error {
	namespacedName := client.ObjectKey{Namespace: replicaSet.Namespace, Name: replicaSet.Name}
	return wait.PollImmediate(100*time.Millisecond, 30*time.Second, func() (bool, error) {
		var rs appsv1.ReplicaSet
		err := e.Client.Get(ctx, namespacedName, &rs)
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
