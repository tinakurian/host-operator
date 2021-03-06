package usersignup

import (
	"context"
	"fmt"
	"k8s.io/apimachinery/pkg/util/validation"
	"strings"

	toolchainv1alpha1 "github.com/codeready-toolchain/api/pkg/apis/toolchain/v1alpha1"
	"github.com/codeready-toolchain/host-operator/pkg/config"
	"github.com/codeready-toolchain/toolchain-common/pkg/cluster"
	commonCondition "github.com/codeready-toolchain/toolchain-common/pkg/condition"
	"k8s.io/apimachinery/pkg/types"

	"github.com/go-logr/logr"
	"github.com/operator-framework/operator-sdk/pkg/predicate"
	errs "github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	// Status condition reasons
	noClustersAvailableReason            = "NoClustersAvailable"
	noTemplateTierAvailableReason        = "NoTemplateTierAvailable"
	failedToReadUserApprovalPolicyReason = "FailedToReadUserApprovalPolicy"
	unableToCreateMURReason              = "UnableToCreateMUR"
	invalidMURState                      = "InvalidMURState"
	approvedAutomaticallyReason          = "ApprovedAutomatically"
	approvedByAdminReason                = "ApprovedByAdmin"
	pendingApprovalReason                = "PendingApproval"
)

var log = logf.Log.WithName("controller_usersignup")

// Add creates a new UserSignup Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileUserSignup{client: mgr.GetClient(), scheme: mgr.GetScheme()}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("usersignup-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource UserSignup
	err = c.Watch(&source.Kind{Type: &toolchainv1alpha1.UserSignup{}}, &handler.EnqueueRequestForObject{},
		predicate.GenerationChangedPredicate{})
	if err != nil {
		return err
	}

	// Watch for changes to the secondary resource MasterUserRecord and requeue the owner UserSignup
	err = c.Watch(&source.Kind{Type: &toolchainv1alpha1.MasterUserRecord{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &toolchainv1alpha1.UserSignup{},
	})
	if err != nil {
		return err
	}

	return nil
}

var _ reconcile.Reconciler = &ReconcileUserSignup{}

// NewSignupError returns a new Signup error
func NewSignupError(msg string) SignupError {
	return SignupError{message: msg}
}

// SignupError an error that occurs during user signup
type SignupError struct {
	message string
}

func (err SignupError) Error() string {
	return err.message
}

// ReconcileUserSignup reconciles a UserSignup object
type ReconcileUserSignup struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client client.Client
	scheme *runtime.Scheme
}

// Reconcile reads that state of the cluster for a UserSignup object and makes changes based on the state read
// and what is in the UserSignup.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileUserSignup) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling UserSignup")

	// Fetch the UserSignup instance
	instance := &toolchainv1alpha1.UserSignup{}
	err := r.client.Get(context.TODO(), request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	// List all MasterUserRecord resources that have a UserID label equal to the UserSignup.Name
	labels := map[string]string{toolchainv1alpha1.MasterUserRecordUserIDLabelKey: instance.Name}
	opts := client.MatchingLabels(labels)
	murList := &toolchainv1alpha1.MasterUserRecordList{}
	if err = r.client.List(context.TODO(), murList, opts); err != nil {
		return reconcile.Result{}, r.wrapErrorWithStatusUpdate(reqLogger, instance, r.setStatusInvalidMURState, err, "Failed to list MasterUserRecords")
	}

	murs := murList.Items
	// If we found more than one MasterUserRecord, then die
	if len(murs) > 1 {
		err = NewSignupError("multiple matching MasterUserRecord resources found")
		return reconcile.Result{}, r.wrapErrorWithStatusUpdate(reqLogger, instance, r.setStatusInvalidMURState, err, "Multiple MasterUserRecords found")
	} else if len(murs) == 1 {
		// If we successfully found an existing MasterUserRecord then our work here is done, set the status
		// to Complete and return
		mur := murs[0]
		reqLogger.Info("MasterUserRecord exists, setting status to Complete")
		instance.Status.CompliantUsername = mur.Name
		return reconcile.Result{}, r.updateStatus(reqLogger, instance, r.setStatusComplete)
	}

	// Check the user approval policy.
	userApprovalPolicy, err := r.ReadUserApprovalPolicyConfig(request.Namespace)
	if err != nil {
		return reconcile.Result{}, r.wrapErrorWithStatusUpdate(reqLogger, instance, r.setStatusFailedToReadUserApprovalPolicy, err, "")
	}

	// If the signup has been explicitly approved (by an admin), or the user approval policy is set to automatic,
	// then proceed with the signup
	if instance.Spec.Approved || userApprovalPolicy == config.UserApprovalPolicyAutomatic {
		if instance.Spec.Approved {
			if statusError := r.updateStatus(reqLogger, instance, r.setStatusApprovedByAdmin); statusError != nil {
				return reconcile.Result{}, statusError
			}
		} else {
			if statusError := r.updateStatus(reqLogger, instance, r.setStatusApprovedAutomatically); statusError != nil {
				return reconcile.Result{}, statusError
			}
		}

		var targetCluster string

		// If a target cluster hasn't been selected, select one from the members
		if instance.Spec.TargetCluster != "" {
			targetCluster = instance.Spec.TargetCluster
		} else {
			// Automatic cluster selection
			members := cluster.GetMemberClusters()
			if len(members) > 0 {
				targetCluster = members[0].Name
			} else {
				reqLogger.Error(err, "No member clusters found")
				if statusError := r.updateStatus(reqLogger, instance, r.setStatusNoClustersAvailable); statusError != nil {
					return reconcile.Result{}, statusError
				}

				err = NewSignupError("no target clusters available")
				return reconcile.Result{}, err
			}
		}
		// look-up the `basic` NSTemplateTier to get the NS templates
		var nstemplateTier toolchainv1alpha1.NSTemplateTier
		err := r.client.Get(context.TODO(), types.NamespacedName{
			Namespace: request.Namespace, // assume that NSTemplateTier were created in the same NS as Usersignups
			Name:      "basic",
		}, &nstemplateTier)
		if err != nil {
			// let's requeue until the NSTemplateTier resource is available
			return reconcile.Result{Requeue: true}, r.wrapErrorWithStatusUpdate(reqLogger, instance, r.setStatusNoTemplateTierAvailable, err, "")
		}
		// Provision the MasterUserRecord
		err = r.provisionMasterUserRecord(instance, targetCluster, nstemplateTier, reqLogger)
		if err != nil {
			return reconcile.Result{}, err
		}
	} else {
		return reconcile.Result{}, r.updateStatus(reqLogger, instance, r.setStatusPendingApproval)
	}

	return reconcile.Result{}, nil
}

func (r *ReconcileUserSignup) generateCompliantUsername(instance *toolchainv1alpha1.UserSignup) (string, error) {
	replaced := strings.ReplaceAll(strings.ReplaceAll(instance.Spec.Username, "@", "-at-"), ".", "-")

	errs := validation.IsQualifiedName(replaced)
	if len(errs) > 0 {
		return "", NewSignupError(fmt.Sprintf("transformed username [%s] is invalid", replaced))
	}

	transformed := replaced

	for i := 1; i < 101; i++ { // No more than 100 attempts to find a vacant name
		mur := &toolchainv1alpha1.MasterUserRecord{}
		// Check if a MasterUserRecord exists with the same transformed name
		namespacedMurName := types.NamespacedName{Namespace: instance.Namespace, Name: transformed}
		err := r.client.Get(context.TODO(), namespacedMurName, mur)
		if err != nil {
			if !errors.IsNotFound(err) {
				return "", err
			}
			// If there was a NotFound error looking up the mur, it means we found an available name
			return transformed, nil
		} else if mur.Labels[toolchainv1alpha1.MasterUserRecordUserIDLabelKey] == instance.Name {
			// If the found MUR has the same UserID as the UserSignup, then *it* is the correct MUR -
			// Return an error here and allow the reconcile() function to pick it up on the next loop
			return "", NewSignupError(fmt.Sprintf("could not generate compliant username as MasterUserRecord [%s] already exists", mur.Name))
		}

		transformed = fmt.Sprintf("%s-%d", replaced, i)
	}

	return "", NewSignupError(fmt.Sprintf("unable to transform username [%s] even after 100 attempts", instance.Spec.Username))
}

// provisionMasterUserRecord does the work of provisioning the MasterUserRecord
func (r *ReconcileUserSignup) provisionMasterUserRecord(userSignup *toolchainv1alpha1.UserSignup, targetCluster string, nstemplateTier toolchainv1alpha1.NSTemplateTier, logger logr.Logger) error {
	namespaces := make([]toolchainv1alpha1.NSTemplateSetNamespace, len(nstemplateTier.Spec.Namespaces))
	for i, ns := range nstemplateTier.Spec.Namespaces {
		namespaces[i] = toolchainv1alpha1.NSTemplateSetNamespace{
			Type:     ns.Type,
			Revision: ns.Revision,
		}
	}

	userAccounts := []toolchainv1alpha1.UserAccountEmbedded{
		{
			TargetCluster: targetCluster,
			Spec: toolchainv1alpha1.UserAccountSpec{
				UserID:  userSignup.Name,
				NSLimit: "default",
				NSTemplateSet: toolchainv1alpha1.NSTemplateSetSpec{
					TierName:   nstemplateTier.Name,
					Namespaces: namespaces,
				},
			},
		},
	}

	// TODO Update the MasterUserRecord with NSTemplateTier values
	// SEE https://jira.coreos.com/browse/CRT-74

	compliantUsername, err := r.generateCompliantUsername(userSignup)
	if err != nil {
		return r.wrapErrorWithStatusUpdate(logger, userSignup, r.setStatusFailedToCreateMUR, err,
			"Error generating compliant username for %s", userSignup.Spec.Username)
	}

	labels := map[string]string{toolchainv1alpha1.MasterUserRecordUserIDLabelKey: userSignup.Name}

	mur := &toolchainv1alpha1.MasterUserRecord{
		ObjectMeta: metav1.ObjectMeta{
			Name:      compliantUsername,
			Namespace: userSignup.Namespace,
			Labels:    labels,
		},
		Spec: toolchainv1alpha1.MasterUserRecordSpec{
			UserAccounts: userAccounts,
		},
	}

	err = controllerutil.SetControllerReference(userSignup, mur, r.scheme)
	if err != nil {
		return r.wrapErrorWithStatusUpdate(logger, userSignup, r.setStatusFailedToCreateMUR, err,
			"Error setting controller reference for MasterUserRecord %s", mur.Name)
	}

	err = r.client.Create(context.TODO(), mur)
	if err != nil {
		return r.wrapErrorWithStatusUpdate(logger, userSignup, r.setStatusFailedToCreateMUR, err,
			"Error creating MasterUserRecord")
	}

	logger.Info("Created MasterUserRecord", "Name", mur.Name, "TargetCluster", targetCluster)
	return nil
}

// ReadUserApprovalPolicyConfig reads the ConfigMap for the toolchain configuration in the operator namespace, and returns
// the config map value for the user approval policy (which will either be "manual" or "automatic")
func (r *ReconcileUserSignup) ReadUserApprovalPolicyConfig(namespace string) (string, error) {
	cm := &corev1.ConfigMap{}
	err := r.client.Get(context.TODO(), types.NamespacedName{Namespace: namespace, Name: config.ToolchainConfigMapName}, cm)
	if err != nil {
		if errors.IsNotFound(err) {
			return config.UserApprovalPolicyManual, nil
		}
		return "", err
	}

	val, ok := cm.Data[config.ToolchainConfigMapUserApprovalPolicy]
	if !ok {
		return "", nil
	}
	return val, nil
}

func (r *ReconcileUserSignup) setStatusApprovedAutomatically(userSignup *toolchainv1alpha1.UserSignup, message string) error {
	return r.updateStatusConditions(
		userSignup,
		toolchainv1alpha1.Condition{
			Type:    toolchainv1alpha1.UserSignupApproved,
			Status:  corev1.ConditionTrue,
			Reason:  approvedAutomaticallyReason,
			Message: message,
		})
}

func (r *ReconcileUserSignup) setStatusApprovedByAdmin(userSignup *toolchainv1alpha1.UserSignup, message string) error {
	return r.updateStatusConditions(
		userSignup,
		toolchainv1alpha1.Condition{
			Type:    toolchainv1alpha1.UserSignupApproved,
			Status:  corev1.ConditionTrue,
			Reason:  approvedByAdminReason,
			Message: message,
		})
}

func (r *ReconcileUserSignup) setStatusPendingApproval(userSignup *toolchainv1alpha1.UserSignup, message string) error {
	return r.updateStatusConditions(
		userSignup,
		toolchainv1alpha1.Condition{
			Type:    toolchainv1alpha1.UserSignupApproved,
			Status:  corev1.ConditionFalse,
			Reason:  pendingApprovalReason,
			Message: message,
		},
		toolchainv1alpha1.Condition{
			Type:    toolchainv1alpha1.UserSignupComplete,
			Status:  corev1.ConditionFalse,
			Reason:  pendingApprovalReason,
			Message: message,
		})
}

func (r *ReconcileUserSignup) setStatusFailedToReadUserApprovalPolicy(userSignup *toolchainv1alpha1.UserSignup, message string) error {
	return r.updateStatusConditions(
		userSignup,
		toolchainv1alpha1.Condition{
			Type:    toolchainv1alpha1.UserSignupComplete,
			Status:  corev1.ConditionFalse,
			Reason:  failedToReadUserApprovalPolicyReason,
			Message: message,
		})
}

func (r *ReconcileUserSignup) setStatusInvalidMURState(userSignup *toolchainv1alpha1.UserSignup, message string) error {
	return r.updateStatusConditions(
		userSignup,
		toolchainv1alpha1.Condition{
			Type:    toolchainv1alpha1.UserSignupComplete,
			Status:  corev1.ConditionFalse,
			Reason:  invalidMURState,
			Message: message,
		})
}

func (r *ReconcileUserSignup) setStatusFailedToCreateMUR(userSignup *toolchainv1alpha1.UserSignup, message string) error {
	return r.updateStatusConditions(
		userSignup,
		toolchainv1alpha1.Condition{
			Type:    toolchainv1alpha1.UserSignupComplete,
			Status:  corev1.ConditionFalse,
			Reason:  unableToCreateMURReason,
			Message: message,
		})
}

func (r *ReconcileUserSignup) setStatusNoClustersAvailable(userSignup *toolchainv1alpha1.UserSignup, message string) error {
	return r.updateStatusConditions(
		userSignup,
		toolchainv1alpha1.Condition{
			Type:    toolchainv1alpha1.UserSignupComplete,
			Status:  corev1.ConditionFalse,
			Reason:  noClustersAvailableReason,
			Message: message,
		})
}

func (r *ReconcileUserSignup) setStatusNoTemplateTierAvailable(userSignup *toolchainv1alpha1.UserSignup, message string) error {
	return r.updateStatusConditions(
		userSignup,
		toolchainv1alpha1.Condition{
			Type:    toolchainv1alpha1.UserSignupComplete,
			Status:  corev1.ConditionFalse,
			Reason:  noTemplateTierAvailableReason,
			Message: message,
		})
}

func (r *ReconcileUserSignup) setStatusComplete(userSignup *toolchainv1alpha1.UserSignup, message string) error {
	return r.updateStatusConditions(
		userSignup,
		toolchainv1alpha1.Condition{
			Type:    toolchainv1alpha1.UserSignupComplete,
			Status:  corev1.ConditionTrue,
			Reason:  "",
			Message: message,
		})
}

func (r *ReconcileUserSignup) updateStatus(logger logr.Logger, userSignup *toolchainv1alpha1.UserSignup,
	statusUpdater func(userAcc *toolchainv1alpha1.UserSignup, message string) error) error {

	if err := statusUpdater(userSignup, ""); err != nil {
		logger.Error(err, "status update failed")
		return err
	}

	return nil
}

// wrapErrorWithStatusUpdate wraps the error and update the UserSignup status. If the update fails then the error is logged.
func (r *ReconcileUserSignup) wrapErrorWithStatusUpdate(logger logr.Logger, userSignup *toolchainv1alpha1.UserSignup,
	statusUpdater func(userAcc *toolchainv1alpha1.UserSignup, message string) error, err error, format string, args ...interface{}) error {
	if err == nil {
		return nil
	}
	if err := statusUpdater(userSignup, err.Error()); err != nil {
		logger.Error(err, "Error updating UserSignup status")
	}
	return errs.Wrapf(err, format, args...)
}

func (r *ReconcileUserSignup) updateStatusConditions(userSignup *toolchainv1alpha1.UserSignup, newConditions ...toolchainv1alpha1.Condition) error {
	var updated bool
	userSignup.Status.Conditions, updated = commonCondition.AddOrUpdateStatusConditions(userSignup.Status.Conditions, newConditions...)
	if !updated {
		// Nothing changed
		return nil
	}
	return r.client.Status().Update(context.TODO(), userSignup)
}
