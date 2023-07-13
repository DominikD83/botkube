package migrate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/AlecAivazis/survey/v2"
	"github.com/hasura/go-graphql-client"
	"github.com/muesli/reflow/indent"
	"github.com/pkg/errors"
	"golang.org/x/oauth2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	cliconfig "github.com/kubeshop/botkube/cmd/cli/cmd/config"
	"github.com/kubeshop/botkube/internal/cli"
	"github.com/kubeshop/botkube/internal/cli/printer"
	"github.com/kubeshop/botkube/internal/ptr"
	gqlModel "github.com/kubeshop/botkube/internal/remote/graphql"
	bkconfig "github.com/kubeshop/botkube/pkg/config"
	"github.com/kubeshop/botkube/pkg/multierror"
)

const (
	migrationJobName = "botkube-migration"
	configMapName    = "botkube-config-exporter"

	instanceDetailsURLFmt = "%s/instances/%s"
)

// Run runs the migration process.
func Run(ctx context.Context, status *printer.StatusPrinter, config []byte, opts Options) (string, error) {
	authToken := opts.Token
	if authToken == "" {
		cfg, err := cliconfig.New()
		if err != nil {
			return "", err
		}
		authToken = cfg.Token
	}

	status.Step("Parsing Botkube configuration")
	botkubeClusterConfig, _, err := bkconfig.LoadWithDefaults([][]byte{config})
	if err != nil {
		return "", err
	}

	return migrate(ctx, status, opts, botkubeClusterConfig, authToken)
}

func migrate(ctx context.Context, status *printer.StatusPrinter, opts Options, botkubeClusterConfig *bkconfig.Config, token string) (string, error) {
	converter := NewConverter()
	src := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)
	httpClient := oauth2.NewClient(context.Background(), src)
	client := graphql.NewClient(opts.CloudAPIURL, httpClient)

	plugins, err := converter.ConvertPlugins(botkubeClusterConfig.Executors, botkubeClusterConfig.Sources)
	if err != nil {
		return "", errors.Wrap(err, "while converting plugins")
	}
	status.Step("Converted %d plugins", len(plugins))

	actions := converter.ConvertActions(botkubeClusterConfig.Actions)
	status.Step("Converted %d action", len(actions))

	platforms := converter.ConvertPlatforms(botkubeClusterConfig.Communications)
	status.Step(`Converted platforms:
    - Slacks: %d
    - Discords: %d
    - Mattermosts: %d`,
		len(platforms.SocketSlacks), len(platforms.Discords), len(platforms.Mattermosts))
	status.End(true)

	instanceName, err := getInstanceName(opts)
	if err != nil {
		return "", errors.Wrap(err, "while parsing instance name")
	}
	status.Step("Creating %q Cloud Instance", instanceName)
	var mutation struct {
		CreateDeployment struct {
			ID          string  `json:"id"`
			HelmCommand *string `json:"helmCommand"`
		} `graphql:"createDeployment(input: $input)"`
	}
	err = client.Mutate(ctx, &mutation, map[string]interface{}{
		"input": gqlModel.DeploymentCreateInput{
			Name:      instanceName,
			Plugins:   plugins,
			Actions:   actions,
			Platforms: platforms,
		},
	})
	if err != nil {
		return "", errors.Wrap(err, "while creating deployment")
	}

	aliases := converter.ConvertAliases(botkubeClusterConfig.Aliases, mutation.CreateDeployment.ID)
	status.Step("Converted %d aliases", len(aliases))

	errs := multierror.New()
	for _, alias := range aliases {
		status.Step("Migrating Alias %q", alias.Name)
		var aliasMutation struct {
			CreateAlias struct {
				ID string `json:"id"`
			} `graphql:"createAlias(input: $input)"`
		}
		err = client.Mutate(ctx, &aliasMutation, map[string]interface{}{
			"input": *alias,
		})
		if err != nil {
			errs = multierror.Append(errs, fmt.Errorf("while creating Alias %q: %w", alias.Name, err))
			continue
		}
	}

	if errs.ErrorOrNil() != nil {
		return "", fmt.Errorf("while migrating aliases: %w%s", errs.ErrorOrNil(), errStateMessage(opts.CloudDashboardURL, mutation.CreateDeployment.ID))
	}

	if opts.SkipConnect {
		status.End(true)
		return mutation.CreateDeployment.ID, nil
	}

	helmCmd := ptr.ToValue(mutation.CreateDeployment.HelmCommand)
	cmds := []string{
		"helm repo add botkube https://charts.botkube.io",
		"helm repo update botkube",
		helmCmd,
	}
	bldr := strings.Builder{}
	for _, cmd := range cmds {
		msg := fmt.Sprintf("$ %s\n\n", cmd)
		bldr.WriteString(indent.String(msg, 4))
	}
	status.InfoWithBody("Connect Botkube instance", bldr.String())

	shouldUpgrade, err := shouldUpgradeInstallation(opts)
	if err != nil {
		return "", err
	}
	if !shouldUpgrade {
		status.Infof("Skipping command execution. Remember to run it manually to finish the migration process.")
		return mutation.CreateDeployment.ID, nil
	}

	status.Infof("Running helm upgrade")
	for _, cmd := range cmds {
		//nolint:gosec //subprocess launched with variable
		cmd := exec.Command("/bin/sh", "-c", cmd)
		cmd.Stderr = NewIndentWriter(os.Stderr, 4)
		cmd.Stdout = NewIndentWriter(os.Stdout, 4)

		if err = cmd.Run(); err != nil {
			return "", err
		}
		fmt.Println()
		fmt.Println()
	}

	status.End(true)

	return mutation.CreateDeployment.ID, nil
}

func getInstanceName(opts Options) (string, error) {
	if opts.InstanceName != "" {
		return opts.InstanceName, nil
	}

	qs := []*survey.Question{
		{
			Name: "instanceName",
			Prompt: &survey.Input{
				Message: "Please type Botkube Instance name: ",
				Default: "Botkube",
			},
			Validate: survey.ComposeValidators(survey.Required),
		},
	}

	if err := survey.Ask(qs, &opts); err != nil {
		return "", err
	}

	return opts.InstanceName, nil
}

func GetConfigFromCluster(ctx context.Context, opts Options) ([]byte, *corev1.Pod, error) {
	k8sCli, err := newK8sClient()
	if err != nil {
		return nil, nil, err
	}
	defer cleanup(ctx, k8sCli, opts)

	botkubePod, err := getBotkubePod(ctx, k8sCli, opts)
	if err != nil {
		return nil, nil, err
	}

	if err = createMigrationJob(ctx, k8sCli, botkubePod, opts.ConfigExporter); err != nil {
		return nil, nil, err
	}

	if err = waitForMigrationJob(ctx, k8sCli, opts); err != nil {
		return nil, nil, err
	}
	config, err := readConfigFromCM(ctx, k8sCli, opts)
	if err != nil {
		return nil, nil, err
	}
	return config, botkubePod, nil
}

func newK8sClient() (*kubernetes.Clientset, error) {
	k8sConfig, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(k8sConfig)
}

func getBotkubePod(ctx context.Context, k8sCli *kubernetes.Clientset, opts Options) (*corev1.Pod, error) {
	pods, err := k8sCli.CoreV1().Pods(opts.Namespace).List(ctx, metav1.ListOptions{LabelSelector: opts.Label})
	if err != nil {
		return nil, err
	}
	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("no botkube pod found")
	}
	return &pods.Items[0], nil
}

func createMigrationJob(ctx context.Context, k8sCli *kubernetes.Clientset, botkubePod *corev1.Pod, cfg ConfigExporterOptions) error {
	var container corev1.Container
	for _, c := range botkubePod.Spec.Containers {
		if c.Name == "botkube" {
			container = c
			break
		}
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      migrationJobName,
			Namespace: botkubePod.Namespace,
			Labels: map[string]string{
				"app":                  migrationJobName,
				"botkube.io/migration": "true",
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            migrationJobName,
							Image:           fmt.Sprintf("%s/%s:%s", cfg.Registry, cfg.Repository, cfg.Tag),
							ImagePullPolicy: corev1.PullIfNotPresent,
							Env:             container.Env,
							VolumeMounts:    container.VolumeMounts,
						},
					},
					Volumes:            botkubePod.Spec.Volumes,
					ServiceAccountName: botkubePod.Spec.ServiceAccountName,
					RestartPolicy:      corev1.RestartPolicyNever,
				},
			},
		},
	}

	_, err := k8sCli.BatchV1().Jobs(botkubePod.Namespace).Create(ctx, job, metav1.CreateOptions{})

	return err
}

func waitForMigrationJob(ctx context.Context, k8sCli *kubernetes.Clientset, opts Options) error {
	ctxWithTimeout, cancelFn := context.WithTimeout(ctx, opts.ConfigExporter.Timeout)
	defer cancelFn()

	ticker := time.NewTicker(opts.ConfigExporter.PollPeriod)
	defer ticker.Stop()

	var job *batchv1.Job
	for {
		select {
		case <-ctxWithTimeout.Done():

			errMsg := fmt.Sprintf("migration job failed: %s", context.Canceled.Error())

			if cli.VerboseMode.IsEnabled() && job != nil {
				errMsg = fmt.Sprintf("%s\n\nDEBUG:\njob:\n\n%s", errMsg, job.String())
			}

			// TODO: Add ability to keep the job if it fails and improve the error
			return errors.New(errMsg)
		case <-ticker.C:
			var err error
			job, err = k8sCli.BatchV1().Jobs(opts.Namespace).Get(ctx, migrationJobName, metav1.GetOptions{})
			if err != nil {
				fmt.Println("Error getting migration job: ", err.Error())
				continue
			}

			if job.Status.Succeeded > 0 {
				return nil
			}
		}
	}
}

func readConfigFromCM(ctx context.Context, k8sCli *kubernetes.Clientset, opts Options) ([]byte, error) {
	configMap, err := k8sCli.CoreV1().ConfigMaps(opts.Namespace).Get(ctx, configMapName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return configMap.BinaryData["config.yaml"], nil
}

func cleanup(ctx context.Context, k8sCli *kubernetes.Clientset, opts Options) {
	foreground := metav1.DeletePropagationForeground
	_ = k8sCli.BatchV1().Jobs(opts.Namespace).Delete(ctx, migrationJobName, metav1.DeleteOptions{PropagationPolicy: &foreground})
	_ = k8sCli.CoreV1().ConfigMaps(opts.Namespace).Delete(ctx, configMapName, metav1.DeleteOptions{})
}

func errStateMessage(dashboardURL, instanceID string) string {
	return fmt.Sprintf("\n\nMigration process failed. Navigate to %s to continue configuring newly created instance.\n"+
		"Alternatively, delete the instance from the link above and try again.", fmt.Sprintf(instanceDetailsURLFmt, dashboardURL, instanceID))
}

func shouldUpgradeInstallation(opts Options) (bool, error) {
	if opts.AutoUpgrade {
		return true, nil
	}

	run := false
	prompt := &survey.Confirm{
		Message: "Would you like to continue?",
		Default: true,
	}

	err := survey.AskOne(prompt, &run)
	if err != nil {
		return false, errors.Wrap(err, "while asking for confirmation")
	}

	return run, nil
}