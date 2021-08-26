/*
Copyright 2020 GramLabs, Inc.

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
	"crypto/rand"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/go-logr/logr"
	"github.com/oklog/ulid"
	optimizeappsv1alpha1 "github.com/thestormforge/optimize-controller/v2/api/apps/v1alpha1"
	optimizev1beta2 "github.com/thestormforge/optimize-controller/v2/api/v1beta2"
	"github.com/thestormforge/optimize-controller/v2/internal/experiment"
	"github.com/thestormforge/optimize-controller/v2/internal/scan"
	"github.com/thestormforge/optimize-controller/v2/internal/server"
	"github.com/thestormforge/optimize-controller/v2/internal/sfio"
	"github.com/thestormforge/optimize-controller/v2/internal/version"
	"github.com/thestormforge/optimize-go/pkg/api"
	applications "github.com/thestormforge/optimize-go/pkg/api/applications/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metameta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Poller handles checking with the Application Services to trigger an in cluster
// activity such as scanning resources or running an experiment.
type Poller struct {
	client        client.Client
	log           logr.Logger
	apiClient     applications.API
	kubectlExecFn func(cmd *exec.Cmd) ([]byte, error)

	config     *rest.Config
	metaMapper metameta.RESTMapper
	scheme     *runtime.Scheme
}

// NewPoller returns a new Poller with the given Kubernetes client, logger,
// and application api client configured.
func NewPoller(kclient client.Client, config *rest.Config, mapper metameta.RESTMapper, scheme *runtime.Scheme, logger logr.Logger) (*Poller, error) {
	appAPI, err := server.NewApplicationAPI(context.Background(), version.GetInfo().String())
	if err != nil {
		logger.Info("Application API is unavailable, skipping setup", "message", err.Error())
		return &Poller{log: logger}, nil
	}

	return &Poller{
		client:     kclient,
		apiClient:  appAPI,
		log:        logger,
		config:     config,
		metaMapper: mapper,
		scheme:     scheme,
	}, nil
}

// Start is used to initiate the polling loop for new tasks.
// Start satisfies the controller-runtime/manager.Runnable interface so we
// can plug into the underlying controller runtime manager that the rest of the
// controllers use.
// If there was an issue connecting to the application services, this will immediately
// return.
func (p *Poller) Start(ch <-chan struct{}) error {
	if p.apiClient == nil {
		return nil
	}
	p.log.Info("Starting application poller")

	ctx, cancel := context.WithCancel(context.Background())

	// Handle controller-manager signal
	go func() {
		for range ch {
			cancel()
		}
	}()

	query := applications.ActivityFeedQuery{}
	query.SetType(applications.TagScan, applications.TagRun)
	subscriber, err := p.apiClient.SubscribeActivity(ctx, query)
	if err != nil {
		p.log.Error(err, "Unable to connect to application service")
		return nil
	}

	for {
		activityCh := make(chan applications.ActivityItem)

		go func() {
			for activityItem := range activityCh {
				p.handleActivity(ctx, activityItem)
			}
		}()

		if err := subscriber.Subscribe(ctx, activityCh); err != nil {
			// Clean exit/shutdown
			if errors.Is(err, context.Canceled) {
				return nil
			}

			p.log.Error(err, "Application service connection interrupted, restarting")

			continue
		}

		// Clean exit from subscriber should end here
		return nil
	}
}

// handleActivity performs the task required for each activity.
// When an ActivityItem is tagged with scan, the generation workflow is used to generate an experiment and the result
// is converted into an api.Template consisting of parameters and metrics.
// When an ActivityItem is tagged with run, the previous scanned template results are merged with
// the results of an experiment generation workflow. Following this, the generated resources are applied/created
// in the cluster.
// note, rbac defined in cli/internal/commands/grant_permissions/generator
func (p *Poller) handleActivity(ctx context.Context, activity applications.ActivityItem) {
	const (
		ActivityReasonInvalidApplication = "InvalidApplication"
		ActivityReasonGenerationFailed   = "GenerationFailed"
		ActivityReasonScanFailed         = "ScanFailed"
		ActivityReasonRunFailed          = "RunFailed"
	)
	log := p.log.WithValues(
		"activityId", activity.ID,
		"activityTags", strings.Join(activity.Tags, ", "),
		"activityURL", activity.URL,
	)

	// Ensure we actually have an action to perform
	if activity.ID == "" || len(activity.Tags) != 1 {
		log.Info("Ignoring invalid activity")
		return
	}

	// We always want to delete the activity after having received it
	defer func() {
		if err := p.apiClient.DeleteActivity(ctx, activity.URL); err != nil {
			log.Error(err, "Failed to delete activity")
		}
	}()

	log.Info("Starting activity task")

	// Activity feed provides us with a scenario URL
	scenario, err := p.apiClient.GetScenario(ctx, activity.ExternalURL)
	if err != nil {
		log = log.WithValues("activityExternalURL", activity.ExternalURL)
		p.handleErrors(ctx, log, activity.URL, ActivityReasonInvalidApplication, "Failed to get scenario", err)
		return
	}

	// Need to fetch top level application so we can get the resources
	applicationURL := scenario.Link(api.RelationUp)
	if applicationURL == "" {
		p.handleErrors(ctx, log, activity.URL, ActivityReasonInvalidApplication, "No matching application URL for scenario", nil)
		return
	}

	templateURL := scenario.Link(api.RelationTemplate)
	if templateURL == "" {
		p.handleErrors(ctx, log, activity.URL, ActivityReasonInvalidApplication, "No matching template URL for scenario", nil)
		return
	}

	experimentURL := scenario.Link(api.RelationExperiments)
	if experimentURL == "" {
		p.handleErrors(ctx, log, activity.URL, ActivityReasonInvalidApplication, "No matching experiment URL for scenario", nil)
		return
	}

	apiApp, err := p.apiClient.GetApplication(ctx, applicationURL)
	if err != nil {
		p.handleErrors(ctx, log, activity.URL, ActivityReasonInvalidApplication, "Failed to get application", err)
		return
	}

	var assembledApp *optimizeappsv1alpha1.Application
	if assembledApp, err = server.APIApplicationToClusterApplication(apiApp, scenario); err != nil {
		p.handleErrors(ctx, log, activity.URL, ActivityReasonGenerationFailed, "Failed to assemble application", err)
		return
	}

	// Use resource namespaces for application namespace.
	// We'll attempt to discover the namespace for the experiment by looking at
	// 1. resource.Namespace
	// 2. resource.Namespaces[0]
	// 3. #TODO? We may look at evaluating namespace selector since we have
	//    access to the kube client
	assembledApp.Namespace = "default"

	for i := range assembledApp.Resources {
		k := assembledApp.Resources[i].Kubernetes
		if k == nil {
			continue
		}

		// Guess the namespace based on the Kubernetes resource
		if k.Namespace != "" {
			assembledApp.Namespace = k.Namespace
			break
		}

		if len(k.Namespaces) > 0 && k.Namespaces[0] != "" {
			assembledApp.Namespace = k.Namespaces[0]
			break
		}
	}

	generatedResources, err := p.generateApp(*assembledApp)
	if err != nil {
		p.handleErrors(ctx, log, activity.URL, ActivityReasonGenerationFailed, "Failed to generate application", err)
		return
	}

	var exp *optimizev1beta2.Experiment
	for i := range generatedResources {
		if expObj, ok := generatedResources[i].(*optimizev1beta2.Experiment); ok {
			exp = expObj

			metav1.SetMetaDataAnnotation(&exp.ObjectMeta, optimizev1beta2.AnnotationExperimentURL, strings.TrimRight(experimentURL, "/")+"/"+exp.Name)

			break
		}
	}

	if exp == nil {
		p.handleErrors(ctx, log, activity.URL, "GenerationFailed", "Invalid experiment generated", err)
		return
	}

	switch activity.Tags[0] {
	case applications.TagScan:

		template, err := server.ClusterExperimentToAPITemplate(exp)
		if err != nil {
			p.handleErrors(ctx, log, activity.URL, ActivityReasonScanFailed, "Failed to convert experiment template", err)
			return
		}

		if err := p.apiClient.UpdateTemplate(ctx, templateURL, *template); err != nil {
			p.handleErrors(ctx, log, activity.URL, ActivityReasonScanFailed, "Failed to save experiment template in server", err)
			return
		}

		log.Info("Successfully completed resource scan")
	case applications.TagRun:

		// We wont compare existing scan with current scan
		// so we can preserve changes via UI

		// Get previous template
		previousTemplate, err := p.apiClient.GetTemplate(ctx, templateURL)
		if err != nil {
			p.handleErrors(ctx, log, activity.URL, ActivityReasonRunFailed, "Failed to get experiment template from server, a 'scan' task must be completed first", err)
			return
		}

		// Overwrite current scan results with previous scan results
		if err = server.APITemplateToClusterExperiment(exp, &previousTemplate); err != nil {
			p.handleErrors(ctx, log, activity.URL, ActivityReasonRunFailed, "Failed to convert experiment template", err)
			return
		}

		// At this point the experiment should be good to create/deploy/run
		// so let's create all the resources and #profit

		// TODO
		// try to clean up on failure ( might be a simple / blind p.client.Delete(ctx,generatedResources[i])
		for i := range generatedResources {
			objKey, err := client.ObjectKeyFromObject(generatedResources[i])
			if err != nil {
				p.handleErrors(ctx, log, activity.URL, ActivityReasonRunFailed, "Failed to get object key", err)
				return
			}

			holder := &unstructured.Unstructured{}
			holder.SetGroupVersionKind(generatedResources[i].GetObjectKind().GroupVersionKind())
			err = p.client.Get(ctx, objKey, holder)
			switch {
			case apierrors.IsNotFound(err):
				if err := p.client.Create(ctx, generatedResources[i]); err != nil {
					p.handleErrors(ctx, log, activity.URL, ActivityReasonRunFailed, "Failed to create object", err)
					return
				}
			case err == nil:
				// Most of this gets handled properly for core resources in kube, but seems like there is a gap around
				// CRD handling. ref: https://github.com/kubernetes/kubernetes/issues/70674
				metameta.NewAccessor().SetResourceVersion(generatedResources[i], holder.GetResourceVersion())
				if err := p.client.Update(ctx, generatedResources[i]); err != nil {
					p.handleErrors(ctx, log, activity.URL, ActivityReasonRunFailed, "Failed to update object", err)
					return
				}
			default:
				// Assume this should be a hard error
				p.handleErrors(ctx, log, activity.URL, ActivityReasonRunFailed, "Failed to get object", err)
				return
			}
		}

		log.Info("Successfully created in cluster resources")
	}
}

func (p *Poller) handleErrors(ctx context.Context, log logr.Logger, u, reason, message string, err error) {
	if reason == "" {
		panic("must supply a reason to handleErrors")
	}

	msg := message
	if err != nil {
		msg = fmt.Errorf("%s: %w", msg, err).Error()
	}
	log.Info("Activity task failed", "failureReason", reason, "failureMessage", msg)

	if err := p.apiClient.PatchApplicationActivity(ctx, u, applications.ActivityFailure{FailureReason: reason, FailureMessage: msg}); err != nil {
		log.Error(err, "Failed to update application activity")
	}
}

func (p *Poller) generateApp(app optimizeappsv1alpha1.Application) ([]runtime.Object, error) {
	// Set defaults for application
	app.Default()

	g := &experiment.Generator{
		Application:    app,
		ExperimentName: strings.ToLower(ulid.MustNew(ulid.Now(), rand.Reader).String()),
		FilterOptions: scan.FilterOptions{
			KubectlExecutor: p.kubectlExecFn,
			MinikubeOpts:    []scan.Option{scan.WithRESTClient(p.config, p.metaMapper)},
		},
	}

	objList := sfio.ObjectList{}
	if err := g.Execute(&objList); err != nil {
		return nil, fmt.Errorf("%s: %w", "failed to generate experiment", err)
	}

	runtimeObjs := make([]runtime.Object, 0, len(objList.Items))
	for i := range objList.Items {
		runtimeObjs = append(runtimeObjs, objList.Items[i].Object)
	}

	return runtimeObjs, nil
}
