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

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	optimizev1beta2 "github.com/thestormforge/optimize-controller/v2/api/v1beta2"
	"github.com/thestormforge/optimize-controller/v2/controllers"
	"github.com/thestormforge/optimize-controller/v2/internal/version"
	"github.com/thestormforge/optimize-go/pkg/config"
	zap2 "go.uber.org/zap"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	_ = clientgoscheme.AddToScheme(scheme)

	_ = optimizev1beta2.AddToScheme(scheme)
	// +kubebuilder:scaffold:scheme
}

func main() {
	// Make it possible to just print the version or configuration and exit
	handleDebugArgs()

	var metricsAddr string
	var enableLeaderElection bool
	flag.StringVar(&metricsAddr, "metrics-addr", ":8080", "The address the metric endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "enable-leader-election", false,
		"Enable leader election for controller manager. Enabling this will ensure there is only one active controller manager.")
	flag.Parse()

	ctrl.SetLogger(zap.New(func(o *zap.Options) {
		o.Development = false

		// Disable stacktraces in most instances
		stl := zap2.NewAtomicLevelAt(zap2.FatalLevel)
		o.StacktraceLevel = &stl
	}))

	v := version.GetInfo()
	setupLog.Info("StormForge Optimize Controller", "version", v.String(), "gitCommit", v.GitCommit)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:             scheme,
		MetricsBindAddress: metricsAddr,
		LeaderElection:     enableLeaderElection,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controllers.ExperimentReconciler{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("Experiment"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Experiment")
		os.Exit(1)
	}
	if err = (&controllers.ServerReconciler{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("Server"),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Server")
		os.Exit(1)
	}
	if err = (&controllers.SetupReconciler{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("Setup"),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Setup")
		os.Exit(1)
	}
	if err = (&controllers.PatchReconciler{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("Patch"),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Patch")
		os.Exit(1)
	}
	if err = (&controllers.ReadyReconciler{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("Ready"),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Ready")
		os.Exit(1)
	}
	if err = (&controllers.TrialJobReconciler{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("Trial"),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Trial")
		os.Exit(1)
	}
	if err = (&controllers.MetricReconciler{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("Metric"),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Metric")
		os.Exit(1)
	}

	// +kubebuilder:scaffold:builder

	poller, err := controllers.NewPoller(
		mgr.GetClient(),
		mgr.GetConfig(),
		mgr.GetRESTMapper(),
		mgr.GetScheme(),
		ctrl.Log.WithName("controllers").WithName("application poller"),
	)
	if err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "application poller")
		os.Exit(1)
	}

	mgr.Add(poller)

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// handleDebugArgs will make the process dump and exit if the first arg is either "version" or "config"
func handleDebugArgs() {
	if len(os.Args) > 1 {
		if os.Args[1] == "version" {
			if output, err := json.Marshal(version.GetInfo()); err != nil {
				os.Exit(1)
			} else {
				fmt.Println(string(output))
				os.Exit(0)
			}
		} else if os.Args[1] == "config" {
			// TODO Host live values from the in-memory configuration at `.../debug/config` instead of this
			cfg := &config.OptimizeConfig{}
			if err := cfg.Load(); err != nil {
				os.Exit(1)
			}
			minified, err := config.Minify(cfg.Reader())
			if err != nil {
				os.Exit(1)
			}
			output, err := json.Marshal(minified)
			if err != nil {
				os.Exit(1)
			}
			fmt.Println(string(output))
			os.Exit(0)
		}
	}
}
