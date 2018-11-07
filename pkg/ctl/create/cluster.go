package create

import (
	"fmt"
	"os"
	"strings"

	"github.com/kubicorn/kubicorn/pkg/logger"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/weaveworks/eksctl/pkg/ami"
	"github.com/weaveworks/eksctl/pkg/ctl"
	"github.com/weaveworks/eksctl/pkg/eks"
	"github.com/weaveworks/eksctl/pkg/eks/api"
	"github.com/weaveworks/eksctl/pkg/kops"
	"github.com/weaveworks/eksctl/pkg/utils"
	"github.com/weaveworks/eksctl/pkg/utils/kubeconfig"
)

const (
	defaultNodeType     = "m5.large"
	defaultSSHPublicKey = "~/.ssh/id_rsa.pub"
)

var (
	writeKubeconfig    bool
	kubeconfigPath     string
	autoKubeconfigPath bool
	setContext         bool
	availabilityZones  []string

	kopsClusterNameForVPC string
	subnets               map[api.SubnetTopology]*[]string
)

func createClusterCmd() *cobra.Command {
	p := &api.ProviderConfig{}
	cfg := api.NewClusterConfig()
	ng := cfg.NewNodeGroup()

	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Create a cluster",
		Run: func(_ *cobra.Command, args []string) {
			if err := doCreateCluster(p, cfg, ng, ctl.GetNameArg(args)); err != nil {
				logger.Critical("%s\n", err.Error())
				os.Exit(1)
			}
		},
	}

	fs := cmd.Flags()

	exampleClusterName := utils.ClusterName("", "")

	fs.StringVarP(&cfg.ID.Name, "name", "n", "", fmt.Sprintf("EKS cluster name (generated if unspecified, e.g. %q)", exampleClusterName))

	fs.StringVarP(&p.Region, "region", "r", "", "AWS region")
	fs.StringVarP(&p.Profile, "profile", "p", "", "AWS credentials profile to use (overrides the AWS_PROFILE environment variable)")
	fs.StringToStringVarP(&cfg.Tags, "tags", "", map[string]string{}, `A list of KV pairs used to tag the AWS resources (e.g. "Owner=John Doe,Team=Some Team")`)

	fs.StringVarP(&ng.InstanceType, "node-type", "t", defaultNodeType, "node instance type")
	fs.IntVarP(&ng.DesiredCapacity, "nodes", "N", api.DefaultNodeCount, "total number of nodes (desired capacity of AGS)")

	// TODO: https://github.com/weaveworks/eksctl/issues/28
	fs.IntVarP(&ng.MinSize, "nodes-min", "m", 0, "minimum nodes in ASG (leave unset for a static nodegroup)")
	fs.IntVarP(&ng.MaxSize, "nodes-max", "M", 0, "maximum nodes in ASG (leave unset for a static nodegroup)")

	fs.IntVarP(&ng.VolumeSize, "node-volume-size", "", 0, "Node volume size (in GB)")
	fs.IntVar(&ng.MaxPodsPerNode, "max-pods-per-node", 0, "maximum number of pods per node (set automatically if unspecified)")
	fs.StringSliceVar(&availabilityZones, "zones", nil, "(auto-select if unspecified)")

	fs.BoolVar(&ng.AllowSSH, "ssh-access", false, "control SSH access for nodes")
	fs.StringVar(&ng.SSHPublicKeyPath, "ssh-public-key", defaultSSHPublicKey, "SSH public key to use for nodes (import from local path, or use existing EC2 key pair)")

	fs.BoolVar(&writeKubeconfig, "write-kubeconfig", true, "toggle writing of kubeconfig")
	fs.BoolVar(&autoKubeconfigPath, "auto-kubeconfig", false, fmt.Sprintf("save kubeconfig file by cluster name, e.g. %q", kubeconfig.AutoPath(exampleClusterName)))
	fs.StringVar(&kubeconfigPath, "kubeconfig", kubeconfig.DefaultPath, "path to write kubeconfig (incompatible with --auto-kubeconfig)")
	fs.BoolVar(&setContext, "set-kubeconfig-context", true, "if true then current-context will be set in kubeconfig; if a context is already set then it will be overwritten")

	fs.DurationVar(&p.WaitTimeout, "aws-api-timeout", api.DefaultWaitTimeout, "")
	// TODO deprecate in 0.2.0
	if err := fs.MarkHidden("aws-api-timeout"); err != nil {
		logger.Debug("ignoring error %q", err.Error())
	}
	fs.DurationVar(&p.WaitTimeout, "timeout", api.DefaultWaitTimeout, "max wait time in any polling operations")

	fs.BoolVar(&cfg.Addons.WithIAM.PolicyAmazonEC2ContainerRegistryPowerUser, "full-ecr-access", false, "enable full access to ECR")
	fs.BoolVar(&cfg.Addons.WithIAM.PolicyAutoScaling, "asg-access", false, "enable iam policy dependency for cluster-autoscaler")
	fs.BoolVar(&cfg.Addons.Storage, "storage-class", true, "if true (default) then a default StorageClass of type gp2 provisioned by EBS will be created")

	fs.StringVar(&ng.AMI, "node-ami", ami.ResolverStatic, "Advanced use cases only. If 'static' is supplied (default) then eksctl will use static AMIs; if 'auto' is supplied then eksctl will automatically set the AMI based on region/instance type; if any other value is supplied it will override the AMI to use for the nodes. Use with extreme care.")
	fs.StringVar(&ng.AMIFamily, "node-ami-family", ami.ImageFamilyAmazonLinux2, "Advanced use cases only. If 'AmazonLinux2' is supplied (default), then eksctl will use the offical AWS EKS AMIs (Amazon Linux 2); if 'Ubuntu1804' is supplied, then eksctl will use the offical Canonical EKS AMIs (Ubuntu 18.04).")

	fs.StringVar(&kopsClusterNameForVPC, "vpc-from-kops-cluster", "", "re-use VPC from a given kops cluster")

	fs.IPNetVar(cfg.VPC.CIDR, "vpc-cidr", api.DefaultCIDR(), "global CIDR to use for VPC")

	subnets = map[api.SubnetTopology]*[]string{
		api.SubnetTopologyPrivate: fs.StringSlice("vpc-private-subnets", nil, "re-use private subnets of an existing VPC"),
		api.SubnetTopologyPublic:  fs.StringSlice("vpc-public-subnets", nil, "re-use public subnets of an existing VPC"),
	}

	fs.BoolVarP(&ng.PrivateNetworking, "node-private-networking", "P", false, "whether to make initial nodegroup networking private")

	return cmd
}

func doCreateCluster(p *api.ProviderConfig, cfg *api.ClusterConfig, ng *api.NodeGroup, nameArg string) error {
	cl := cfg.ID
	ctl := eks.New(p, cfg)

	if !ctl.IsSupportedRegion() {
		return fmt.Errorf("--region=%s is not supported - use one of: %s", cl.Region, strings.Join(api.SupportedRegions(), ", "))
	}
	logger.Info("using region %s", cl.Region)

	if err := ctl.CheckAuth(); err != nil {
		return err
	}

	if utils.ClusterName(cl.Name, nameArg) == "" {
		return fmt.Errorf("--name=%s and argument %s cannot be used at the same time", cl.Name, nameArg)
	}
	cl.Name = utils.ClusterName(cl.Name, nameArg)

	if autoKubeconfigPath {
		if kubeconfigPath != kubeconfig.DefaultPath {
			return fmt.Errorf("--kubeconfig and --auto-kubeconfig cannot be used at the same time")
		}
		kubeconfigPath = kubeconfig.AutoPath(cl.Name)
	}

	if ng.SSHPublicKeyPath == "" {
		return fmt.Errorf("--ssh-public-key must be non-empty string")
	}

	createOrImportVPC := func() error {
		subnetsGiven := len(*subnets[api.SubnetTopologyPrivate])+len(*subnets[api.SubnetTopologyPublic]) != 0

		subnetInfo := func() string {
			return fmt.Sprintf("VPC (%s) and subnets (private:%v public:%v)",
				cfg.VPC.ID, cfg.SubnetIDs(api.SubnetTopologyPrivate), cfg.SubnetIDs(api.SubnetTopologyPublic))
		}

		customNetworkingNotice := "custom VPC/subnets will be used; if resulting cluster doesn't function as expected, make sure to review the configuration of VPC/subnets"

		canUseForPrivateNodeGroup := func() error {
			if ng.PrivateNetworking && !cfg.HasSufficientPrivateSubnets() {
				return fmt.Errorf("none or too few private subnets to use with --node-private-networking")
			}
			return nil
		}

		if !subnetsGiven && kopsClusterNameForVPC == "" {
			// default: create dedicated VPC
			if err := ctl.SetAvailabilityZones(cfg, availabilityZones); err != nil {
				return err
			}
			if err := ctl.SetSubnets(cfg); err != nil {
				return err
			}
			return nil
		}

		if kopsClusterNameForVPC != "" {
			// import VPC from a given kops cluster
			if len(availabilityZones) != 0 {
				return fmt.Errorf("--vpc-from-kops-cluster and --zones cannot be used at the same time")
			}

			if subnetsGiven {
				return fmt.Errorf("--vpc-from-kops-cluster and --vpc-private-subnets/--vpc-public-subnets cannot be used at the same time")
			}

			kw, err := kops.NewWrapper(p.Region, kopsClusterNameForVPC)
			if err != nil {
				return err
			}

			if err := kw.UseVPC(cfg); err != nil {
				return err
			}

			if err := canUseForPrivateNodeGroup(); err != nil {
				return err
			}

			logger.Success("using %s from kops cluster %q", subnetInfo(), kopsClusterNameForVPC)
			logger.Warning(customNetworkingNotice)
			return nil
		}

		// use subnets as specified by --vpc-{private,public}-subnets flags

		if len(availabilityZones) != 0 {
			return fmt.Errorf("--vpc-private-subnets/--vpc-public-subnets and --zones cannot be used at the same time")
		}

		for topology := range subnets {
			if err := ctl.UseSubnets(cfg, topology, *subnets[topology]); err != nil {
				return err
			}
		}

		if err := cfg.HasSufficientSubnets(); err != nil {
			logger.Critical("unable to use given %s", subnetInfo())
			return err
		}

		if err := canUseForPrivateNodeGroup(); err != nil {
			return err
		}

		logger.Success("using existing %s", subnetInfo())
		logger.Warning(customNetworkingNotice)
		return nil
	}

	if err := createOrImportVPC(); err != nil {
		return err
	}

	if err := ctl.EnsureAMI(ng); err != nil {
		return err
	}

	if err := ctl.LoadSSHPublicKey(cl.Name, ng); err != nil {
		return err
	}

	logger.Debug("cfg = %#v", cfg)

	logger.Info("creating %s", cl.LogString())

	{ // core action
		stackManager := ctl.NewStackManager(cfg)
		logger.Info("will create 2 separate CloudFormation stacks for cluster itself and the initial nodegroup")
		logger.Info("if you encounter any issues, check CloudFormation console or try 'eksctl utils describe-stacks --region=%s --name=%s'", cl.Region, cl.Name)
		errs := stackManager.CreateClusterWithNodeGroups()
		// read any errors (it only gets non-nil errors)
		if len(errs) > 0 {
			logger.Info("%d error(s) occurred and cluster hasn't been created properly, you may wish to check CloudFormation console", len(errs))
			logger.Info("to cleanup resources, run 'eksctl delete cluster --region=%s --name=%s'", cl.Region, cl.Name)
			for _, err := range errs {
				logger.Critical("%s\n", err.Error())
			}
			return fmt.Errorf("failed to create cluster %q", cl.Name)
		}
	}

	logger.Success("all EKS cluster resource for %q had been created", cl.Name)

	// obtain cluster credentials, write kubeconfig

	{ // post-creation action
		clientConfigBase, err := ctl.NewClientConfig(cfg)
		if err != nil {
			return err
		}

		if writeKubeconfig {
			config := clientConfigBase.WithExecAuthenticator()
			kubeconfigPath, err = kubeconfig.Write(kubeconfigPath, config.Client, setContext)
			if err != nil {
				return errors.Wrap(err, "writing kubeconfig")
			}

			logger.Success("saved kubeconfig as %q", kubeconfigPath)
		} else {
			kubeconfigPath = ""
		}

		// create Kubernetes client
		clientSet, err := clientConfigBase.NewClientSetWithEmbeddedToken()
		if err != nil {
			return err
		}

		if err = ctl.WaitForControlPlane(cl, clientSet); err != nil {
			return err
		}

		// authorise nodes to join
		if err = ctl.CreateNodeGroupAuthConfigMap(clientSet, ng); err != nil {
			return err
		}

		// wait for nodes to join
		if err = ctl.WaitForNodes(clientSet, ng); err != nil {
			return err
		}

		// add default storage class
		if cfg.Addons.Storage {
			if err = ctl.AddDefaultStorageClass(clientSet); err != nil {
				return err
			}
		}

		// check kubectl version, and offer install instructions if missing or old
		// also check heptio-authenticator
		// TODO: https://github.com/weaveworks/eksctl/issues/30
		env, err := ctl.GetCredentialsEnv()
		if err != nil {
			return err
		}
		if err := utils.CheckAllCommands(kubeconfigPath, setContext, clientConfigBase.ContextName, env); err != nil {
			logger.Critical("%s\n", err.Error())
			logger.Info("cluster should be functional despite missing (or misconfigured) client binaries")
		}

		// If GPU instance type, give instructions
		if utils.IsGPUInstanceType(ng.InstanceType) {
			logger.Info("as you are using a GPU optimized instance type you will need to install NVIDIA Kubernetes device plugin.")
			logger.Info("\t see the following page for instructions: https://github.com/NVIDIA/k8s-device-plugin")
		}
	}

	logger.Success("%s is ready", cl.LogString())

	return nil
}
