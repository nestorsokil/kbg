package controllers

import (
	"context"
	"fmt"
	"github.com/go-logr/logr"
	clusterv1alpha1 "github.com/nestorsokil/kbg/api/v1alpha1"
	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	v12 "k8s.io/api/batch/v1"
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

const (
	LabelColor = "kbg/color"
	LabelName  = "kbg/name"
	LabelApp   = "kbg/app"
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
		name:   req.NamespacedName,
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

	if err := engine.ensureServices(ctx); err != nil {
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

// TODO make sure that all public methods are atomic

// DeployEngine is a stateful B/G deployment helper
type DeployEngine struct {
	client.Client
	log logr.Logger

	name types.NamespacedName

	activeReplicasDesired *int32
	backupReplicasDesired *int32

	ActiveSvc *v1.Service
	BackupSvc *v1.Service

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
	// TODO skip if number of pods already == desired
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
	if e.Deploy.Spec.SwapStrategy == clusterv1alpha1.ScaleThenSwap {
		if err := e.Scale(ctx, e.Backup, e.activeReplicasDesired); err != nil {
			return errors.Wrap(err, "failed to scale")
		}
	}

	// TODO retry/rollback?
	e.ActiveSvc.Spec.Selector[LabelColor] = e.Backup.Labels[LabelColor]
	if err := e.Client.Update(ctx, e.ActiveSvc); err != nil {
		return errors.Wrap(err, "unable to Swap")
	}

	e.BackupSvc.Spec.Selector[LabelColor] = e.Active.Labels[LabelColor]
	if err := e.Client.Update(ctx, e.BackupSvc); err != nil {
		return errors.Wrap(err, "unable to Swap")
	}

	temp := e.Active
	e.Active = e.Backup
	e.Backup = temp

	if e.Deploy.Spec.SwapStrategy == clusterv1alpha1.SwapThenScale {
		if err := e.Scale(ctx, e.Active, e.activeReplicasDesired); err != nil {
			return errors.Wrap(err, "failed to scale")
		}
	}

	if e.Backup.Spec.Replicas != e.backupReplicasDesired {
		if err := e.Scale(ctx, e.Backup, e.backupReplicasDesired); err != nil {
			e.log.Error(err, "failed to scale backup")
		}
	}

	if err := e.updateDeployStatus(ctx, func(d *clusterv1alpha1.BlueGreenDeployment) {
		d.Status.ActiveColor = e.Active.Labels[LabelColor]
		d.Status.BackupReplicas = e.Backup.Status.Replicas
		d.Status.ActiveReplicas = e.Active.Status.Replicas
		serviceLabels := e.ActiveSvc.Spec.Selector
		selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: serviceLabels})
		if err != nil {
			e.log.Error(err, "Failed to get active selector")
			return
		}
		d.Status.Selector = selector.String()
	}); err != nil {
		e.log.Error(err, "Failed to update status")
	}

	return nil
}

func (e *DeployEngine) UpgradeBackup(ctx context.Context) (prevSpec *v1.PodTemplateSpec, err error) {
	return e.applyTemplateOnBackup(ctx, &e.Deploy.Spec.Template)
}

func (e *DeployEngine) DemoteBackup(ctx context.Context, desiredState *v1.PodTemplateSpec) (prevSpec *v1.PodTemplateSpec, err error) {
	return e.applyTemplateOnBackup(ctx, desiredState)
}

func (e *DeployEngine) applyTemplateOnBackup(ctx context.Context, template *v1.PodTemplateSpec) (prevSpec *v1.PodTemplateSpec, err error) {
	color := e.Backup.Labels[LabelColor]
	prevSpec = &e.Backup.Spec.Template
	if err := e.Client.Delete(ctx, e.Backup); err != nil {
		return prevSpec, errors.Wrap(err, "failed to destroy stale ReplicaSet")
	}
	e.Backup.Spec.Template = *template

	newRs, err := e.createReplicaSet(ctx, e.backupReplicasDesired, color)
	if err != nil {
		return prevSpec, errors.Wrap(err, "failed to create new ReplicaSet")
	}
	e.Backup = newRs
	if e.Deploy.Status.BackupReplicas != e.Backup.Status.Replicas {
		if err := e.updateDeployStatus(ctx, func(d *clusterv1alpha1.BlueGreenDeployment) {
			d.Status.BackupReplicas = e.Backup.Status.Replicas
		}); err != nil {
			e.log.Error(err, "failed to update status")
		}
	}
	return prevSpec, nil
}

func (e *DeployEngine) RunTestsOnBackup(ctx context.Context) error {
	// probably a shitty check, whatever
	if len(e.Deploy.Spec.TestSpec.Template.Spec.Containers) == 0 {
		e.log.Info("No tests specified, skipping")
		return nil
	}
	jobName := fmt.Sprintf("%s-test-job-%s", e.Deploy.Name, randKey(5))
	jobNamespace := e.Deploy.Namespace
	testJob := v12.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: jobNamespace,
		},
		Spec: e.Deploy.Spec.TestSpec,
	}

	if err := e.Create(ctx, &testJob); err != nil {
		return errors.Wrap(err, "failed to initiate test")
	}
	defer func() {
		deletePolicy := metav1.DeletePropagationBackground // clean up pods in background
		if err := e.Delete(ctx, &testJob, &client.DeleteOptions{
			PropagationPolicy: &deletePolicy,
		}); err != nil {
			e.log.Error(err, "Failed to cleanup job")
		}
	}()
	if err := e.awaitJobCompletion(ctx, &testJob); err != nil {
		return errors.Wrap(err, "tests failed")
	}
	e.log.Info("Tests finished successfully")
	return nil
}

func (e *DeployEngine) awaitJobCompletion(ctx context.Context, job *v12.Job) error {
	key := types.NamespacedName{Namespace: job.Namespace, Name: job.Name}
	timeout := 5 * time.Minute // todo configurable
	for start := time.Now(); time.Since(start) < timeout; {
		<-time.After(10 * time.Second)
		if err := e.Get(ctx, key, job); err != nil {
			return errors.Wrap(err, "failed waiting for job completion")
		}
		for _, cond := range job.Status.Conditions {
			if cond.Type == v12.JobComplete && cond.Status == v1.ConditionTrue {
				return nil
			} else if cond.Type == v12.JobFailed && cond.Status == v1.ConditionTrue {
				return errors.Errorf("job failed: %s", cond.Reason)
			}
		}
	}
	return errors.Errorf("timed out waiting for job completion")
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
	if err := e.Client.Get(ctx, e.name, e.Deploy); err != nil {
		return err
	}
	// TODO these default should be handled by defaulting admission webhook
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

	if e.Deploy.Spec.SwapStrategy == "" || e.Deploy.Spec.BackupScaleDownPercent == nil {
		if err := e.updateDeploy(ctx, func(d *clusterv1alpha1.BlueGreenDeployment) {
			if e.Deploy.Spec.SwapStrategy == "" {
				e.log.Info(fmt.Sprintf("No swap strategy, defaulting to %s", clusterv1alpha1.ScaleThenSwap))
				e.Deploy.Spec.SwapStrategy = clusterv1alpha1.ScaleThenSwap
			}
			if e.Deploy.Spec.BackupScaleDownPercent == nil {
				var defaultScaleDown int32 = 50
				e.log.Info(fmt.Sprintf("No scaleDownPercent, setting to %d", defaultScaleDown))
				e.Deploy.Spec.BackupScaleDownPercent = &defaultScaleDown
			}
		}); err != nil {
			return errors.Wrap(err, "failed to set defaults")
		}
	}
	return nil
}

func (e *DeployEngine) ensureServices(ctx context.Context) error {
	e.ActiveSvc = &v1.Service{}
	activeName := e.Deploy.Name
	activeColor := e.Deploy.Status.ActiveColor
	if err := e.ensureService(ctx, e.ActiveSvc, activeName, activeColor); err != nil {
		return errors.Wrap(err, "failed to ensure active service")
	}

	e.BackupSvc = &v1.Service{}
	backupName := fmt.Sprintf("%s-backup", e.Deploy.Name)
	backupColor := clusterv1alpha1.OppositeColors[e.Deploy.Status.ActiveColor]
	if err := e.ensureService(ctx, e.BackupSvc, backupName, backupColor); err != nil {
		return errors.Wrap(err, "failed to ensure active service")
	}

	if err := e.updateDeployStatus(ctx, func(d *clusterv1alpha1.BlueGreenDeployment) {
		serviceLabels := e.ActiveSvc.Spec.Selector
		selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: serviceLabels})
		if err != nil {
			e.log.Error(err, "Failed to get active selector")
			return
		}
		e.Deploy.Status.Selector = selector.String()
	}); err != nil {
		e.log.Error(err, "Failed to set deployment scale selector")
	}

	return nil
}

func (e *DeployEngine) ensureService(ctx context.Context, target *v1.Service, expectedName, expectedColor string) error {
	key := client.ObjectKey{Namespace: e.Deploy.Namespace, Name: expectedName}
	err := e.Get(ctx, key, target)
	if err == nil {
		if !svcEquals(&target.Spec, &e.Deploy.Spec.Service) {
			e.log.Info("Detected Service change, updating")
			svcCopy := e.ActiveSvc.DeepCopy()
			if svcCopy.Spec.Type != e.Deploy.Spec.Service.Type {
				// TODO handle type change?
			}

			if err := e.updateSvc(ctx, func(svc *v1.Service) {
				svc.Spec = e.Deploy.Spec.Service
				svc.Spec.Selector = target.Spec.Selector
				if svcCopy.Spec.ClusterIP != "" {
					svc.Spec.ClusterIP = svcCopy.Spec.ClusterIP
				}
				// TODO preserve other immutable properties
			}); err != nil {
				return errors.Wrap(err, "failed to update service")
			}
		}
		return nil
	} else if kuberrors.IsNotFound(err) {
		e.log.Info(fmt.Sprintf("Service %s/%s was not found, creating", e.Deploy.Namespace, expectedName))
		svcSpec := e.Deploy.Spec.Service.DeepCopy()
		// TODO maybe selector should be static and just reapply labels on ReplicaSets?
		svcSpec.Selector = map[string]string{
			LabelApp:   e.Deploy.Name,
			LabelColor: expectedColor,
		}
		target = &v1.Service{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Service",
				APIVersion: "v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      expectedName,
				Namespace: e.Deploy.Namespace,
			},
			Spec: *svcSpec,
		}
		if err := e.Client.Create(ctx, target); err != nil {
			return err
		}
		e.log.Info(fmt.Sprintf("New service %s/%s was created", target.Namespace, target.Name))
	} else {
		return errors.Wrap(err, "could not get Svc")
	}
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
		LabelApp:   e.name.Name,
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
	backupName := fmt.Sprintf("%s-backup", name.Name)
	bakServiceKey := client.ObjectKey{Namespace: name.Namespace, Name: backupName}
	if err := e.Client.Get(ctx, bakServiceKey, &svc); err != nil {
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

func (e *DeployEngine) updateDeploy(ctx context.Context, mut func(*clusterv1alpha1.BlueGreenDeployment)) error {
	return errors.Wrap(retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := e.Get(ctx, e.name, e.Deploy); err != nil {
			return errors.Wrap(err, "failed to get deploy with retry")
		}
		mut(e.Deploy)
		return e.Update(ctx, e.Deploy)
	}), "failed to update with retry")
}

func (e *DeployEngine) updateDeployStatus(ctx context.Context, mut func(*clusterv1alpha1.BlueGreenDeployment)) error {
	return errors.Wrap(retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := e.Get(ctx, e.name, e.Deploy); err != nil {
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

func (e *DeployEngine) updateSvc(ctx context.Context, mut func(*v1.Service)) error {
	return errors.Wrap(retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := e.Get(ctx, e.name, e.ActiveSvc); err != nil {
			return errors.Wrap(err, "failed to get deploy with retry")
		}
		mut(e.ActiveSvc)
		return e.Update(ctx, e.ActiveSvc)
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
