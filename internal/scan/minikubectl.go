/*
Copyright 2021 GramLabs, Inc.

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

package scan

import (
	"bytes"
	"fmt"

	"github.com/spf13/pflag"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

type minikubectl struct {
	*genericclioptions.ConfigFlags
	*genericclioptions.ResourceBuilderFlags
	*genericclioptions.PrintFlags
	IgnoreNotFound bool
	Manager        *miniManager
}

// newMinikubectl returns a lite-kubectl
func newMinikubectl(setters ...Option) (*minikubectl, error) {
	// Default interface setup
	minikubectl := defaultOptions()

	// Configure interface with any specified options
	for _, setter := range setters {
		if err := setter(minikubectl); err != nil {
			return nil, err
		}
	}

	return minikubectl, nil
}

// AddFlags configures the supplied flag set with the recognized flags.
func (m *minikubectl) AddFlags(flags *pflag.FlagSet) {
	m.ConfigFlags.AddFlags(flags)
	m.ResourceBuilderFlags.AddFlags(flags)

	// PrintFlags somehow is tied to Cobra...
	flags.StringVarP(m.PrintFlags.OutputFormat, "output", "o", *m.PrintFlags.OutputFormat, "")

	// Don't bother with usage strings here, we aren't showing help to anyone
	flags.BoolVar(&m.IgnoreNotFound, "ignore-not-found", m.IgnoreNotFound, "")
}

// Complete validates we can execute against the supplied arguments.
func (m *minikubectl) Complete(args []string) error {
	if len(args) == 0 || args[0] != "get" {
		return fmt.Errorf("minikubectl only supports get")
	}

	return nil
}

// Run executes the supplied arguments and returns the output as bytes.
func (m *minikubectl) Run(args []string) ([]byte, error) {
	var rcg genericclioptions.RESTClientGetter
	if m.Manager != nil {
		rcg = m.Manager
	} else {
		rcg = m.ConfigFlags
	}

	r := m.ResourceBuilderFlags.ToBuilder(rcg, args[1:])
	v := r.Do()

	// Create a printer to dump the objects
	printer, err := m.PrintFlags.ToPrinter()
	if err != nil {
		return nil, err
	}

	// Use the printer to render everything into a byte buffer
	var b bytes.Buffer
	err = v.Visit(func(info *resource.Info, err error) error {
		if err != nil {
			if m.IgnoreNotFound && apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}

		return printer.PrintObj(info.Object, &b)
	})
	if err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

// Option is a function that can be used to configure minikubectl
type Option func(*minikubectl) error

func defaultOptions() *minikubectl {
	outputFormat := ""
	return &minikubectl{
		ConfigFlags: genericclioptions.NewConfigFlags(false),
		ResourceBuilderFlags: genericclioptions.NewResourceBuilderFlags().
			WithLabelSelector("").
			WithAll(true).
			WithLatest(),
		PrintFlags: &genericclioptions.PrintFlags{
			JSONYamlPrintFlags: genericclioptions.NewJSONYamlPrintFlags(),
			OutputFormat:       &outputFormat,
		},
	}
}

// WithRESTClient can be used to provide an alternative configuration for minikubectl
func WithRESTClient(cfg *rest.Config, mapper meta.RESTMapper) Option {
	return func(m *minikubectl) (err error) {
		m.Manager = &miniManager{
			config: cfg,
			mapper: mapper,
			ns:     *m.ConfigFlags.Namespace,
		}

		return nil
	}
}

// miniManager is used to use what we can from controller-runtime.Manager and satisfy the
// RESTClientGetter interface so we dont need to use another kube client
type miniManager struct {
	config *rest.Config
	mapper meta.RESTMapper
	ns     string
}

// ToRESTConfig implements the RESTClientGetter interface
func (m *miniManager) ToRESTConfig() (*rest.Config, error) {
	return m.config, nil
}

// ToDiscoveryClient implements the RESTClientGetter interface
func (m *miniManager) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	return memory.NewMemCacheClient(discovery.NewDiscoveryClientForConfigOrDie(m.config)), nil
}

// ToRESTMapper implements the RESTClientGetter interface
func (m *miniManager) ToRESTMapper() (meta.RESTMapper, error) {
	return m.mapper, nil
}

// ToRawKubeConfigLoader implements the RESTClientGetter interface
func (m *miniManager) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	// Only ClientConfig.Namespace() is used from cli builder, so we need something
	// dumb here
	return &dumberConfig{ns: m.ns}
}

// dumberConfig implments the ClientConfig interface
type dumberConfig struct {
	ns string
}

func (d *dumberConfig) RawConfig() (clientcmdapi.Config, error) { return clientcmdapi.Config{}, nil }
func (d *dumberConfig) ClientConfig() (*rest.Config, error)     { return nil, nil }
func (d *dumberConfig) Namespace() (string, bool, error)        { return d.ns, false, nil }
func (d *dumberConfig) ConfigAccess() clientcmd.ConfigAccess    { return nil }
