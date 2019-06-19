package experiment

import (
	"context"
	"encoding/json"
	"strconv"

	cordeliaclient "github.com/gramLabs/cordelia/pkg/api"
	cordeliaapi "github.com/gramLabs/cordelia/pkg/api/cordelia/v1alpha1"
	cordeliav1alpha1 "github.com/gramLabs/cordelia/pkg/apis/cordelia/v1alpha1"
	cordeliatrial "github.com/gramLabs/cordelia/pkg/controller/trial"
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
	annotationPrefix = "cordelia.carbonrelay.com/"

	annotationExperimentURL  = annotationPrefix + "experiment-url"
	annotationNextTrialURL   = annotationPrefix + "next-trial-url"
	annotationReportTrialURL = annotationPrefix + "report-trial-url"

	finalizer = "finalizer.cordelia.carbonrelay.com"
)

var log = logf.Log.WithName("controller")

// Add creates a new Experiment Controller and adds it to the Manager with default RBAC. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	// We need a remote address to do anything in this controller
	config, err := cordeliaclient.DefaultConfig()
	if err != nil || config.Address == "" {
		return err
	}
	oc, err := cordeliaclient.NewClient(*config)
	if err != nil {
		return err
	}
	return add(mgr, newReconciler(mgr, oc))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, oc cordeliaclient.Client) reconcile.Reconciler {
	return &ReconcileExperiment{Client: mgr.GetClient(), scheme: mgr.GetScheme(), api: cordeliaapi.NewApi(oc)}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("experiment-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to Experiment
	err = c.Watch(&source.Kind{Type: &cordeliav1alpha1.Experiment{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// Watch for changes to owned Trials
	err = c.Watch(&source.Kind{Type: &cordeliav1alpha1.Trial{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &cordeliav1alpha1.Experiment{},
	})
	if err != nil {
		return err
	}

	return nil
}

var _ reconcile.Reconciler = &ReconcileExperiment{}

// ReconcileExperiment reconciles a Experiment object
type ReconcileExperiment struct {
	client.Client
	scheme *runtime.Scheme
	api    cordeliaapi.API
}

// +kubebuilder:rbac:groups=cordelia.carbonrelay.com,resources=experiments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cordelia.carbonrelay.com,resources=experiments/status,verbs=get;update;patch

// Reconcile reads that state of the cluster for a Experiment object and makes changes based on the state read
// and what is in the Experiment.Spec
func (r *ReconcileExperiment) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	// Fetch the Experiment instance
	experiment := &cordeliav1alpha1.Experiment{}
	err := r.Get(context.TODO(), request.NamespacedName, experiment)
	if err != nil {
		if errors.IsNotFound(err) {
			// Object not found, return.  Created objects are automatically garbage collected.
			// For additional cleanup logic use finalizers.
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	// Make sure we aren't deleted without a chance to clean up
	if dirty := addFinalizer(experiment); dirty {
		err := r.Update(context.TODO(), experiment)
		return reconcile.Result{}, err
	}

	// Synchronize with the server
	if dirty, err := r.syncWithServer(experiment); err != nil {
		return reconcile.Result{}, err
	} else if dirty {
		err = r.Update(context.TODO(), experiment)
		return reconcile.Result{}, err
	}

	// Find trials labeled for this experiment
	list := &cordeliav1alpha1.TrialList{}
	opts := &client.ListOptions{}
	if experiment.Spec.Selector == nil {
		opts.MatchingLabels(experiment.Spec.Template.Labels)
		if opts.LabelSelector.Empty() {
			opts.MatchingLabels(experiment.GetDefaultLabels())
		}
	} else if opts.LabelSelector, err = metav1.LabelSelectorAsSelector(experiment.Spec.Selector); err != nil {
		return reconcile.Result{}, err
	}
	if err := r.List(context.TODO(), list, client.UseListOptions(opts)); err != nil {
		return reconcile.Result{}, err
	}

	// Add an additional trial if needed
	nextTrialURL := experiment.GetAnnotations()[annotationNextTrialURL]
	if nextTrialURL != "" && experiment.GetReplicas() > len(list.Items) {
		// Find an available namespace
		if namespace, err := r.findAvailableNamespace(experiment, list.Items); err != nil {
			return reconcile.Result{}, err
		} else if namespace != "" {
			trial := &cordeliav1alpha1.Trial{}
			populateTrialFromTemplate(experiment, trial, namespace)
			if err := controllerutil.SetControllerReference(experiment, trial, r.scheme); err != nil {
				return reconcile.Result{}, err
			}

			// Obtain a suggestion from the server
			suggestion, reportTrialURL, err := r.api.NextTrial(context.TODO(), nextTrialURL)
			if err != nil {
				if aerr, ok := err.(*cordeliaapi.Error); ok {
					switch aerr.Type {
					case cordeliaapi.ErrExperimentStopped:
						// The experiment is stopped, set replicas to 0 to prevent further interaction with the server
						experiment.SetReplicas(0)
						delete(experiment.GetAnnotations(), annotationNextTrialURL) // HTTP "Gone" semantics require us to purge this
						err = r.Update(context.TODO(), experiment)
						return reconcile.Result{}, err
					case cordeliaapi.ErrTrialUnavailable:
						// No suggestions available, wait to requeue until after the retry delay
						return reconcile.Result{Requeue: true, RequeueAfter: aerr.RetryAfter}, nil
					}
				}
				return reconcile.Result{}, err
			}

			// Add the information from the server
			trial.GetAnnotations()[annotationReportTrialURL] = reportTrialURL
			for _, a := range suggestion.Assignments {
				if v, err := a.Value.Int64(); err == nil {
					trial.Spec.Assignments = append(trial.Spec.Assignments, cordeliav1alpha1.Assignment{
						Name:  a.ParameterName,
						Value: v,
					})
				}
			}

			// Create the trial
			// TODO If there is an error, notify server that we failed to adopt the suggestion?
			log.Info("Creating new trial", "namespace", trial.Namespace, "reportTrialURL", reportTrialURL, "assignments", trial.Spec.Assignments)
			err = r.Create(context.TODO(), trial)
			return reconcile.Result{}, err
		}
	}

	// Reconcile each trial
	for _, t := range list.Items {
		// TODO Check if the trial has a remote server annotation, if not, we need to manually create the trial on the server before we can report it
		if cordeliatrial.IsTrialFinished(&t) {
			if t.DeletionTimestamp == nil {
				// Delete the trial to force finalization
				err = r.Delete(context.TODO(), &t)
				return reconcile.Result{}, err
			} else {
				// Create an observation for the remote server
				trialValues := cordeliaapi.TrialValues{}
				for _, c := range t.Status.Conditions {
					if c.Type == cordeliav1alpha1.TrialFailed && c.Status == corev1.ConditionTrue {
						trialValues.Failed = true
					}
				}
				for _, v := range t.Spec.Values {
					if fv, err := strconv.ParseFloat(v.Value, 64); err == nil {
						trialValues.Values = append(trialValues.Values, cordeliaapi.Value{
							MetricName: v.Name,
							Value:      fv,
							// TODO Error is the standard deviation for the metric
						})
					}
				}

				// Send the observation to the server
				reportTrialURL := t.GetAnnotations()[annotationReportTrialURL]
				log.Info("Reporting trial", "namespace", t.Namespace, "reportTrialURL", reportTrialURL, "assignments", t.Spec.Assignments, "values", trialValues.Values)
				if err := r.api.ReportTrial(context.TODO(), reportTrialURL, trialValues); err != nil {
					// This error only matters if the experiment itself is not deleted, otherwise ignore it so we can remove the finalizer
					if experiment.DeletionTimestamp == nil {
						return reconcile.Result{}, err
					}
				}

				// Remove the trial finalizer once we have sent information to the server
				for i, _ := range t.Finalizers {
					if t.Finalizers[i] == finalizer {
						t.Finalizers[i] = t.Finalizers[len(t.Finalizers)-1]
						t.Finalizers = t.Finalizers[:len(t.Finalizers)-1]
						err := r.Update(context.TODO(), &t)
						return reconcile.Result{}, err
					}
				}
			}
		} else if t.DeletionTimestamp != nil {
			// The trial was explicitly deleted before it finished, remove the finalizer so it can go away
			for i, _ := range t.Finalizers {
				if t.Finalizers[i] == finalizer {
					// TODO Notify the server that the trial was abandoned
					t.Finalizers[i] = t.Finalizers[len(t.Finalizers)-1]
					t.Finalizers = t.Finalizers[:len(t.Finalizers)-1]
					err := r.Update(context.TODO(), &t)
					return reconcile.Result{}, err
				}
			}
		} else if experiment.DeletionTimestamp != nil {
			// The experiment is deleted, delete the trial as well
			err = r.Delete(context.TODO(), &t)
			return reconcile.Result{}, err
		}
	}

	// Remove our finalizer if we have been deleted and all trials were reconciled
	if experiment.DeletionTimestamp != nil {
		for i, _ := range experiment.Finalizers {
			if experiment.Finalizers[i] == finalizer {
				experiment.Finalizers[i] = experiment.Finalizers[len(experiment.Finalizers)-1]
				experiment.Finalizers = experiment.Finalizers[:len(experiment.Finalizers)-1]
				err := r.Update(context.TODO(), experiment)
				return reconcile.Result{}, err
			}
		}
	}

	// No action
	return reconcile.Result{}, nil
}

func addFinalizer(experiment *cordeliav1alpha1.Experiment) bool {
	if experiment.DeletionTimestamp != nil {
		return false
	}
	for _, f := range experiment.Finalizers {
		if f == finalizer {
			return false
		}
	}
	experiment.Finalizers = append(experiment.Finalizers, finalizer)
	return true
}

func (r *ReconcileExperiment) syncWithServer(experiment *cordeliav1alpha1.Experiment) (bool, error) {
	experimentURL := experiment.GetAnnotations()[annotationExperimentURL]

	if experiment.GetReplicas() > 0 {
		// Define the experiment on the server
		if experimentURL == "" {
			n := cordeliaapi.NewExperimentName(experiment.Name)
			e := cordeliaapi.Experiment{}
			copyExperimentToRemote(experiment, &e)

			log.Info("Creating remote experiment", "name", n)
			if ee, err := r.api.CreateExperiment(context.TODO(), n, e); err == nil {
				experiment.GetAnnotations()[annotationExperimentURL] = ee.Self
				experiment.GetAnnotations()[annotationNextTrialURL] = ee.NextTrial
				if experiment.GetReplicas() > int(ee.Optimization.ParallelTrials) && ee.Optimization.ParallelTrials > 0 {
					*experiment.Spec.Replicas = ee.Optimization.ParallelTrials
				}
				return true, nil
			} else {
				return false, err
			}
		}
	}

	// Notify the server of the deletion
	if experiment.DeletionTimestamp != nil && experimentURL != "" {
		if err := r.api.DeleteExperiment(context.TODO(), experimentURL); err != nil {
			log.Error(err, "Failed to delete experiment", "experimentURL", experimentURL)
		}
		delete(experiment.GetAnnotations(), annotationExperimentURL)
		delete(experiment.GetAnnotations(), annotationNextTrialURL)
		experiment.SetReplicas(0)
		return true, nil
	}

	return false, nil
}

// Copy the custom resource state into a client API representation
func copyExperimentToRemote(experiment *cordeliav1alpha1.Experiment, e *cordeliaapi.Experiment) {
	e.Optimization = cordeliaapi.Optimization{}
	if experiment.Spec.Parallelism != nil {
		e.Optimization.ParallelTrials = *experiment.Spec.Parallelism
	} else {
		e.Optimization.ParallelTrials = int32(experiment.GetReplicas())
	}

	e.Parameters = nil
	for _, p := range experiment.Spec.Parameters {
		e.Parameters = append(e.Parameters, cordeliaapi.Parameter{
			Type: cordeliaapi.ParameterTypeInteger,
			Name: p.Name,
			Bounds: cordeliaapi.Bounds{
				Min: json.Number(strconv.FormatInt(p.Min, 10)),
				Max: json.Number(strconv.FormatInt(p.Max, 10)),
			},
		})
	}

	e.Metrics = nil
	for _, m := range experiment.Spec.Metrics {
		e.Metrics = append(e.Metrics, cordeliaapi.Metric{
			Name:     m.Name,
			Minimize: m.Minimize,
		})
	}
}

// Creates a new trial for an experiment
func populateTrialFromTemplate(experiment *cordeliav1alpha1.Experiment, trial *cordeliav1alpha1.Trial, namespace string) {
	// Start with the trial template
	experiment.Spec.Template.ObjectMeta.DeepCopyInto(&trial.ObjectMeta)
	experiment.Spec.Template.Spec.DeepCopyInto(&trial.Spec)

	// Overwrite the target namespace unless we are only running a single trial on the cluster
	if experiment.GetReplicas() > 1 || experiment.Spec.NamespaceSelector != nil || experiment.Spec.Template.Namespace != "" {
		trial.Spec.TargetNamespace = namespace
	}

	trial.Finalizers = append(trial.Finalizers, finalizer)

	if trial.Namespace == "" {
		trial.Namespace = namespace
	}

	if trial.Name == "" {
		if trial.Namespace != experiment.Namespace {
			trial.Name = experiment.Name
		} else if trial.GenerateName == "" {
			trial.GenerateName = experiment.Name + "-"
		}
	}

	if len(trial.Labels) == 0 {
		trial.Labels = experiment.GetDefaultLabels()
	}

	if trial.Annotations == nil {
		trial.Annotations = make(map[string]string)
	}

	if trial.Spec.ExperimentRef == nil {
		trial.Spec.ExperimentRef = experiment.GetSelfReference()
	}
}

// Searches for a namespace to run a new trial in, returning an empty string if no such namespace can be found
func (r *ReconcileExperiment) findAvailableNamespace(experiment *cordeliav1alpha1.Experiment, trials []cordeliav1alpha1.Trial) (string, error) {
	// Determine which namespaces are already in use
	inuse := make(map[string]bool, len(trials))
	for _, t := range trials {
		inuse[t.Namespace] = true
	}

	// Find eligible namespaces
	if experiment.Spec.NamespaceSelector != nil {
		ls, err := metav1.LabelSelectorAsSelector(experiment.Spec.NamespaceSelector)
		if err != nil {
			return "", err
		}
		list := &corev1.NamespaceList{}
		if err := r.List(context.TODO(), list, client.UseListOptions(&client.ListOptions{LabelSelector: ls})); err != nil {
			return "", err
		}

		// Find the first available namespace
		for _, item := range list.Items {
			if !inuse[item.Name] {
				return item.Name, nil
			}
		}
		return "", nil
	}

	// Check if the experiment namespace is available
	if inuse[experiment.Namespace] {
		return "", nil
	}
	return experiment.Namespace, nil
}
