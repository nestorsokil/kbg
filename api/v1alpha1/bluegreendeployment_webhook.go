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

package v1alpha1

import (
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

// log is for logging in this package.
var bluegreendeploymentlog = logf.Log.WithName("bluegreendeployment-resource")

func (r *BlueGreenDeployment) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(r).
		Complete()
}

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!

// +kubebuilder:webhook:path=/mutate-cluster-kbg-v1alpha1-bluegreendeployment,mutating=true,failurePolicy=fail,groups=cluster.kbg,resources=bluegreendeployments,verbs=create;update,versions=v1alpha1,name=mbluegreendeployment.kb.io

var _ webhook.Defaulter = &BlueGreenDeployment{}

var (
	DefaultReplicas         int32 = 1
	DefaultScaleDownPercent int32 = 50
)

// Default implements webhook.Defaulter so a webhook will be registered for the type
func (r *BlueGreenDeployment) Default() {
	bluegreendeploymentlog.Info("default", "name", r.Name)
	r.Status.ActiveColor = ColorBlue
	if r.Spec.Replicas == nil {
		r.Spec.Replicas = &DefaultReplicas
	}
	if r.Spec.BackupScaleDownPercent == nil {
		r.Spec.BackupScaleDownPercent = &DefaultScaleDownPercent
	}
}

// +kubebuilder:webhook:verbs=create;update,path=/validate-cluster-kbg-v1alpha1-bluegreendeployment,mutating=false,failurePolicy=fail,groups=cluster.kbg,resources=bluegreendeployments,versions=v1alpha1,name=vbluegreendeployment.kb.io
var _ webhook.Validator = &BlueGreenDeployment{}

// ValidateCreate implements webhook.Validator so a webhook will be registered for the type
func (r *BlueGreenDeployment) ValidateCreate() error {
	bluegreendeploymentlog.Info("validate create", "name", r.Name)
	return r.validate()
}

// ValidateUpdate implements webhook.Validator so a webhook will be registered for the type
func (r *BlueGreenDeployment) ValidateUpdate(old runtime.Object) error {
	bluegreendeploymentlog.Info("validate update", "name", r.Name)
	return r.validate()
}

// ValidateDelete implements webhook.Validator so a webhook will be registered for the type
func (r *BlueGreenDeployment) ValidateDelete() error {
	bluegreendeploymentlog.Info("validate delete", "name", r.Name)
	return nil
}

func (r *BlueGreenDeployment) validate() error {
	var errs field.ErrorList
	scaleDown := *r.Spec.BackupScaleDownPercent
	if scaleDown < 0 || scaleDown > 100 {
		path := field.NewPath("spec").Child("backupScaleDownPercent")
		errs = append(errs, field.Invalid(path, scaleDown, "should be in [0, 100]"))
	}
	replicas := *r.Spec.Replicas
	if replicas < 0 {
		path := field.NewPath("spec").Child("replicas")
		errs = append(errs, field.Invalid(path, scaleDown, "should be > 0"))
	}

	// todo other stuff, but also have a look at declarative https://book.kubebuilder.io/reference/markers/crd-validation.html

	return apierrors.NewInvalid(
		schema.GroupKind{Group: r.GroupVersionKind().Group, Kind: r.GroupVersionKind().Kind},
		r.Name, errs)
}
