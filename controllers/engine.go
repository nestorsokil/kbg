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
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sync"
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
		Client:     client,
		log:        log,
		deployName: req.NamespacedName,
	}
	if err := engine.getDeployment(ctx); err != nil {
		if kuberrors.IsNotFound(err) {
			log.Info(fmt.Sprintf("Deployment %s was deleted, cleaning up", req.NamespacedName))
			engine.cleanup(ctx, req.NamespacedName)
			return nil, ErrDeleted
		}

		return nil, errors.Wrap(err, "failed to obtain deployment")
	}

	engine.activeReplicasDesired = engine.Deploy.Spec.Replicas
	backupReplicas := computeDesiredBackup(*engine.activeReplicasDesired, *engine.Deploy.Spec.BackupScaleDownPercent)
	engine.backupReplicasDesired = &backupReplicas

	if err := engine.ensureService(ctx); err != nil {
		return nil, err
	}
	if err := engine.ensureReplicaSets(ctx); err != nil {
		return nil, err
	}
	if err := engine.updateDeployStatus(ctx, func(d *clusterv1alpha1.BlueGreenDeployment) {
		d.Status.ActiveReplicas = engine.Active.Status.Replicas
		d.Status.BackupReplicas = engine.Backup.Status.Replicas
	}); err != nil {
		log.Error(err, "Failed to align status")
	}

	return engine, nil
}

// DeployEngine is a stateful B/G deployment helper
type DeployEngine struct {
	client.Client
	log logr.Logger

	deployName types.NamespacedName

	activeReplicasDesired *int32
	backupReplicasDesired *int32

	Svc    *v1.Service
	Active *appsv1.ReplicaSet
	Backup *appsv1.ReplicaSet
	Deploy *clusterv1alpha1.BlueGreenDeployment
}

// ActiveMatchesSpec returns true when active ReplicaSet matches desired Spec
func (e *DeployEngine) ActiveMatchesSpec() bool {
	return podEquals(&e.Active.Spec.Template, &e.Deploy.Spec.Template)
}

// ActiveMatchesSpec returns true when backup ReplicaSet matches desired Spec
func (e *DeployEngine) BackupMatchesSpec() bool {
	return podEquals(&e.Backup.Spec.Template, &e.Deploy.Spec.Template)
}

func (e *DeployEngine) OverrideColor() *string {
	return e.Deploy.Spec.OverrideColor
}

func (e *DeployEngine) CurrentStatus() string {
	return e.Deploy.Status.StatusName
}

func (e *DeployEngine) IsActive(color string) bool {
	return e.Active.Labels[LabelColor] == color
}

func (e *DeployEngine) Scale(ctx context.Context, rs *appsv1.ReplicaSet, desired *int32) error {
	if err := e.updateReplicaSet(ctx, rs, func(rs *appsv1.ReplicaSet) {
		rs.Spec.Replicas = desired
	}); err != nil {
		return errors.Wrap(err, "failed to scale replica set")
	}
	if err := e.awaitAllPods(ctx, rs); err != nil {
		e.log.Error(err, "failed awaiting pod availability")
	}
	return nil
}

func (e *DeployEngine) Swap(ctx context.Context) error {
	if err := e.Scale(ctx, e.Backup, e.activeReplicasDesired); err != nil {
		return errors.Wrap(err, "failed to scale")
	}
	e.Svc.Spec.Selector[LabelColor] = e.Backup.Labels[LabelColor]
	if err := e.Client.Update(ctx, e.Svc); err != nil {
		return errors.Wrap(err, "unable to Swap")
	}

	temp := e.Active
	e.Active = e.Backup
	e.Backup = temp

	if e.Backup.Spec.Replicas != e.backupReplicasDesired {
		if err := e.Scale(ctx, e.Backup, e.backupReplicasDesired); err != nil {
			e.log.Error(err, "failed to scale backup")
		}
	}

	if err := e.updateDeployStatus(ctx, func(d *clusterv1alpha1.BlueGreenDeployment) {
		d.Status.ActiveColor = e.Active.Labels[LabelColor]
		d.Status.BackupReplicas = e.Backup.Status.Replicas
		d.Status.ActiveReplicas = e.Active.Status.Replicas
	}); err != nil {
		e.log.Error(err, "Failed to update status")
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

	if e.Deploy.Status.BackupReplicas != e.Backup.Status.Replicas {
		if err := e.updateDeployStatus(ctx, func(d *clusterv1alpha1.BlueGreenDeployment) {
			d.Status.BackupReplicas = e.Backup.Status.Replicas
		}); err != nil {
			e.log.Error(err, "failed to update status")
		}
	}

	return nil
}

func (e *DeployEngine) SetStatus(ctx context.Context, status string) {
	if err := e.updateDeployStatus(ctx, func(d *clusterv1alpha1.BlueGreenDeployment) {
		d.Status.StatusName = status
	}); err != nil {
		e.log.Error(err, "Failed to update status")
	}
}

func (e *DeployEngine) getDeployment(ctx context.Context) error {
	e.Deploy = &clusterv1alpha1.BlueGreenDeployment{}
	if err := e.Client.Get(ctx, e.deployName, e.Deploy); err != nil {
		return err
	}
	if e.Deploy.Status.ActiveColor == "" || e.Deploy.Status.StatusName == "" {
		if err := e.updateDeployStatus(ctx, func(d *clusterv1alpha1.BlueGreenDeployment) {
			if d.Status.ActiveColor == "" {
				e.log.Info("No color set for deployment, updating")
				d.Status.ActiveColor = clusterv1alpha1.ColorBlue
			}
			if d.Status.StatusName == "" {
				e.log.Info("No status set for deployment, updating")
				d.Status.StatusName = clusterv1alpha1.StatusUnknown
			}
		}); err != nil {
			return errors.Wrap(err, "failed to set status")
		}
	}
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
	var wg sync.WaitGroup
	errs := make(chan error)
	done := make(chan struct{})
	// create blue & green RS in parallel
	for color := range clusterv1alpha1.Colors {
		wg.Add(1)
		go func(color string) {
			defer wg.Done()
			if rs, err := e.obtainReplicaSet(ctx, color); err != nil {
				errs <- err
				return
			} else {
				if e.Deploy.Status.ActiveColor == color {
					e.Active = rs
				} else {
					e.Backup = rs
				}
			}
		}(color)
	}
	go func() { wg.Wait(); close(done) }()
	select {
	case err := <-errs:
		return errors.Wrap(err, "failed obtaining replica set")
	case <-done:
		return nil
	}
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
		e.log.Error(err, "Failed waiting for all replicas")
	}
	e.log.Info(fmt.Sprintf("Created ReplicaSet %s/%s", rs.Namespace, rs.Name))
	return &rs, nil
}

func (e *DeployEngine) cleanup(ctx context.Context, name types.NamespacedName) {
	var svc v1.Service
	if err := e.Client.Get(ctx, name, &svc); err != nil {
		e.log.Error(err, "Failed to lookup service")
	} else if err := e.Client.Delete(ctx, &svc); err != nil {
		e.log.Error(err, "Failed to delete service")
	}
	for color := range clusterv1alpha1.Colors {
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

func (e *DeployEngine) updateDeployStatus(ctx context.Context, mut func(*clusterv1alpha1.BlueGreenDeployment)) error {
	return errors.Wrap(retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := e.Get(ctx, e.deployName, e.Deploy); err != nil {
			return errors.Wrap(err, "failed to get deploy with retry")
		}
		mut(e.Deploy)
		return e.Status().Update(ctx, e.Deploy)
	}), "failed to update with retry")
}

func (e *DeployEngine) updateReplicaSet(ctx context.Context, r *appsv1.ReplicaSet, mut func(*appsv1.ReplicaSet)) error {
	return errors.Wrap(retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		key := client.ObjectKey{Namespace: r.Namespace, Name: r.Name}
		if err := retry.OnError(retry.DefaultRetry, kuberrors.IsNotFound, func() error {
			return e.Get(ctx, key, r)
		}); err != nil {
			return errors.Wrap(err, "failed to get replica set with retry")
		}
		mut(r)
		return e.Update(ctx, r)
	}), "failed to update with retry")
}

func (e *DeployEngine) awaitAllPods(ctx context.Context, replicaSet *appsv1.ReplicaSet) error {
	namespacedName := client.ObjectKey{Namespace: replicaSet.Namespace, Name: replicaSet.Name}
	expectedGeneration := replicaSet.Generation
	expectedReplicas := *replicaSet.Spec.Replicas
	return wait.PollImmediate(100*time.Millisecond, 30*time.Second, func() (bool, error) {
		if err := retry.OnError(retry.DefaultRetry, kuberrors.IsNotFound, func() error {
			return e.Client.Get(ctx, namespacedName, replicaSet)
		}); err != nil {
			return false, errors.Wrap(err, "failed to get ReplicaSet")
		}
		sameGeneration := replicaSet.Status.ObservedGeneration >= expectedGeneration
		samePodsNum := replicaSet.Status.AvailableReplicas == expectedReplicas
		return sameGeneration && samePodsNum, nil
	})
}

func computeDesiredBackup(desiredActive, percent int32) int32 {
	scaleDownPercent := percent
	factor := float32(scaleDownPercent) / float32(100.0)
	return int32(float32(desiredActive) * factor)
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
