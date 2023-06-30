/*
   MIT License

   Copyright (c) Microsoft Corporation.

   Permission is hereby granted, free of charge, to any person obtaining a copy
   of this software and associated documentation files (the "Software"), to deal
   in the Software without restriction, including without limitation the rights
   to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
   copies of the Software, and to permit persons to whom the Software is
   furnished to do so, subject to the following conditions:

   The above copyright notice and this permission notice shall be included in all
   copies or substantial portions of the Software.

   THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
   IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
   FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
   AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
   LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
   OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
   SOFTWARE

*/

package symphonymicrosoftcom

import (
	"context"
	"fmt"
	"strconv"

	symphonyv1 "gopls-workspace/apis/symphony.microsoft.com/v1"
	"gopls-workspace/constants"
	utils "gopls-workspace/utils"
	provisioningstates "gopls-workspace/utils/models"

	"github.com/azure/symphony/api/pkg/apis/v1alpha1/model"
	api_utils "github.com/azure/symphony/api/pkg/apis/v1alpha1/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// TargetReconciler reconciles a Target object
type TargetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=symphony.microsoft.com,resources=targets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=symphony.microsoft.com,resources=targets/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=symphony.microsoft.com,resources=targets/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Target object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.11.0/pkg/reconcile
func (r *TargetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	myFinalizerName := "target.symphony.microsoft.com/finalizer"

	log := ctrllog.FromContext(ctx)
	log.Info("Reconcile Target")

	// Get target
	target := &symphonyv1.Target{}
	if err := r.Get(ctx, req.NamespacedName, target); err != nil {
		log.Error(err, "unable to fetch Target object")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	r.ensureOperationState(target, provisioningstates.Reconciling)
	err := r.Status().Update(ctx, target)
	if err != nil {
		log.Error(err, "unable to update Target status")
		return ctrl.Result{}, err
	}

	if target.ObjectMeta.DeletionTimestamp.IsZero() { // update

		if !controllerutil.ContainsFinalizer(target, myFinalizerName) {
			controllerutil.AddFinalizer(target, myFinalizerName)
			if err := r.Update(ctx, target); err != nil {
				return ctrl.Result{}, err
			}
		}

		deployment, err := utils.CreateSymphonyDeploymentFromTarget(*target)
		if err != nil {
			log.Error(err, "failed to create Symphony deployment")
			return ctrl.Result{}, r.updateTargetStatus(target, "deploymentCreationFailed", provisioningstates.Failed, model.SummarySpec{
				TargetCount:  1,
				SuccessCount: 0,
				TargetResults: map[string]model.TargetResultSpec{
					"self": {
						Status:  "Failed",
						Message: err.Error(),
					},
				},
			}, err)
		}

		if len(deployment.Assignments) != 0 {
			summary, err := api_utils.Deploy("http://symphony-service:8080/v1alpha2/", "admin", "", deployment)
			if err != nil {
				log.Error(err, "failed to deploy to Symphony")
				return ctrl.Result{}, r.updateTargetStatus(target, "Failed", provisioningstates.Failed, summary, err)
			}

			if err := r.Update(ctx, target); err != nil {
				return ctrl.Result{}, r.updateTargetStatus(target, "State Failed", provisioningstates.Failed, summary, err)
			} else {
				err = r.updateTargetStatus(target, "OK", provisioningstates.Succeeded, summary, nil)
				if err != nil {
					return ctrl.Result{}, err
				}
			}
		}

		return ctrl.Result{}, nil
	} else { // remove
		if controllerutil.ContainsFinalizer(target, myFinalizerName) {
			//summary := model.SummarySpec{}
			deployment, err := utils.CreateSymphonyDeploymentFromTarget(*target)
			if err != nil {
				log.Error(err, "failed to generate Symphony deployment")
			} else {
				_, err = api_utils.Remove("http://symphony-service:8080/v1alpha2/", "admin", "", deployment)
				if err != nil { // TODO: this could stop the CRD being removed if the underlying component is permanantly destroyed
					log.Error(err, "failed to delete components")
				}
			}

			controllerutil.RemoveFinalizer(target, myFinalizerName)
			if err := r.Update(ctx, target); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	return ctrl.Result{}, nil
}

func (r *TargetReconciler) updateTargetStatus(target *symphonyv1.Target, status string, provisioningStatus string, summary model.SummarySpec, provisioningError error) error {
	// Start clean and update all the fields in target.Status.Properties{}
	target.Status.Properties = make(map[string]string)

	target.Status.Properties["status"] = status
	target.Status.Properties["targets"] = strconv.Itoa(summary.TargetCount)
	target.Status.Properties["deployed"] = strconv.Itoa(summary.SuccessCount)
	for k, v := range summary.TargetResults {
		target.Status.Properties["targets."+k] = fmt.Sprintf("%s - %s", v.Status, v.Message)
	}

	r.updateProvisioningStatus(target, provisioningStatus, provisioningError)

	target.Status.LastModified = metav1.Now()
	return r.Status().Update(context.Background(), target)
}

// SetupWithManager sets up the controller with the Manager.
func (r *TargetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	genChangePredicate := predicate.GenerationChangedPredicate{}
	annotationPredicate := predicate.AnnotationChangedPredicate{}
	return ctrl.NewControllerManagedBy(mgr).
		WithEventFilter(predicate.Or(genChangePredicate, annotationPredicate)).
		For(&symphonyv1.Target{}).
		Complete(r)
}

func (r *TargetReconciler) ensureOperationState(target *symphonyv1.Target, provisioningState string) {
	target.Status.ProvisioningStatus.Status = provisioningState
	target.Status.ProvisioningStatus.OperationID = target.ObjectMeta.Annotations[constants.AzureOperationKey]
}

func (r *TargetReconciler) updateProvisioningStatus(target *symphonyv1.Target, provisioningStatus string, provisioningError error) {
	r.ensureOperationState(target, provisioningStatus)
	// Start with a clean Error object and update all the fields
	target.Status.ProvisioningStatus.Error = symphonyv1.ErrorType{}

	// Fill error details into target
	errorObj := &target.Status.ProvisioningStatus.Error
	if provisioningError != nil {
		parsedError, err := utils.ParseAsAPIError(provisioningError)
		if err != nil {
			errorObj.Code = "500"
			errorObj.Message = fmt.Sprintf("Deployment failed. %s", provisioningError.Error())
			return
		}
		errorObj.Code = parsedError.Code
		errorObj.Message = "Deployment failed."
		errorObj.Target = "Symphony"
		errorObj.Details = make([]symphonyv1.TargetError, 0)
		for k, v := range parsedError.Spec.TargetResults {
			errorObj.Details = append(errorObj.Details, symphonyv1.TargetError{
				Code:    v.Status,
				Message: v.Message,
				Target:  k,
			})
		}
	}
}