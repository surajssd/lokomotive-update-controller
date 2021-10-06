package updater

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	sourceapi "github.com/fluxcd/source-controller/api/v1beta1"

	"github.com/kinvolk/nebraska/updater"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/yaml"

	"github.com/kinvolk/fluxlib/lib"
	helmrelease "github.com/kinvolk/fluxlib/lib/helm-release"
	sourcecontroller "github.com/kinvolk/fluxlib/lib/source-controller"
)

const (
	namespace      = "flux-system"
	defaultVersion = "0.0.1"
)

type Config struct {
	Kubeconfig     string
	ApplicationID  string
	Interval       int64
	Dev            bool
	NebraskaServer string
	Channel        string
	Docker         bool
	EnvPath        string

	grc            *sourcecontroller.GitRepoConfig
	hrc            *helmrelease.HelmReleaseConfig
	nbsClient      updater.Updater
	clusterID      string
	currentVersion string
	updateConfig   *UpdateConfig
}

type UpdateConfig struct {
	Packages []Package `json:"packages"`
}

type Package struct {
	Name  string `json:"name"`
	Chart string `json:"chart"`

	// TODO: Only one should be provided.
	GitRepo  *sourceapi.GitRepositorySpec  `json:"gitrepo,omitempty"`
	HelmRepo *sourceapi.HelmRepositorySpec `json:"helmrepo,omitempty"`
}

var fluxInstallInterval = metav1.Duration{Duration: 5 * time.Minute} //nolint:gomnd

func generateGitRepository(pkg *Package) *sourceapi.GitRepository {
	return &sourceapi.GitRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pkg.Name,
			Namespace: namespace,
		},
		Spec: *pkg.GitRepo,
	}
}

// func generateHelmRepository(pkg *Package) *sourceapi.HelmRepository {
// 	return &sourceapi.HelmRepository{
// 		ObjectMeta: metav1.ObjectMeta{
// 			Name:      pkg.Name,
// 			Namespace: namespace,
// 		},
// 		Spec: *pkg.HelmRepo,
// 	}
// }

func Reconcile(cfg *Config) error {
	kubeconfig, err := ioutil.ReadFile(cfg.Kubeconfig)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading kubeconfig: %w", err)
	}

	cfg.grc, err = sourcecontroller.NewGitRepoConfig(
		sourcecontroller.WithKubeconfig(kubeconfig),
	)
	if err != nil {
		return fmt.Errorf("initializing GitRepository client: %w", err)
	}

	cfg.hrc, err = helmrelease.NewHelmReleaseConfig(
		helmrelease.WithKubeconfig(kubeconfig),
	)
	if err != nil {
		return fmt.Errorf("initializing HelmRelease config: %w", err)
	}

	// TODO: Initialize the HelmRepository config.

	if err = cfg.getClusterID(kubeconfig); err != nil {
		return fmt.Errorf("retrieving cluster id: %w", err)
	}

	cfg.currentVersion = defaultVersion

	if err := cfg.setupNebraskaClient(); err != nil {
		return fmt.Errorf("setting up nebraska client: %w", err)
	}

	log.Debug("initialization complete")

	_ = wait.PollInfinite(time.Duration(cfg.Interval)*time.Minute, func() (done bool, err error) {
		log.Debug("reconciling infinitely!")

		if err := cfg.reconcile(); err != nil {
			log.Error(err)
		}

		return false, nil
	})

	return nil
}

// func addVToVersion(version string) string {
// 	if !strings.HasPrefix(version, "v") {
// 		version = "v" + version
// 	}

// 	return version
// }

func removeVFromVersion(version string) string {
	return strings.TrimPrefix(version, "v")
}

func (cfg *Config) getClusterID(kubeconfig []byte) error {
	// Return random UUID when using dev mode.
	if cfg.Dev {
		cfg.clusterID = string(uuid.NewUUID())
		return nil
	}

	c, err := lib.GetKubernetesClient(kubeconfig, nil)
	if err != nil {
		return fmt.Errorf("creating kubernetes client: %w", err)
	}

	var got corev1.Namespace
	if err := c.Get(context.TODO(), types.NamespacedName{Name: "kube-system"}, &got); err != nil {
		return fmt.Errorf("getting kube-system namespace: %w", err)
	}

	cfg.clusterID = string(got.UID)
	log.Debugf("got cluster id: %s", cfg.clusterID)

	return nil
}

func downloadFileContents(u string) ([]byte, error) {
	resp, err := http.Get(u)
	if err != nil {
		return nil, fmt.Errorf("getting file from %s: %w", u, err)
	}

	defer resp.Body.Close()

	file, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading the response: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http request returned non-200 response: %q. Message: %s", resp.Status, string(file))
	}

	return file, nil
}

func parseUpdateConfig(config []byte) (*UpdateConfig, error) {
	var ret UpdateConfig

	if err := yaml.Unmarshal(config, &ret); err != nil {
		return nil, fmt.Errorf("unmarshalling response into UpdateConfig: %w.\nGiven config:\n%s", err, string(config))
	}

	// TODO: Add validation code here.

	return &ret, nil
}

func (cfg *Config) getUpdateConfig(cfgLink string) error {
	updateCfgFile, err := downloadFileContents(cfgLink)
	if err != nil {
		return fmt.Errorf("downloading file from %s: %w", cfgLink, err)
	}

	cfg.updateConfig, err = parseUpdateConfig(updateCfgFile)
	if err != nil {
		return fmt.Errorf("parsing update config: %w", err)
	}

	return nil
}

func (cfg *Config) updateFluxCRs() error {
	for _, pkg := range cfg.updateConfig.Packages {
		gr := generateGitRepository(&pkg)
		if err := cfg.grc.CreateOrUpdate(gr); err != nil {
			return fmt.Errorf("creating/updating GitRepository %s: %w", pkg.Name, err)
		}

		log.Debugf("Created/Updated the GitRepository: %s", pkg.Name)

		hrCluster, err := cfg.hrc.Get(pkg.Name, namespace)
		if err != nil {
			return fmt.Errorf("getting HelmRelease: %s", pkg.Name)
		}

		hr := hrCluster.DeepCopy()
		hr.Spec.Chart.Spec.Chart = pkg.Chart
		hr.Spec.Chart.Spec.SourceRef.Name = pkg.Name

		// TODO: Also add support of the HelmRepository.
		// hr.Spec.Chart.Spec.SourceRef.Kind = "HelmRepository"

		if err := cfg.hrc.CreateOrUpdate(hr); err != nil {
			return fmt.Errorf("updating HelmRelease %s: %w", hr.Name, err)
		}

		log.Debugf("Updated the HelmRelease: %s", pkg.Name)
	}

	log.Info("Updated all the HelmReleases.")

	return nil
}

func (cfg *Config) waitForHelmReleaseReadiness() error {
	log.Debug("checking the HelmRelease readiness.")

	// Poll for ten minutes every ten seconds.
	if err := wait.PollImmediate(time.Second*10, time.Minute*10, func() (done bool, err error) {
		ready := true

		for _, pkg := range cfg.updateConfig.Packages {
			hr, err := cfg.hrc.Get(pkg.Name, namespace)
			if err != nil {
				return false, fmt.Errorf("getting the HelmRelease %s: %w", hr.Name, err)
			}

			// Not ready yet.
			if hr.Generation != hr.Status.ObservedGeneration || !apimeta.IsStatusConditionTrue(hr.Status.Conditions, meta.ReadyCondition) {
				ready = false
			}
		}

		// No need to poll any more, all the HelmReleases are ready.
		if ready {
			return true, nil
		}

		return false, nil
	}); err != nil {
		return fmt.Errorf("waiting for the HelmReleases to be ready: %w", err)
	}

	log.Info("All the HelmReleases are ready with the new version.")

	return nil
}

func (cfg *Config) setupNebraskaClient() error {
	var err error

	nbsConfig := updater.Config{
		OmahaURL:        cfg.NebraskaServer,
		AppID:           cfg.ApplicationID,
		Channel:         cfg.Channel,
		InstanceID:      cfg.clusterID,
		InstanceVersion: removeVFromVersion(cfg.currentVersion),
	}

	cfg.nbsClient, err = updater.New(nbsConfig)
	if err != nil {
		return fmt.Errorf("initializing nebraska client: %w", err)
	}

	return nil
}

func (cfg *Config) reconcile() error {
	ctx := context.TODO()

	// Let us check if there is an update.
	info, err := cfg.nbsClient.CheckForUpdates(ctx)
	if err != nil {
		return fmt.Errorf("checking for updates: %w", err)
	}

	// There is no update hence return.
	if !info.HasUpdate {
		log.Info("no update available")

		// Print the response just in case.
		log.Debugf("got this response: %#v", info.OmahaResponse().Apps[0])

		return nil
	}

	_ = cfg.nbsClient.ReportProgress(ctx, updater.ProgressDownloadStarted)

	// There is a new update.
	version := info.Version
	link := info.URL()

	log.Debugf("update available: %s", version)

	if err := cfg.getUpdateConfig(link); err != nil {
		_ = cfg.nbsClient.ReportProgress(ctx, updater.ProgressError)

		return fmt.Errorf("getting the update config provided in Nebraska update: %w", err)
	}

	if err := cfg.updateFluxCRs(); err != nil {
		_ = cfg.nbsClient.ReportProgress(ctx, updater.ProgressError)

		return fmt.Errorf("updating flux CRs: %w", err)
	}

	_ = cfg.nbsClient.ReportProgress(ctx, updater.ProgressDownloadFinished)
	_ = cfg.nbsClient.ReportProgress(ctx, updater.ProgressInstallationStarted)

	if err := cfg.waitForHelmReleaseReadiness(); err != nil {
		_ = cfg.nbsClient.ReportProgress(ctx, updater.ProgressError)

		return err
	}

	// Update the current version to the new one.
	cfg.currentVersion = version

	_ = cfg.nbsClient.ReportProgress(ctx, updater.ProgressInstallationFinished)
	_ = cfg.nbsClient.ReportProgress(ctx, updater.ProgressUpdateComplete)

	cfg.nbsClient.SetInstanceVersion(info.Version)

	return nil
}
