/*
Copyright 2019 The Kubernetes Authors.

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
	"bufio"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v2"
)

const (
	kubeMetricImportPath = `"k8s.io/component-base/metrics"`
	// Should equal to final directory name of kubeMetricImportPath
	kubeMetricsDefaultImportName = "metrics"

	kubeURLRoot = "k8s.io/kubernetes/"
)

var (
	// env configs
	GOROOT                string = os.Getenv("GOROOT")
	GOOS                  string = os.Getenv("GOOS")
	KUBE_ROOT             string = os.Getenv("KUBE_ROOT")
	ALL_STABILITY_CLASSES bool
)

func main() {

	flag.BoolVar(&ALL_STABILITY_CLASSES, "allstabilityclasses", true, "use this flag to enable all stability classes")
	flag.Parse()
	if len(flag.Args()) < 1 {
		fmt.Fprintf(os.Stderr, "USAGE: %s <DIR or FILE or '-'> [...]\n", os.Args[0])
		os.Exit(64)
	}
	stableMetricNames := map[string]struct{}{}
	stableMetrics := []metric{}
	errors := []error{}

	addStdin := false
	for _, arg := range flag.Args() {
		if arg == "-" {
			addStdin = true
			continue
		}
		ms, es := searchPathForStableMetrics(arg)
		for _, m := range ms {
			if _, ok := stableMetricNames[m.Name]; !ok {
				stableMetrics = append(stableMetrics, m)
			}
			stableMetricNames[m.Name] = struct{}{}
		}
		errors = append(errors, es...)
	}
	if addStdin {
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Split(bufio.ScanLines)
		for scanner.Scan() {
			arg := scanner.Text()
			ms, es := searchPathForStableMetrics(arg)
			stableMetrics = append(stableMetrics, ms...)
			errors = append(errors, es...)
		}
	}

	for _, err := range errors {
		fmt.Fprintf(os.Stderr, "%s\n", err)
	}
	if len(errors) != 0 {
		os.Exit(1)
	}
	if len(stableMetrics) == 0 {
		os.Exit(0)
	}
	for i, m := range stableMetrics {
		if m.StabilityLevel == "" {
			m.StabilityLevel = "ALPHA"
		}
		stableMetrics[i] = m
	}
	sort.Sort(byFQName(stableMetrics))
	data, err := yaml.Marshal(stableMetrics)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		os.Exit(1)
	}

	fmt.Print(string(data))
}

func searchPathForStableMetrics(path string) ([]metric, []error) {
	metrics := []metric{}
	errors := []error{}
	err := filepath.Walk(path, func(path string, info os.FileInfo, err error) error {
		if strings.HasPrefix(path, "vendor") {
			return filepath.SkipDir
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		ms, es := searchFileForStableMetrics(path, nil)
		errors = append(errors, es...)
		metrics = append(metrics, ms...)
		return nil
	})
	if err != nil {
		errors = append(errors, err)
	}
	return metrics, errors
}

// Pass either only filename of existing file or src including source code in any format and a filename that it comes from
func searchFileForStableMetrics(filename string, src interface{}) ([]metric, []error) {
	fileset := token.NewFileSet()
	tree, err := parser.ParseFile(fileset, filename, src, parser.AllErrors)
	if err != nil {
		return []metric{}, []error{err}
	}
	metricsImportName, err := getLocalNameOfImportedPackage(tree, kubeMetricImportPath, kubeMetricsDefaultImportName)
	if err != nil {
		return []metric{}, addFileInformationToErrors([]error{err}, fileset)
	}
	if metricsImportName == "" {
		return []metric{}, []error{}
	}
	variables := globalVariableDeclarations(tree)

	variables, err = importedGlobalVariableDeclaration(variables, tree.Imports)
	if err != nil {
		return []metric{}, addFileInformationToErrors([]error{err}, fileset)
	}

	stableMetricsFunctionCalls, errors := findStableMetricDeclaration(tree, metricsImportName)
	metrics, es := decodeMetricCalls(stableMetricsFunctionCalls, metricsImportName, variables)
	errors = append(errors, es...)
	return metrics, addFileInformationToErrors(errors, fileset)
}

func getLocalNameOfImportedPackage(tree *ast.File, importPath, defaultImportName string) (string, error) {
	var importName string
	for _, im := range tree.Imports {
		if im.Path.Value == importPath {
			if im.Name == nil {
				importName = defaultImportName
			} else {
				if im.Name.Name == "." {
					return "", newDecodeErrorf(im, errImport)
				}
				importName = im.Name.Name
			}
		}
	}
	return importName, nil
}

func addFileInformationToErrors(es []error, fileset *token.FileSet) []error {
	for i := range es {
		if de, ok := es[i].(*decodeError); ok {
			es[i] = de.errorWithFileInformation(fileset)
		}
	}
	return es
}

func globalVariableDeclarations(tree *ast.File) map[string]ast.Expr {
	consts := make(map[string]ast.Expr)
	for _, d := range tree.Decls {
		if gd, ok := d.(*ast.GenDecl); ok && (gd.Tok == token.CONST || gd.Tok == token.VAR) {
			for _, spec := range gd.Specs {
				if vspec, ok := spec.(*ast.ValueSpec); ok {
					for _, name := range vspec.Names {
						for _, value := range vspec.Values {
							consts[name.Name] = value
						}
					}
				}
			}
		}
	}
	return consts
}

func localImportPath(importExpr string) (string, error) {
	// parse directory path
	var pathPrefix string
	if strings.Contains(importExpr, kubeURLRoot) {
		// search k/k local checkout
		pathPrefix = KUBE_ROOT
		importExpr = strings.Replace(importExpr, kubeURLRoot, "", 1)
	} else if strings.Contains(importExpr, "k8s.io/klog/v2") || strings.Contains(importExpr, "k8s.io/util") {
		pathPrefix = strings.Join([]string{KUBE_ROOT, "vendor"}, string(os.PathSeparator))
	} else if strings.Contains(importExpr, "k8s.io/") {
		// search k/k/staging local checkout
		pathPrefix = strings.Join([]string{KUBE_ROOT, "staging", "src"}, string(os.PathSeparator))
	} else if strings.Contains(importExpr, ".") {
		// not stdlib -> prefix with GOMODCACHE
		// pathPrefix = strings.Join([]string{KUBE_ROOT, "vendor"}, string(os.PathSeparator))

		//  this requires implementing SIV, skip for now
		return "", fmt.Errorf("unable to handle general, non STL imports for metric analysis.  import path:  %s", importExpr)
	} else {
		// stdlib -> prefix with GOROOT
		pathPrefix = strings.Join([]string{GOROOT, "src"}, string(os.PathSeparator))
	} // ToDo: support non go mod

	crossPlatformImportExpr := strings.Replace(importExpr, "/", string(os.PathSeparator), -1)
	importDirectory := strings.Join([]string{pathPrefix, strings.Trim(crossPlatformImportExpr, "\"")}, string(os.PathSeparator))

	return importDirectory, nil
}

func importedGlobalVariableDeclaration(localVariables map[string]ast.Expr, imports []*ast.ImportSpec) (map[string]ast.Expr, error) {
	for _, im := range imports {
		// get imported label
		var importAlias string
		if im.Name == nil {
			pathSegments := strings.Split(im.Path.Value, "/")
			importAlias = strings.Trim(pathSegments[len(pathSegments)-1], "\"")
		} else {
			importAlias = im.Name.String()
		}

		// find local path on disk for listed import
		importDirectory, err := localImportPath(im.Path.Value)
		if err != nil {
			// uncomment the below log line if you want to start using non k8s/non stl libs for resolving const/var in metric definitions
			// fmt.Fprint(os.Stderr, err.Error() + "\n")
			continue
		}

		files, err := os.ReadDir(importDirectory)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to read import path directory %s with error %s, skipping\n", importDirectory, err)
			continue
		}

		for _, file := range files {
			if file.IsDir() {
				// do not grab constants from subpackages
				continue
			}

			if strings.Contains(file.Name(), "_test") {
				// do not parse test files
				continue
			}

			if !strings.HasSuffix(file.Name(), ".go") {
				// not a go code file, do not attempt to parse
				continue
			}

			fileset := token.NewFileSet()
			tree, err := parser.ParseFile(fileset, strings.Join([]string{importDirectory, file.Name()}, string(os.PathSeparator)), nil, parser.AllErrors)
			if err != nil {
				return nil, fmt.Errorf("failed to parse path %s with error %w", im.Path.Value, err)
			}

			// pass parsed filepath into globalVariableDeclarations
			variables := globalVariableDeclarations(tree)

			// add returned map into supplied map and prepend import label to all keys
			for k, v := range variables {
				importK := strings.Join([]string{importAlias, k}, ".")
				if _, ok := localVariables[importK]; !ok {
					localVariables[importK] = v
				} else {
					// cross-platform file that gets included in the correct OS build via OS build tags
					// use whatever matches GOOS

					if strings.Contains(file.Name(), GOOS) {
						// assume at some point we will find the correct OS version of this file
						// if we are running on an OS that does not have an OS specific file for something then we will include a constant we shouldn't
						// TODO: should we include/exclude based on the build tags?
						localVariables[importK] = v
					}

				}
			}
		}

	}

	return localVariables, nil
}
