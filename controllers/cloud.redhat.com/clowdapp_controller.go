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
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	strimzi "github.com/RedHatInsights/strimzi-client-go/apis/kafka.strimzi.io/v1beta2"
	"github.com/go-logr/logr"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	// Import the providers to initialize them
	"github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/clowder_config"
	"github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/config"
	"github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers"

	// These imports are to register the providers with the provider registration system
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/confighash"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/cronjob"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/database"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/dependencies"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/deployment"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/featureflags"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/inmemorydb"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/iqe"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/kafka"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/logging"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/metrics"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/namespace"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/objectstore"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/pullsecrets"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/serviceaccount"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/servicemesh"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/sidecar"
	_ "github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/web"

	crd "github.com/RedHatInsights/clowder/apis/cloud.redhat.com/v1alpha1"
	"github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/errors"
	"github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/utils"

	"github.com/RedHatInsights/go-difflib/difflib"
)

const appFinalizer = "finalizer.app.cloud.redhat.com"

// ClowdAppReconciler reconciles a ClowdApp object
type ClowdAppReconciler struct {
	client.Client
	Log      logr.Logger
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=cloud.redhat.com,resources=clowdapps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cloud.redhat.com,resources=clowdapps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=serviceaccounts;configmaps;services;persistentvolumeclaims;secrets;events;namespaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=cronjobs;jobs,verbs=get;list;create;update;watch;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kafka.strimzi.io,resources=kafkatopics,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kafka.strimzi.io,resources=kafkas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kafka.strimzi.io,resources=kafkausers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kafka.strimzi.io,resources=kafkaconnects,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cyndi.cloud.redhat.com,resources=cyndipipelines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings;roles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=prometheuses;servicemonitors,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kafka.strimzi.io,resources=kafkaconnectors,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=endpoints;pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses;networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=config.openshift.io,resources=ingresses,verbs=get;list

// Reconcile fn
func (r *ClowdAppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	qualifiedName := fmt.Sprintf("%s:%s", req.Namespace, req.Name)
	log := r.Log.WithValues("app", qualifiedName).WithValues("id", utils.RandString(5))
	ctx = context.WithValue(ctx, errors.ClowdKey("log"), &log)
	ctx = context.WithValue(ctx, errors.ClowdKey("recorder"), &r.Recorder)
	app := crd.ClowdApp{}
	err := r.Client.Get(ctx, req.NamespacedName, &app)

	if err != nil {
		if k8serr.IsNotFound(err) {
			// Must have been deleted
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	isAppMarkedForDeletion := app.GetDeletionTimestamp() != nil
	if isAppMarkedForDeletion {
		if contains(app.GetFinalizers(), appFinalizer) {
			if err := r.finalizeApp(log, &app); err != nil {
				return ctrl.Result{}, err
			}

			controllerutil.RemoveFinalizer(&app, appFinalizer)
			err := r.Update(ctx, &app)
			if err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Add finalizer for this CR
	if !contains(app.GetFinalizers(), appFinalizer) {
		if err := r.addFinalizer(log, &app); err != nil {
			return ctrl.Result{}, err
		}
	}

	if ReadEnv() == app.Spec.EnvName {
		r.Recorder.Eventf(&app, "Warning", "ClowdEnvLocked", "Clowder Environment [%s] is locked", app.Spec.EnvName)
		return ctrl.Result{Requeue: true}, fmt.Errorf("env currently being reconciled")
	}

	log.Info("Reconciliation started", "app", fmt.Sprintf("%s:%s", app.Namespace, app.Name))

	if app.Spec.Disabled {
		log.Info("Reconciliation aborted - set to be disabled", "app", fmt.Sprintf("%s:%s", app.Namespace, app.Name))
		return ctrl.Result{}, nil
	}

	ctx = context.WithValue(ctx, errors.ClowdKey("obj"), &app)

	env := crd.ClowdEnvironment{}
	err = r.Client.Get(ctx, types.NamespacedName{
		Name: app.Spec.EnvName,
	}, &env)

	if err != nil {
		r.Recorder.Eventf(&app, "Warning", "ClowdEnvMissing", "Clowder Environment [%s] is missing", app.Spec.EnvName)
		SetClowdAppConditions(ctx, r.Client, &app, crd.ReconciliationFailed, err)
		return ctrl.Result{Requeue: true}, err
	}

	if env.Generation != env.Status.Generation {
		r.Recorder.Eventf(&app, "Warning", "ClowdEnvNotReconciled", "Clowder Environment [%s] is not reconciled", app.Spec.EnvName)
		log.Info("Env not yet reconciled", "app", app.Name, "namespace", app.Namespace)
		return ctrl.Result{Requeue: true, RequeueAfter: time.Second * 10}, nil
	}

	if !env.IsReady() {
		r.Recorder.Eventf(&app, "Warning", "ClowdEnvNotReady", "Clowder Environment [%s] is not ready", app.Spec.EnvName)
		log.Info("Env not yet ready", "app", app.Name, "namespace", app.Namespace)
		return ctrl.Result{Requeue: true, RequeueAfter: time.Second * 10}, nil
	}

	cache := providers.NewObjectCache(ctx, r.Client, scheme)

	provider := providers.Provider{
		Client: r.Client,
		Ctx:    ctx,
		Env:    &env,
		Cache:  &cache,
		Log:    log,
	}

	var requeue = false

	provErr := r.runProviders(log, &provider, &app)

	if provErr != nil {
		if non_fatal := errors.HandleError(ctx, provErr); !non_fatal {
			SetClowdAppConditions(ctx, r.Client, &app, crd.ReconciliationFailed, provErr)
			return ctrl.Result{}, provErr
		} else {
			requeue = true
		}
	}

	cacheErr := cache.ApplyAll()

	if cacheErr != nil {
		if non_fatal := errors.HandleError(ctx, provErr); !non_fatal {
			SetClowdAppConditions(ctx, r.Client, &app, crd.ReconciliationFailed, cacheErr)
			return ctrl.Result{}, cacheErr
		} else {
			requeue = true
		}
	}

	if statusErr := SetDeploymentStatus(ctx, r.Client, &app); statusErr != nil {
		return ctrl.Result{}, err
	}

	// Delete all resources that are not used anymore
	if !requeue {
		r.Recorder.Eventf(&app, "Normal", "SuccessfulReconciliation", "Clowdapp reconciled [%s]", app.GetClowdName())
		log.Info("Reconciliation successful", "app", fmt.Sprintf("%s:%s", app.Namespace, app.Name))
		err := cache.Reconcile(&app)
		if err != nil {
			log.Info("Reconcile error", "error", err)
			return ctrl.Result{Requeue: requeue}, nil
		}
		SetClowdAppConditions(ctx, r.Client, &app, crd.ReconciliationSuccessful, nil)
	} else {
		var err error
		if provErr != nil {
			err = provErr
		} else if cacheErr != nil {
			err = cacheErr
		}
		log.Info("Reconciliation partially successful", "app", fmt.Sprintf("%s:%s", app.Namespace, app.Name))
		r.Recorder.Eventf(&app, "Warning", "SuccessfulPartialReconciliation", "Clowdapp requeued [%s]", app.GetClowdName())
		SetClowdAppConditions(ctx, r.Client, &app, crd.ReconciliationPartiallySuccessful, err)
	}

	if err == nil {
		if _, ok := managedApps[app.GetIdent()]; !ok {
			managedApps[app.GetIdent()] = true
		}
		managedAppsMetric.Set(float64(len(managedApps)))
	}

	return ctrl.Result{Requeue: requeue}, nil
}

func isOurs(meta metav1.Object, gvk schema.GroupVersionKind) bool {
	if gvk.Kind == "ClowdEnvironment" {
		return true
	} else if gvk.Kind == "ClowdApp" {
		return true
	} else if gvk.Kind == "ClowdJobInvocation" {
		return true
	} else if len(meta.GetOwnerReferences()) == 0 {
		return false
	} else if meta.GetOwnerReferences()[0].Kind == "ClowdApp" {
		return true
	} else if meta.GetOwnerReferences()[0].Kind == "ClowdEnvironment" {
		return true
	} else if meta.GetOwnerReferences()[0].Kind == "ClowdJobInvocation" {
		return true
	}
	return false
}

func ignoreStatusUpdatePredicate(logr logr.Logger, ctrlName string) predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			gvk, _ := utils.GetKindFromObj(scheme, e.Object)
			if !isOurs(e.Object, gvk) {
				return false
			}
			logr.Info("Reconciliation trigger", "ctrl", ctrlName, "type", "create", "resType", gvk.Kind, "name", e.Object.GetName(), "namespace", e.Object.GetNamespace())
			return true
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			gvk, _ := utils.GetKindFromObj(scheme, e.ObjectNew)
			if !isOurs(e.ObjectNew, gvk) {
				return false
			}

			logr.Info("Reconciliation trigger", "ctrl", ctrlName, "type", "update", "resType", gvk.Kind, "name", e.ObjectOld.GetName(), "namespace", e.ObjectOld.GetNamespace())

			if clowder_config.LoadedConfig.DebugOptions.Trigger.Diff {
				if e.ObjectNew.GetObjectKind().GroupVersionKind() == secretCompare {
					logr.Info("Trigger diff", "diff", "hidden", "ctrl", ctrlName, "type", "update", "resType", gvk.Kind, "name", e.ObjectOld.GetName(), "namespace", e.ObjectOld.GetNamespace())
				} else {
					oldObjJSON, _ := json.MarshalIndent(e.ObjectOld, "", "  ")
					newObjJSON, _ := json.MarshalIndent(e.ObjectNew, "", "  ")

					diff := difflib.UnifiedDiff{
						A:        difflib.SplitLines(string(oldObjJSON)),
						B:        difflib.SplitLines(string(newObjJSON)),
						FromFile: "old",
						ToFile:   "new",
						Context:  3,
					}
					text, _ := difflib.GetUnifiedDiffString(diff)
					logr.Info("Trigger diff", "diff", text, "ctrl", ctrlName, "type", "update", "resType", gvk.Kind, "name", e.ObjectOld.GetName(), "namespace", e.ObjectOld.GetNamespace())
				}
			}

			// Always update if a deployment being watched changes - this allows
			// status rollups to occur
			if reflect.TypeOf(e.ObjectNew).String() == "*v1.Deployment" {
				return true
			}

			// Allow reconciliation if the env changed status
			if objOld, ok := e.ObjectOld.(*crd.ClowdEnvironment); ok {
				if objNew, ok := e.ObjectNew.(*crd.ClowdEnvironment); ok {
					if !objOld.Status.Ready && objNew.Status.Ready {
						return true
					}
				}
			}

			if objOld, ok := e.ObjectOld.(*strimzi.Kafka); ok {
				if objNew, ok := e.ObjectNew.(*strimzi.Kafka); ok {
					if (objOld.Status != nil && objNew.Status != nil) && len(objOld.Status.Listeners) != len(objNew.Status.Listeners) {
						return true
					}
				}
			}

			// Ignore updates to CR status in which case metadata.Generation does not change
			return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration()
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			gvk, _ := utils.GetKindFromObj(scheme, e.Object)
			if !isOurs(e.Object, gvk) {
				return false
			}
			logr.Info("Reconciliation trigger", "ctrl", ctrlName, "type", "delete", "resType", gvk, "name", e.Object.GetName(), "namespace", e.Object.GetNamespace())
			return true
		},
		GenericFunc: func(e event.GenericEvent) bool {
			gvk, _ := utils.GetKindFromObj(scheme, e.Object)
			if !isOurs(e.Object, gvk) {
				return false
			}
			logr.Info("Reconciliation trigger", "ctrl", ctrlName, "type", "generic", "resType", gvk, "name", e.Object.GetName(), "namespace", e.Object.GetNamespace())
			return true
		},
	}
}

// SetupWithManager sets up with Manager
func (r *ClowdAppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Log.Info("Setting up manager")
	utils.Log = r.Log.WithValues("name", "util")
	r.Recorder = mgr.GetEventRecorderFor("app")

	cache := mgr.GetCache()

	cache.IndexField(
		context.TODO(), &crd.ClowdApp{}, "spec.envName", func(o client.Object) []string {
			return []string{o.(*crd.ClowdApp).Spec.EnvName}
		})

	return ctrl.NewControllerManagedBy(mgr).
		For(&crd.ClowdApp{}).
		Watches(
			&source.Kind{Type: &crd.ClowdEnvironment{}},
			handler.EnqueueRequestsFromMapFunc(r.appsToEnqueueUponEnvUpdate),
		).
		Owns(&apps.Deployment{}).
		Owns(&core.Service{}).
		Owns(&core.ConfigMap{}).
		WithEventFilter(ignoreStatusUpdatePredicate(r.Log, "app")).
		WithOptions(controller.Options{
			RateLimiter: workqueue.NewItemExponentialFailureRateLimiter(time.Duration(500*time.Millisecond), time.Duration(60*time.Second)),
		}).
		Complete(r)
}

func (r *ClowdAppReconciler) appsToEnqueueUponEnvUpdate(a client.Object) []reconcile.Request {
	reqs := []reconcile.Request{}
	ctx := context.Background()
	obj := types.NamespacedName{
		Name:      a.GetName(),
		Namespace: a.GetNamespace(),
	}

	// Get the ClowdEnvironment resource

	env := crd.ClowdEnvironment{}
	err := r.Client.Get(ctx, obj, &env)

	if err != nil {
		if k8serr.IsNotFound(err) {
			// Must have been deleted
			return reqs
		}
		r.Log.Error(err, "Failed to fetch ClowdEnvironment")
		return nil
	}

	// Get all the ClowdApp resources

	appList := crd.ClowdAppList{}
	r.Client.List(ctx, &appList)

	// Filter based on base attribute

	for _, app := range appList.Items {
		if app.Spec.EnvName == env.Name {
			// Add filtered resources to return result
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      app.Name,
					Namespace: app.Namespace,
				},
			})
		}
	}

	return reqs
}

func (r *ClowdAppReconciler) finalizeApp(reqLogger logr.Logger, a *crd.ClowdApp) error {

	delete(managedApps, a.GetIdent())

	managedAppsMetric.Set(float64(len(managedApps)))
	reqLogger.Info("Successfully finalized ClowdApp")
	return nil
}

func (r *ClowdAppReconciler) addFinalizer(reqLogger logr.Logger, a *crd.ClowdApp) error {
	reqLogger.Info("Adding Finalizer for the ClowdApp")
	controllerutil.AddFinalizer(a, appFinalizer)

	// Update CR
	err := r.Update(context.TODO(), a)
	if err != nil {
		reqLogger.Error(err, "Failed to update ClowdApp with finalizer")
		return err
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func updateMetadata(app *crd.ClowdApp, appConfig *config.AppConfig) {
	metadata := config.AppMetadata{}

	for _, deployment := range app.Spec.Deployments {
		deploymentMetadata := config.DeploymentMetadata{
			Name:  deployment.Name,
			Image: deployment.PodSpec.Image,
		}
		metadata.Deployments = append(metadata.Deployments, deploymentMetadata)
	}

	appConfig.Metadata = &metadata

	appConfig.Metadata.Name = &app.Name
}

func (r *ClowdAppReconciler) runProviders(log logr.Logger, provider *providers.Provider, a *crd.ClowdApp) error {

	c := config.AppConfig{}

	// Update app metadata
	updateMetadata(a, &c)

	for _, provAcc := range providers.ProvidersRegistration.Registry {
		log.Info("running provider:", "name", provAcc.Name, "order", provAcc.Order)
		prov, err := provAcc.SetupProvider(provider)
		if err != nil {
			return errors.Wrap(fmt.Sprintf("getprov: %s", provAcc.Name), err)
		}
		err = prov.Provide(a, &c)
		if err != nil {
			reterr := errors.Wrap(fmt.Sprintf("runapp: %s", provAcc.Name), err)
			reterr.Requeue = true
			return reterr
		}
		log.Info("running provider: complete", "name", provAcc.Name, "order", provAcc.Order)
	}

	return nil
}
