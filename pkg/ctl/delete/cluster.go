package delete

import (
	"fmt"
	"os"
	"strings"

	"github.com/kubicorn/kubicorn/pkg/logger"
	"github.com/spf13/cobra"
	"github.com/weaveworks/eksctl/pkg/ctl"
	"github.com/weaveworks/eksctl/pkg/eks"
	"github.com/weaveworks/eksctl/pkg/eks/api"
	"github.com/weaveworks/eksctl/pkg/utils/kubeconfig"
)

func deleteClusterCmd() *cobra.Command {
	p := &api.ProviderConfig{}
	cfg := api.NewClusterConfig()

	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Delete a cluster",
		Run: func(_ *cobra.Command, args []string) {
			if err := doDeleteCluster(p, cfg, ctl.GetNameArg(args)); err != nil {
				logger.Critical("%s\n", err.Error())
				os.Exit(1)
			}
		},
	}

	fs := cmd.Flags()

	fs.StringVarP(&cfg.ID.Name, "name", "n", "", "EKS cluster name (required)")

	fs.StringVarP(&p.Region, "region", "r", "", "AWS region")
	fs.StringVarP(&p.Profile, "profile", "p", "", "AWS credentials profile to use (overrides the AWS_PROFILE environment variable)")

	fs.BoolVarP(&waitDelete, "wait", "w", false, "Wait for deletion of all resources before exiting")

	fs.DurationVar(&p.WaitTimeout, "timeout", api.DefaultWaitTimeout, "max wait time in any polling operations")

	return cmd
}

func doDeleteCluster(p *api.ProviderConfig, cfg *api.ClusterConfig, name string) error {
	ctl := eks.New(p, cfg)

	if err := ctl.CheckAuth(); err != nil {
		return err
	}

	if cfg.ID.Name != "" && name != "" {
		return fmt.Errorf("--name=%s and argument %s cannot be used at the same time", cfg.ID.Name, name)
	}

	if name != "" {
		cfg.ID.Name = name
	}

	if cfg.ID.Name == "" {
		return fmt.Errorf("--name must be set")
	}

	logger.Info("deleting EKS cluster %q", cfg.ID.Name)

	var deletedResources []string

	handleIfError := func(err error, name string) bool {
		if err != nil {
			logger.Debug("continue despite error: %v", err)
			return true
		}
		logger.Debug("deleted %q", name)
		deletedResources = append(deletedResources, name)
		return false
	}

	// We can remove all 'DeprecatedDelete*' calls in 0.2.0

	stackManager := ctl.NewStackManager(cfg)

	{
		errs := stackManager.WaitDeleteAllNodeGroups()
		if len(errs) > 0 {
			logger.Info("%d error(s) occurred while deleting nodegroup(s)", len(errs))
			for _, err := range errs {
				logger.Critical("%s\n", err.Error())
			}
			handleIfError(fmt.Errorf("failed to delete nodegroup(s)"), "nodegroup(s)")
		}
		logger.Debug("all nodegroups were deleted")
	}

	var clusterErr bool
	if waitDelete {
		clusterErr = handleIfError(stackManager.WaitDeleteCluster(), "cluster")
	} else {
		clusterErr = handleIfError(stackManager.DeleteCluster(), "cluster")
	}

	if clusterErr {
		if handleIfError(ctl.DeprecatedDeleteControlPlane(cfg.ID), "control plane") {
			handleIfError(stackManager.DeprecatedDeleteStackControlPlane(waitDelete), "stack control plane (deprecated)")
		}
	}

	handleIfError(stackManager.DeprecatedDeleteStackServiceRole(waitDelete), "service group (deprecated)")
	handleIfError(stackManager.DeprecatedDeleteStackVPC(waitDelete), "stack VPC (deprecated)")
	handleIfError(stackManager.DeprecatedDeleteStackDefaultNodeGroup(waitDelete), "default nodegroup (deprecated)")

	ctl.MaybeDeletePublicSSHKey(cfg.ID.Name)

	kubeconfig.MaybeDeleteConfig(cfg.ID)

	if len(deletedResources) == 0 {
		logger.Warning("no EKS cluster resources were found for %q", cfg.ID.Name)
	} else {
		logger.Success("the following EKS cluster resource(s) for %q will be deleted: %s. If in doubt, check CloudFormation console", cfg.ID.Name, strings.Join(deletedResources, ", "))
	}

	return nil
}
