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

// wtf. https://github.com/golang/tools/blob/master/go/analysis/passes/buildtag/buildtag.go#L47-L51
// // +build ignore

// Generator handles embedding non-go files into the compiled binary.
// Embedded assets are gzipped and base64 encoded.
// This makes use of redskyctl/internal/kustomize/assets.go to interact with the
// embedded data.
package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io/ioutil"
	"log"
	"strings"

	flag "github.com/spf13/pflag"
	"sigs.k8s.io/kustomize/api/filesys"
	"sigs.k8s.io/kustomize/api/krusty"
)

func main() {
	var (
		path          = flag.String("path", ".", "path of kustomization.yaml")
		outputPackage = flag.String("package", "generated", "output package name")
		outputFile    = flag.String("output", "output.go", "output filename")
		headerFile    = flag.String("header", "", "optional header file to include at the top of the generated file")
		prefix        = flag.String("prefix", "zz_generated", "output file prefix")
	)

	flag.Parse()

	var outputBuffer bytes.Buffer
	if *headerFile != "" {
		h, err := ioutil.ReadFile(*headerFile)
		if err != nil {
			log.Fatal("failed to read header file")
		}
		outputBuffer.Write(h)
	}
	outputBuffer.WriteString("\n// Code generated by cmd/generator, DO NOT EDIT.\n")
	outputBuffer.WriteString(fmt.Sprintf("package %s\n\n", *outputPackage))

	// Run `kustomize build` against path
	k := krusty.MakeKustomizer(filesys.MakeFsOnDisk(), krusty.MakeDefaultOptions())
	resources, err := k.Run(*path)
	if err != nil {
		log.Fatal("failed to run kustomize build:", err)
	}

	yamls, err := resources.AsYaml()
	if err != nil {
		log.Fatal("failed to get kustomize yamls:", err)
	}

	// Compress data
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err = zw.Write(yamls); err != nil {
		log.Fatal("failed to gzip yamls:", err)
	}

	zw.Close()

	outputBuffer.WriteString("// The below is a gzipped encoded yaml\n")
	outputBuffer.WriteString(fmt.Sprintf("var %s = Asset{data: []byte(%q)}\n", "kustomizeBase", buf.Bytes()))

	if err := ioutil.WriteFile(strings.Join([]string{*prefix, *outputFile}, "."), outputBuffer.Bytes(), 0644); err != nil {
		log.Fatal("failed to write output file")
	}
}
