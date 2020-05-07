/*
Copyright 2019 The Skaffold Authors

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

package deploy

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/segmentio/textio"
	yaml "gopkg.in/yaml.v2"

	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/build"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/color"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/constants"
	deploy "github.com/GoogleContainerTools/skaffold/pkg/skaffold/deploy/kubectl"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/event"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/kubectl"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/runner/runcontext"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/schema/latest"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/util"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/warnings"
)

type patchPath struct {
	Path  string `yaml:"path"`
	Patch string `yaml:"patch"`
}

type patchWrapper struct {
	*patchPath
}

// kustomization is the content of a kustomization.yaml file.
type kustomization struct {
	Bases                 []string             `yaml:"bases"`
	Resources             []string             `yaml:"resources"`
	Patches               []patchWrapper       `yaml:"patches"`
	PatchesStrategicMerge []string             `yaml:"patchesStrategicMerge"`
	CRDs                  []string             `yaml:"crds"`
	PatchesJSON6902       []patchJSON6902      `yaml:"patchesJson6902"`
	ConfigMapGenerator    []configMapGenerator `yaml:"configMapGenerator"`
	SecretGenerator       []secretGenerator    `yaml:"secretGenerator"`
}

type patchJSON6902 struct {
	Path string `yaml:"path"`
}

type configMapGenerator struct {
	Files []string `yaml:"files"`
	Env   string   `yaml:"env"`
	Envs  []string `yaml:"envs"`
}

type secretGenerator struct {
	Files []string `yaml:"files"`
	Env   string   `yaml:"env"`
	Envs  []string `yaml:"envs"`
}

// KustomizeDeployer deploys workflows using kustomize CLI.
type KustomizeDeployer struct {
	*latest.KustomizeDeploy

	kubectl            deploy.CLI
	insecureRegistries map[string]bool
	BuildArgs          []string
}

func NewKustomizeDeployer(runCtx *runcontext.RunContext) *KustomizeDeployer {
	return &KustomizeDeployer{
		KustomizeDeploy: runCtx.Cfg.Deploy.KustomizeDeploy,
		kubectl: deploy.CLI{
			CLI:         kubectl.NewFromRunContext(runCtx),
			Flags:       runCtx.Cfg.Deploy.KustomizeDeploy.Flags,
			ForceDeploy: runCtx.Opts.Force,
		},
		insecureRegistries: runCtx.InsecureRegistries,
		BuildArgs:          runCtx.Cfg.Deploy.KustomizeDeploy.BuildArgs,
	}
}

// Labels returns the labels specific to kustomize.
func (k *KustomizeDeployer) Labels() map[string]string {
	return map[string]string{
		constants.Labels.Deployer: "kustomize",
	}
}

// Deploy runs `kubectl apply` on the manifest generated by kustomize.
func (k *KustomizeDeployer) Deploy(ctx context.Context, out io.Writer, builds []build.Artifact, labellers []Labeller) *Result {
	event.DeployInProgress()

	manifests, err := k.renderManifests(ctx, out, builds, labellers)
	if err != nil {
		event.DeployFailed(err)
		return NewDeployErrorResult(err)
	}

	if len(manifests) == 0 {
		event.DeployComplete()
		return NewDeploySuccessResult(nil)
	}

	namespaces, err := manifests.CollectNamespaces()
	if err != nil {
		event.DeployInfoEvent(fmt.Errorf("could not fetch deployed resource namespace. "+
			"This might cause port-forward and deploy health-check to fail: %w", err))
	}

	if err := k.kubectl.Apply(ctx, textio.NewPrefixWriter(out, " - "), manifests); err != nil {
		event.DeployFailed(err)
		return NewDeployErrorResult(fmt.Errorf("kubectl error: %w", err))
	}

	event.DeployComplete()
	return NewDeploySuccessResult(namespaces)
}

func (k *KustomizeDeployer) renderManifests(ctx context.Context, out io.Writer, builds []build.Artifact, labellers []Labeller) (deploy.ManifestList, error) {
	if err := k.kubectl.CheckVersion(ctx); err != nil {
		color.Default.Fprintln(out, "kubectl client version:", k.kubectl.Version(ctx))
		color.Default.Fprintln(out, err)
	}

	manifests, err := k.readManifests(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading manifests: %w", err)
	}

	if len(manifests) == 0 {
		return nil, nil
	}

	manifests, err = manifests.ReplaceImages(builds)
	if err != nil {
		return nil, fmt.Errorf("replacing images in manifests: %w", err)
	}

	for _, transform := range manifestTransforms {
		manifests, err = transform(manifests, builds, k.insecureRegistries)
		if err != nil {
			return nil, fmt.Errorf("unable to transform manifests: %w", err)
		}
	}

	manifests, err = manifests.SetLabels(merge(k, labellers...))
	if err != nil {
		return nil, fmt.Errorf("setting labels in manifests: %w", err)
	}

	return manifests, nil
}

// Cleanup deletes what was deployed by calling Deploy.
func (k *KustomizeDeployer) Cleanup(ctx context.Context, out io.Writer) error {
	manifests, err := k.readManifests(ctx)
	if err != nil {
		return fmt.Errorf("reading manifests: %w", err)
	}

	if err := k.kubectl.Delete(ctx, textio.NewPrefixWriter(out, " - "), manifests); err != nil {
		return fmt.Errorf("delete: %w", err)
	}

	return nil
}

// Dependencies lists all the files that describe what needs to be deployed.
func (k *KustomizeDeployer) Dependencies() ([]string, error) {
	deps := newStringSet()
	for _, kustomizePath := range k.KustomizePaths {
		depsForKustomization, err := dependenciesForKustomization(kustomizePath)
		if err != nil {
			return nil, err
		}
		deps.insert(depsForKustomization...)
	}
	return deps.toList(), nil
}

func (k *KustomizeDeployer) Render(ctx context.Context, out io.Writer, builds []build.Artifact, labellers []Labeller, filepath string) error {
	manifests, err := k.renderManifests(ctx, out, builds, labellers)
	if err != nil {
		return err
	}

	manifestOut := out
	if filepath != "" {
		f, err := os.OpenFile(filepath, os.O_RDWR|os.O_CREATE, 0666)
		if err != nil {
			return fmt.Errorf("opening file for writing manifests: %w", err)
		}
		defer f.Close()
		f.WriteString(manifests.String() + "\n")
		return nil
	}

	fmt.Fprintln(manifestOut, manifests.String())
	return nil
}

// UnmarshalYAML implements JSON unmarshalling by reading an inline yaml fragment.
func (p *patchWrapper) UnmarshalYAML(unmarshal func(interface{}) error) (err error) {
	pp := &patchPath{}
	if err := unmarshal(&pp); err != nil {
		var oldPathString string
		if err := unmarshal(&oldPathString); err != nil {
			return err
		}
		warnings.Printf("list of file paths deprecated: see https://github.com/kubernetes-sigs/kustomize/blob/master/docs/plugins/builtins.md#patchtransformer")
		pp.Path = oldPathString
	}
	p.patchPath = pp
	return nil
}

func dependenciesForKustomization(dir string) ([]string, error) {
	var deps []string

	path, err := findKustomizationConfig(dir)
	if err != nil {
		// No kustomization config found so assume it's remote and stop traversing
		return deps, nil
	}

	buf, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}

	content := kustomization{}
	if err := yaml.Unmarshal(buf, &content); err != nil {
		return nil, err
	}

	deps = append(deps, path)

	candidates := append(content.Bases, content.Resources...)

	for _, candidate := range candidates {
		// If the file  doesn't exist locally, we can assume it's a remote file and
		// skip it, since we can't monitor remote files. Kustomize itself will
		// handle invalid/missing files.
		local, mode := pathExistsLocally(candidate, dir)
		if !local {
			continue
		}

		if mode.IsDir() {
			candidateDeps, err := dependenciesForKustomization(filepath.Join(dir, candidate))
			if err != nil {
				return nil, err
			}
			deps = append(deps, candidateDeps...)
		} else {
			deps = append(deps, filepath.Join(dir, candidate))
		}
	}

	deps = append(deps, util.AbsolutePaths(dir, content.PatchesStrategicMerge)...)
	deps = append(deps, util.AbsolutePaths(dir, content.CRDs)...)
	for _, patch := range content.Patches {
		if patch.Path != "" {
			deps = append(deps, filepath.Join(dir, patch.Path))
		}
	}
	for _, jsonPatch := range content.PatchesJSON6902 {
		deps = append(deps, filepath.Join(dir, jsonPatch.Path))
	}
	for _, generator := range content.ConfigMapGenerator {
		deps = append(deps, util.AbsolutePaths(dir, generator.Files)...)
		envs := generator.Envs
		if generator.Env != "" {
			envs = append(envs, generator.Env)
		}
		deps = append(deps, util.AbsolutePaths(dir, envs)...)
	}
	for _, generator := range content.SecretGenerator {
		deps = append(deps, util.AbsolutePaths(dir, generator.Files)...)
		envs := generator.Envs
		if generator.Env != "" {
			envs = append(envs, generator.Env)
		}
		deps = append(deps, util.AbsolutePaths(dir, envs)...)
	}

	return deps, nil
}

// A Kustomization config must be at the root of the directory. Kustomize will
// error if more than one of these files exists so order doesn't matter.
func findKustomizationConfig(dir string) (string, error) {
	candidates := []string{"kustomization.yaml", "kustomization.yml", "Kustomization"}
	for _, candidate := range candidates {
		if local, _ := pathExistsLocally(candidate, dir); local {
			return filepath.Join(dir, candidate), nil
		}
	}
	return "", fmt.Errorf("no Kustomization configuration found in directory: %s", dir)
}

func pathExistsLocally(filename string, workingDir string) (bool, os.FileMode) {
	path := filename
	if !filepath.IsAbs(filename) {
		path = filepath.Join(workingDir, filename)
	}
	if f, err := os.Stat(path); err == nil {
		return true, f.Mode()
	}
	return false, 0
}

func (k *KustomizeDeployer) readManifests(ctx context.Context) (deploy.ManifestList, error) {
	var manifests deploy.ManifestList
	for _, kustomizePath := range k.KustomizePaths {
		cmd := exec.CommandContext(ctx, "kustomize", buildCommandArgs(k.BuildArgs, kustomizePath)...)
		out, err := util.RunCmdOut(cmd)
		if err != nil {
			return nil, fmt.Errorf("kustomize build: %w", err)
		}

		if len(out) == 0 {
			continue
		}
		manifests.Append(out)
	}
	return manifests, nil
}

func buildCommandArgs(buildArgs []string, kustomizePath string) []string {
	var args []string
	args = append(args, "build")

	if len(buildArgs) > 0 {
		for _, v := range buildArgs {
			parts := strings.Split(v, " ")
			args = append(args, parts...)
		}
	}

	if len(kustomizePath) > 0 {
		args = append(args, kustomizePath)
	}

	return args
}
