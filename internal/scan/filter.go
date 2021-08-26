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
	"io"
	"os/exec"

	"github.com/spf13/pflag"
	"github.com/thestormforge/konjure/pkg/konjure"
	"sigs.k8s.io/kustomize/api/filesys"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/types"
)

// FilterOptions allow the behavior of the Konjure filter to be customized.
type FilterOptions struct {
	DefaultReader     io.Reader
	KubectlExecutor   func(cmd *exec.Cmd) ([]byte, error)
	KustomizeExecutor func(cmd *exec.Cmd) ([]byte, error)
	MinikubeOpts      []Option
}

// NewFilter creates a new filter with the supplied working directory.
func (o *FilterOptions) NewFilter(workingDirectory string) *konjure.Filter {
	f := &konjure.Filter{
		Depth:             100,
		KeepStatus:        true,
		WorkingDirectory:  workingDirectory,
		DefaultReader:     o.DefaultReader,
		KubectlExecutor:   o.KubectlExecutor,
		KustomizeExecutor: o.KustomizeExecutor,
	}

	if f.KubectlExecutor == nil {
		f.KubectlExecutor = kubectl(o.MinikubeOpts...)
	}

	if f.KustomizeExecutor == nil {
		f.KustomizeExecutor = kustomize
	}

	return f
}

func kubectl(minikubeOpts ...Option) func(*exec.Cmd) ([]byte, error) {
	return func(cmd *exec.Cmd) ([]byte, error) {
		// If LookPath found the kubectl binary, it is safer to just use it. That
		// way the cluster version doesn't need to be in the compatibility range of
		// whatever client-go we were compiled with.
		if cmd.Path != "kubectl" {
			return cmd.Output()
		}

		// Kustomize has a clown. We have minikubectl.
		k, err := newMinikubectl(minikubeOpts...)
		if err != nil {
			return nil, err
		}

		// Create and populate a new flag set
		flags := pflag.NewFlagSet("minikubectl", pflag.ContinueOnError)
		k.AddFlags(flags)

		// Parse the arguments on exec.Cmd (ignoring arg[0] which is "kubectl")
		if err := flags.Parse(cmd.Args[1:]); err != nil {
			return nil, err
		}

		// If complete fails, assume it was because we asked too much of minikubectl
		// and we should just run the real thing in a subprocess
		if err := k.Complete(flags.Args()); err != nil {
			return cmd.Output()
		}

		// Run minikubectl with the remaining arguments
		return k.Run(flags.Args())
	}
}

func kustomize(cmd *exec.Cmd) ([]byte, error) {
	// If the command path is absolute, it was found by LookPath. In this
	// case we will just fork so the embedded version of Kustomize does not
	// becoming a limiting factor.

	// We are only handling the very specific case of `kustomize build X`
	if len(cmd.Args) != 3 || cmd.Path != "kustomize" || cmd.Args[1] != "build" {
		return cmd.Output()
	}

	// Restrict to disk access to be consistent with the flow when we fork
	fs := filesys.MakeFsOnDisk()

	// Create Krusty options
	opts := &krusty.Options{
		LoadRestrictions: types.LoadRestrictionsNone,
		PluginConfig:     types.DisabledPluginConfig(),
	}

	// Run the Kustomization in process
	rm, err := krusty.MakeKustomizer(opts).Run(fs, cmd.Args[1])
	if err != nil {
		return nil, err
	}

	return rm.AsYaml()
}
