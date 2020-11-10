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

package experiment

import (
	"fmt"
	"strings"

	"github.com/redskyops/redskyops-controller/api/apps/v1alpha1"
	redskyv1beta1 "github.com/redskyops/redskyops-controller/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
)

var zero = resource.MustParse("0")

const requestsQueryFormat = `({{ cpuRequests . "%s" }} * %d) + ({{ memoryRequests . "%s" | GB }} * %d)`

func (g *Generator) addObjectives(list *corev1.List) error {
	for _, obj := range g.Application.Objectives {
		switch {

		case obj.Requests != nil:
			addRequestsMetric(&obj, list)

		}
	}

	return nil
}

func addRequestsMetric(obj *v1alpha1.Objective, list *corev1.List) {
	lbl := labels.Set(obj.Requests.Labels).String()

	cpuWeight := obj.Requests.Weights.Cpu()
	if cpuWeight == nil {
		cpuWeight = &zero
	}

	memoryWeight := obj.Requests.Weights.Memory()
	if memoryWeight == nil {
		memoryWeight = &zero
	}

	// Add the cost metric to the experiment
	exp := findOrAddExperiment(list)
	exp.Spec.Metrics = append(exp.Spec.Metrics, redskyv1beta1.Metric{
		Name:     obj.Name,
		Minimize: true,
		Type:     redskyv1beta1.MetricPrometheus,
		Query:    fmt.Sprintf(requestsQueryFormat, lbl, cpuWeight.Value(), lbl, memoryWeight.Value()),
		Min:      obj.Min,
		Max:      obj.Max,
		Optimize: obj.Optimize,
	})

	// If the name contains "cost" and the weights are non-zero, add non-optimized metrics for each request
	if strings.Contains(obj.Name, "cost") && !cpuWeight.IsZero() && !memoryWeight.IsZero() && (obj.Optimize == nil || *obj.Optimize) {
		nonOptimized := false
		exp.Spec.Metrics = append(exp.Spec.Metrics, redskyv1beta1.Metric{
			Name:     obj.Name + "-cpu-requests",
			Minimize: true,
			Optimize: &nonOptimized,
			Type:     redskyv1beta1.MetricPrometheus,
			Query:    fmt.Sprintf("{{ cpuRequests . \"%s\" }}", lbl),
		}, redskyv1beta1.Metric{
			Name:     obj.Name + "-memory-requests",
			Minimize: true,
			Optimize: &nonOptimized,
			Type:     redskyv1beta1.MetricPrometheus,
			Query:    fmt.Sprintf("{{ memoryRequests . \"%s\" }}", lbl), // TODO Convert to GB?
		})
	}

	// The cost metric requires Prometheus
	ensurePrometheus(list)
}

// addStormForgerObjectives adds metrics for objectives supported by StormForger.
func addStormForgerObjectives(app *v1alpha1.Application, list *corev1.List) error {
	for _, obj := range app.Objectives {
		switch {

		case obj.Latency != nil:
			addStormForgerLatencyMetric(&obj, list)

		}
	}

	return nil
}

func addStormForgerLatencyMetric(obj *v1alpha1.Objective, list *corev1.List) {
	var m string
	switch v1alpha1.FixLatency(obj.Latency.LatencyType) {
	case v1alpha1.LatencyMinimum:
		m = "min"
	case v1alpha1.LatencyMaximum:
		m = "max"
	case v1alpha1.LatencyMean:
		m = "mean"
	case v1alpha1.LatencyPercentile50:
		m = "median"
	case v1alpha1.LatencyPercentile95:
		m = "percentile_95"
	case v1alpha1.LatencyPercentile99:
		m = "percentile_99"
	default:
		// This is not a latency measure that StormForger can produce, skip it
		return
	}

	// Filter the metric to match what was sent to the Push Gateway
	exp := findOrAddExperiment(list)
	exp.Spec.Metrics = append(exp.Spec.Metrics, redskyv1beta1.Metric{
		Name:     obj.Name,
		Minimize: true,
		Type:     redskyv1beta1.MetricPrometheus,
		Query:    m + `{job="trialRun",instance="{{ .Trial.Name }}"}`,
		Min:      obj.Min,
		Max:      obj.Max,
		Optimize: obj.Optimize,
	})
}