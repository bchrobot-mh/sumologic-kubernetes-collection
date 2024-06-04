package stepfuncs

import (
	"context"
	"fmt"
	"os"
	"path"
	"testing"

	k8s_internal "github.com/SumoLogic/sumologic-kubernetes-collection/tests/integration/internal/k8s"
	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	log "k8s.io/klog/v2"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/SumoLogic/sumologic-kubernetes-collection/tests/integration/internal/ctxopts"
	"github.com/SumoLogic/sumologic-kubernetes-collection/tests/integration/internal/strings"
)

const (
	// envNameHelmNoDependencyUpdate is the name of an environment variable that
	// controls whether to skip the 'helm dependency update' invocation.
	// If its set to anything else than an empty string then it's being skipped.
	envNameHelmNoDependencyUpdate = "HELM_NO_DEPENDENCY_UPDATE"
)

// HelmVersion returns a features.Func that will run helm version
func HelmVersionOpt() features.Func {
	return func(ctx context.Context, t *testing.T, envConf *envconf.Config) context.Context {
		helmOptions := HelmOptionsFromT(t, ctxopts.KubectlOptions(ctx, envConf), []string{})
		_, err := helm.RunHelmCommandAndGetOutputE(t, helmOptions, "version")
		require.NoError(t, err)

		return ctx
	}
}

// HelmDependencyUpdateOpt returns a features.Func that will run helm dependency update using
// the provided path as an argument.
//
// NOTE:
// This step will be skipped if the relevant environment variable (envNameHelmNoDependencyUpdate)
// will be set to a non empty value.
func HelmDependencyUpdateOpt(path string) features.Func {
	return func(ctx context.Context, t *testing.T, envConf *envconf.Config) context.Context {
		if os.Getenv(envNameHelmNoDependencyUpdate) != "" {
			t.Logf(
				"Skipping helm dependency update because %s env is set", envNameHelmNoDependencyUpdate,
			)
			return ctx
		}
		helmOptions := HelmOptionsFromT(t, ctxopts.KubectlOptions(ctx, envConf), []string{})
		_, err := helm.RunHelmCommandAndGetOutputE(t, helmOptions, "dependency", "update", path)
		require.NoError(t, err)

		return ctx
	}
}

// HelmInstallOpt returns a features.Func that with run helm install using the provided path
// and releaseName as arguments.
//
// NOTE:
// By default the default cluster namespace will be used. If you'd like to specify the namespace
// use SetKubectlNamespaceOpt.
func HelmInstallOpt(path string, releaseName string) features.Func {
	return func(ctx context.Context, t *testing.T, envConf *envconf.Config) context.Context {
		helmOptions := HelmOptionsFromT(t, ctxopts.KubectlOptions(ctx, envConf), []string{"--wait"})

		err := helm.InstallE(t, helmOptions, path, releaseName)
		if err != nil {
			kubectlOptions := ctxopts.KubectlOptions(ctx, envConf)

			// Print setup job logs if installation failed.
			k8s.RunKubectl(t, kubectlOptions,
				"logs", fmt.Sprintf("-ljob-name=%s-sumologic-setup", releaseName),
			)

			// Print the status of all Pods
			kubectlOptions.Namespace = ""
			k8s.RunKubectl(t, kubectlOptions,
				"get", "pods", "--all-namespaces",
			)

			// Get the non-running Pods, we need to do this separately because describe doesn't allow filtering by field
			// There's also no filtering over readiness, so we need to iterate over the list ourselves
			pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{})
			notReadyPods := []corev1.Pod{}
			for _, pod := range pods {
				if !k8s_internal.IsPodReady(&pod) {
					notReadyPods = append(notReadyPods, pod)
				}
			}

			// Describe and print logs for Pods which aren't running
			for _, pod := range notReadyPods {
				kubectlOptions.Namespace = pod.Namespace
				log.InfoS("Describing Pod", "pod name", pod.Name)
				k8s.RunKubectl(t, kubectlOptions, "describe", "pod", pod.Name)
				log.InfoS("Printing logs for Pod", "pod name", pod.Name)
				k8s.RunKubectl(t, kubectlOptions, "logs", pod.Name)
			}
			require.NoError(t, err)
		}

		return ctx
	}
}

// HelmInstallTestOpt wraps HelmInstallOpt with helm release name generation for tests.
func HelmInstallTestOpt(path string) features.Func {
	return func(ctx context.Context, t *testing.T, envConf *envconf.Config) context.Context {
		releaseName := strings.ReleaseNameFromT(t)
		// create the namespace here so it can be propagated in the context
		// only the final before test action gets to modify the context
		ctx = KubectlCreateNamespaceTestOpt()(ctx, t, envConf)
		return HelmInstallOpt(path, releaseName)(ctx, t, envConf)
	}
}

// HelmDeleteOpt returns a features.Func that with run helm delete using the provided release name
// as argument.
//
// NOTE:
// By default the default cluster namespace will be used. If you'd like to specify the namespace
// use SetKubectlNamespaceOpt.
func HelmDeleteOpt(release string) features.Func {
	return func(ctx context.Context, t *testing.T, envConf *envconf.Config) context.Context {
		helmOptions := HelmOptionsFromT(t, ctxopts.KubectlOptions(ctx, envConf), []string{"--wait"})
		helm.Delete(t, helmOptions, release, true)
		return ctx
	}
}

// HelmDeleteTestOpt wraps HelmDeleteOpt by taking the release name saved in context
// by HelmInstallTestOpt/HelmInstallOpt.
func HelmDeleteTestOpt() features.Func {
	return func(ctx context.Context, t *testing.T, envConf *envconf.Config) context.Context {
		releaseName := strings.ReleaseNameFromT(t)
		return HelmDeleteOpt(releaseName)(ctx, t, envConf)
	}
}

// HelmOptionsFromT returns helm options for the test. It sets the values file from the
// `values` directory and concatenates that with a name name generated from a test name.
//
// The details of values file name generation can be found in `strings.ValueFileFromT()`.
func HelmOptionsFromT(t *testing.T, kubectlOptions *k8s.KubectlOptions, extraInstallationArgs []string) *helm.Options {
	valuesFilePath := path.Join("values", strings.ValueFileFromT(t))
	extraArgs := map[string][]string{
		"install": extraInstallationArgs,
	}

	// yamlFilePathCommon contains a shared set of values that:
	// * decrease the requested resources so that pods fit on runners available on Github CI.
	// * set dummy access keys, access IDs and sumologic-mock's URL as endpoint in the chart.
	const yamlFilePathCommon = "values/values_common.yaml"

	return &helm.Options{
		KubectlOptions: kubectlOptions,
		ValuesFiles:    []string{yamlFilePathCommon, valuesFilePath},
		ExtraArgs:      extraArgs,
	}
}
