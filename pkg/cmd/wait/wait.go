package wait

import (
	"context"
	"strings"
	"time"

	"github.com/jenkins-x/go-scm/scm"
	jxc "github.com/jenkins-x/jx-api/v3/pkg/client/clientset/versioned"
	"github.com/jenkins-x/jx-helpers/v3/pkg/kube"
	"github.com/jenkins-x/jx-helpers/v3/pkg/kube/jxclient"
	"github.com/jenkins-x/jx-helpers/v3/pkg/kube/naming"
	"github.com/jenkins-x/jx-helpers/v3/pkg/options"
	"github.com/jenkins-x/jx-helpers/v3/pkg/termcolor"
	"github.com/jenkins-x/jx-pipeline/pkg/constants"
	"github.com/jenkins-x/jx-pipeline/pkg/triggers"
	"github.com/jenkins-x/lighthouse/pkg/config"
	"github.com/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/jenkins-x/jx-helpers/v3/pkg/cobras/helper"
	"github.com/spf13/cobra"

	"github.com/jenkins-x/jx-helpers/v3/pkg/cobras/templates"
	"github.com/jenkins-x/jx-logging/v3/pkg/log"
)

// Options contains the command line options
type Options struct {
	WaitDuration        time.Duration
	PollPeriod          time.Duration
	Owner               string
	Repository          string
	LighthouseConfigMap string
	Namespace           string
	KubeClient          kubernetes.Interface
	JXClient            jxc.Interface
}

var (
	info = termcolor.ColorInfo

	cmdLong = templates.LongDesc(`
		Waits for a pipeline to be imported and activated by the boot Job

`)

	cmdExample = templates.Examples(`
		# Waits for the pipeline to be setup for the given repository
		jx pipeline wait --owner myorg --repo myrepo
	`)
)

// NewCmdPipelineWait creates the command
func NewCmdPipelineWait() (*cobra.Command, *Options) {
	o := &Options{}

	cmd := &cobra.Command{
		Use:     "wait",
		Short:   "Waits for a pipeline to be imported and activated by the boot Job",
		Long:    cmdLong,
		Example: cmdExample,
		Aliases: []string{"build", "run"},
		Run: func(cmd *cobra.Command, args []string) {
			err := o.Run()
			helper.CheckErr(err)
		},
	}
	cmd.Flags().StringVarP(&o.Owner, "owner", "o", "", "The owner name to wait for")
	cmd.Flags().StringVarP(&o.Repository, "repo", "r", "", "The repository name o wait for")
	cmd.Flags().StringVarP(&o.LighthouseConfigMap, "configmap", "", constants.LighthouseConfigMapName, "The name of the Lighthouse ConfigMap to find the trigger configurations")
	cmd.Flags().StringVarP(&o.Namespace, "namespace", "n", "", "The namespace to look for the lighthouse configuration. Defaults to the current namespace")

	cmd.Flags().DurationVarP(&o.WaitDuration, "duration", "", time.Minute*20, "Maximum duration to wait for one or more matching triggers to be setup in Lighthouse. Useful for when a new repository is being imported via GitOps")
	cmd.Flags().DurationVarP(&o.PollPeriod, "poll-period", "", time.Second*2, "Poll period when waiting for one or more matching triggers to be setup in Lighthouse. Useful for when a new repository is being imported via GitOps")

	return cmd, o
}

// Validate verifies things are setup correctly
func (o *Options) Validate() error {
	var err error
	o.KubeClient, o.Namespace, err = kube.LazyCreateKubeClientAndNamespace(o.KubeClient, o.Namespace)
	if err != nil {
		return errors.Wrapf(err, "failed to create kube client")
	}
	o.JXClient, err = jxclient.LazyCreateJXClient(o.JXClient)
	if err != nil {
		return errors.Wrapf(err, "failed to create jx client")
	}

	if o.Owner == "" {
		return options.MissingOption("owner")
	}
	if o.Repository == "" {
		return options.MissingOption("repo")
	}
	return nil
}

// Run implements this command
func (o *Options) Run() error {
	err := o.Validate()
	if err != nil {
		return errors.Wrapf(err, "failed to validate options")
	}

	fullName := scm.Join(o.Owner, o.Repository)

	exists, err := o.waitForRepositoryToBeSetup(o.KubeClient, o.Namespace, fullName)
	if err != nil {
		return errors.Wrapf(err, "failed to wait for repository to be setup in lighthouse")
	}
	if !exists {
		return errors.Errorf("repository %s is not yet setup in lighthouse", fullName)
	}

	err = o.waitForWebHookToBeSetup(o.JXClient, o.Namespace, o.Owner, o.Repository)
	if err != nil {
		return errors.Wrapf(err, "failed to wait for repository to have its webhook enabled")
	}

	log.Logger().Infof("the repository %s is now setup in lighthouse and has its webhook enabled", info(fullName))
	return nil
}

func (o *Options) waitForRepositoryToBeSetup(kubeClient kubernetes.Interface, ns, fullName string) (bool, error) {
	end := time.Now().Add(o.WaitDuration)
	name := o.LighthouseConfigMap
	logWaiting := false

	for {
		cfg, err := triggers.LoadLighthouseConfig(kubeClient, ns, name, true)
		if err != nil {
			return false, errors.Wrapf(err, "failed to load lighthouse config")
		}
		flag := o.containsRepositoryTrigger(cfg, fullName)
		if flag {
			return flag, nil
		}

		if time.Now().After(end) {
			return false, errors.Errorf("failed to find trigger in the lighthouse configuration in ConfigMap %s in namespace %s for repository: %s within %s", name, ns, fullName, o.WaitDuration.String())
		}

		if !logWaiting {
			logWaiting = true
			log.Logger().Infof("waiting up to %s for a trigger to be added to the lighthouse configuration in ConfigMap %s in namespace %s for repository: %s", info(o.WaitDuration.String()), info(name), info(ns), info(fullName))
		}
		time.Sleep(o.PollPeriod)
	}
}

func (o *Options) waitForWebHookToBeSetup(jxClient jxc.Interface, ns, owner, repository string) error {
	end := time.Now().Add(o.WaitDuration)
	name := naming.ToValidName(o.Owner + "-" + o.Repository)
	logWaiting := false

	fullName := scm.Join(owner, repository)
	lastValue := ""
	found := false
	lastFailMessage := ""
	ctx := context.Background()

	for {
		sr, err := jxClient.JenkinsV1().SourceRepositories(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if !apierrors.IsNotFound(err) {
				return errors.Wrapf(err, "failed to find SourceRepository %s in namespace %s", name, ns)
			}
		} else {
			if !found {
				found = true
				log.Logger().Infof("found SourceRepository %s for %s", info(sr.Name), info(sr.Spec.URL))
			}

			if sr.Annotations == nil {
				sr.Annotations = map[string]string{}
			}
			value := sr.Annotations["webhook.jenkins-x.io"]
			if value != "" {
				if value != lastValue {
					lastValue = value
					log.Logger().Infof("webhook status annotation is: %s", info(value))

					if value == "true" {
						return nil
					}

					if strings.HasPrefix(strings.ToLower(value), "err") {
						failure := sr.Annotations["webhook.jenkins-x.io/error"]
						if failure != "" && failure != lastFailMessage {
							lastFailMessage = failure
							log.Logger().Warnf("when creating webhook: %s", lastFailMessage)
						}
					}
				}
			}
		}

		if time.Now().After(end) {
			return errors.Errorf("failed to find trigger in the lighthouse configuration in ConfigMap %s in namespace %s for repository: %s within %s", name, ns, fullName, o.WaitDuration.String())
		}

		if !logWaiting {
			logWaiting = true
			log.Logger().Infof("waiting up to %s the webhook to be registered for the SourceRepository %s in namespace %s for repository: %s", info(o.WaitDuration.String()), info(name), info(ns), info(fullName))
		}
		time.Sleep(o.PollPeriod)
	}
}

// containsRepositoryTrigger returns true if the trigger is setup for the repository
func (o *Options) containsRepositoryTrigger(cfg *config.Config, fullName string) bool {
	if cfg.Postsubmits[fullName] != nil {
		return true
	}
	if cfg.InRepoConfig.Enabled != nil {
		f := cfg.InRepoConfig.Enabled[fullName]
		if f != nil {
			return *f
		}
	}
	return false
}
