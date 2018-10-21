package eks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/cloudformation"
	humanize "github.com/dustin/go-humanize"
	"go.uber.org/zap"
)

func (md *embedded) createWorkerNode() error {
	if md.cfg.ClusterState.CFStackNodeGroupKeyPairName == "" {
		return errors.New("cannot create worker node without key name")
	}
	if md.cfg.ClusterState.CFStackNodeGroupName == "" {
		return errors.New("cannot create empty worker node")
	}

	now := time.Now().UTC()
	h, _ := os.Hostname()
	v := workerNodeStack{
		Description:   md.cfg.ClusterName + "-worker-node-stack",
		TagKey:        md.cfg.Tag,
		TagValue:      md.cfg.ClusterName,
		Hostname:      h,
		EnableNodeSSH: false,
	}
	s, err := createWorkerNodeTemplate(v)
	if err != nil {
		return err
	}

	_, err = md.cf.CreateStack(&cloudformation.CreateStackInput{
		StackName: aws.String(md.cfg.ClusterState.CFStackNodeGroupName),
		Tags: []*cloudformation.Tag{
			{
				Key:   aws.String(md.cfg.Tag),
				Value: aws.String(md.cfg.ClusterName),
			},
			{
				Key:   aws.String("HOSTNAME"),
				Value: aws.String(h),
			},
		},

		// TemplateURL: aws.String("https://amazon-eks.s3-us-west-2.amazonaws.com/cloudformation/2018-08-30/amazon-eks-nodegroup.yaml"),
		TemplateBody: aws.String(s),

		Parameters: []*cloudformation.Parameter{
			{
				ParameterKey:   aws.String("ClusterName"),
				ParameterValue: aws.String(md.cfg.ClusterName),
			},
			{
				ParameterKey:   aws.String("NodeGroupName"),
				ParameterValue: aws.String(md.cfg.ClusterState.CFStackNodeGroupName),
			},
			{
				ParameterKey:   aws.String("KeyName"),
				ParameterValue: aws.String(md.cfg.ClusterState.CFStackNodeGroupKeyPairName),
			},
			{
				ParameterKey:   aws.String("NodeImageId"),
				ParameterValue: aws.String(md.cfg.WorkerNodeAMI),
			},
			{
				ParameterKey:   aws.String("NodeInstanceType"),
				ParameterValue: aws.String(md.cfg.WorkerNodeInstanceType),
			},
			{
				ParameterKey:   aws.String("NodeAutoScalingGroupMinSize"),
				ParameterValue: aws.String(fmt.Sprintf("%d", md.cfg.WorkderNodeASGMin)),
			},
			{
				ParameterKey:   aws.String("NodeAutoScalingGroupMaxSize"),
				ParameterValue: aws.String(fmt.Sprintf("%d", md.cfg.WorkderNodeASGMax)),
			},
			{
				ParameterKey:   aws.String("NodeVolumeSize"),
				ParameterValue: aws.String(fmt.Sprintf("%d", md.cfg.WorkerNodeVolumeSizeGB)),
			},
			{
				ParameterKey:   aws.String("VpcId"),
				ParameterValue: aws.String(md.cfg.ClusterState.CFStackVPCID),
			},
			{
				ParameterKey:   aws.String("Subnets"),
				ParameterValue: aws.String(strings.Join(md.cfg.ClusterState.CFStackVPCSubnetIDs, ",")),
			},
			{
				ParameterKey:   aws.String("ClusterControlPlaneSecurityGroup"),
				ParameterValue: aws.String(md.cfg.ClusterState.CFStackVPCSecurityGroupID),
			},
		},

		Capabilities: aws.StringSlice([]string{"CAPABILITY_IAM"}),
	})
	if err != nil {
		return err
	}
	md.cfg.ClusterState.StatusWorkerNodeCreated = true
	md.cfg.Sync()

	// usually takes 3-minute
	md.lg.Info("waiting for 2-minute")
	select {
	case <-md.stopc:
		md.lg.Info("interrupted worker node creation")
		return nil
	case <-time.After(2 * time.Minute):
	}

	waitTime := 5*time.Minute + 2*time.Duration(md.cfg.WorkderNodeASGMax)*time.Minute
	retryStart := time.Now().UTC()
	for time.Now().UTC().Sub(retryStart) < waitTime {
		select {
		case <-md.stopc:
			return nil
		default:
		}

		var do *cloudformation.DescribeStacksOutput
		do, err = md.cf.DescribeStacks(&cloudformation.DescribeStacksInput{
			StackName: aws.String(md.cfg.ClusterState.CFStackNodeGroupName),
		})
		if err != nil {
			md.lg.Warn("failed to describe worker node", zap.Error(err))
			md.cfg.ClusterState.CFStackNodeGroupStatus = err.Error()
			md.cfg.Sync()
			time.Sleep(5 * time.Second)
			continue
		}

		if len(do.Stacks) != 1 {
			return fmt.Errorf("%q expects 1 Stack, got %v", md.cfg.ClusterState.CFStackNodeGroupName, do.Stacks)
		}

		md.cfg.ClusterState.CFStackNodeGroupStatus = *do.Stacks[0].StackStatus
		if isCFCreateFailed(md.cfg.ClusterState.CFStackNodeGroupStatus) {
			return fmt.Errorf("failed to create %q (%q)",
				md.cfg.ClusterState.CFStackNodeGroupName,
				md.cfg.ClusterState.CFStackNodeGroupStatus,
			)
		}

		for _, op := range do.Stacks[0].Outputs {
			if *op.OutputKey == "NodeInstanceRole" {
				md.lg.Info("found NodeInstanceRole", zap.String("output", *op.OutputValue))
				md.cfg.ClusterState.CFStackNodeGroupWorkerNodeInstanceRoleARN = *op.OutputValue
			}
		}

		if md.cfg.ClusterState.CFStackNodeGroupStatus == "CREATE_COMPLETE" {
			break
		}

		md.cfg.Sync()

		md.lg.Info("creating worker node", zap.String("request-started", humanize.RelTime(now, time.Now().UTC(), "ago", "from now")))
		time.Sleep(15 * time.Second)
	}

	if err != nil {
		md.lg.Info("failed to create worker node",
			zap.String("name", md.cfg.ClusterState.CFStackNodeGroupName),
			zap.String("stack-status", md.cfg.ClusterState.CFStackNodeGroupStatus),
			zap.String("request-started", humanize.RelTime(now, time.Now().UTC(), "ago", "from now")),
			zap.Error(err),
		)
		return err
	}

	if md.cfg.ClusterState.CFStackNodeGroupWorkerNodeInstanceRoleARN == "" {
		return errors.New("cannot find node group instance role ARN")
	}

	md.lg.Info("created worker node",
		zap.String("name", md.cfg.ClusterState.CFStackNodeGroupName),
		zap.String("stack-status", md.cfg.ClusterState.CFStackNodeGroupStatus),
		zap.String("request-started", humanize.RelTime(now, time.Now().UTC(), "ago", "from now")),
	)

	// write config map file
	var cmPath string
	cmPath, err = writeConfigMapNodeAuth(md.cfg.ClusterState.CFStackNodeGroupWorkerNodeInstanceRoleARN)
	if err != nil {
		return err
	}
	defer os.RemoveAll(cmPath)

	kcfgPath := md.cfg.KubeConfigPath
	applied := false
	retryStart = time.Now().UTC()
	for time.Now().UTC().Sub(retryStart) < waitTime {
		select {
		case <-md.stopc:
			return nil
		default:
		}

		// TODO: use "k8s.io/client-go"
		if !applied {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			cmd := md.kubectl.CommandContext(ctx,
				md.kubectlPath,
				"--kubeconfig="+kcfgPath,
				"apply", "--filename="+cmPath,
			)
			var kexo []byte
			kexo, err = cmd.CombinedOutput()
			cancel()
			if err != nil {
				if strings.Contains(err.Error(), "unknown flag:") {
					return fmt.Errorf("unknown flag %s", string(kexo))
				}
				md.lg.Warn("failed to apply config map",
					zap.String("stack-name", md.cfg.ClusterState.CFStackNodeGroupName),
					zap.String("output", string(kexo)),
					zap.Error(err),
				)
				md.cfg.ClusterState.EC2NodeGroupStatus = err.Error()
				md.cfg.Sync()
				time.Sleep(5 * time.Second)
				continue
			}
			applied = true
			md.lg.Info("kubectl apply completed", zap.String("output", string(kexo)))
		}

		// TODO: use "k8s.io/client-go"
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := md.kubectl.CommandContext(ctx,
			md.kubectlPath,
			"--kubeconfig="+kcfgPath,
			"get", "nodes", "-ojson",
		)
		var kexo []byte
		kexo, err = cmd.CombinedOutput()
		cancel()
		if err != nil {
			if strings.Contains(err.Error(), "unknown flag:") {
				return fmt.Errorf("unknown flag %s", string(kexo))
			}
			md.lg.Warn("failed to get nodes", zap.String("output", string(kexo)), zap.Error(err))
			md.cfg.ClusterState.EC2NodeGroupStatus = err.Error()
			md.cfg.Sync()
			time.Sleep(5 * time.Second)
			continue
		}

		var ns *nodeList
		ns, err = kubectlGetNodes(kexo)
		if err != nil {
			md.lg.Warn("failed to parse get nodes output", zap.Error(err))
			md.cfg.ClusterState.EC2NodeGroupStatus = err.Error()
			md.cfg.Sync()
			time.Sleep(10 * time.Second)
			continue
		}
		nodesN := len(ns.Items)
		readyN := countReadyNodes(ns)
		md.lg.Info(
			"created worker nodes",
			zap.Int("created-nodes", nodesN),
			zap.Int("ready-nodes", readyN),
			zap.Int("worker-node-asg-min", md.cfg.WorkderNodeASGMin),
			zap.Int("worker-node-asg-max", md.cfg.WorkderNodeASGMax),
		)
		if readyN == md.cfg.WorkderNodeASGMax {
			md.cfg.ClusterState.EC2NodeGroupStatus = "READY"
			md.cfg.Sync()
			break
		}

		md.cfg.ClusterState.EC2NodeGroupStatus = fmt.Sprintf("%d AVAILABLE", nodesN)
		md.cfg.Sync()

		time.Sleep(15 * time.Second)
	}

	if md.cfg.ClusterState.EC2NodeGroupStatus != "READY" {
		return fmt.Errorf(
			"worker nodes are not ready (status %q, ASG max %d)",
			md.cfg.ClusterState.EC2NodeGroupStatus,
			md.cfg.WorkderNodeASGMax,
		)
	}

	md.lg.Info(
		"enabled node group to join cluster",
		zap.String("name", md.cfg.ClusterState.CFStackNodeGroupName),
		zap.String("request-started", humanize.RelTime(now, time.Now().UTC(), "ago", "from now")),
	)
	return md.cfg.Sync()
}

func (md *embedded) deleteWorkerNode() error {
	if !md.cfg.ClusterState.StatusWorkerNodeCreated {
		return nil
	}
	defer func() {
		md.cfg.ClusterState.StatusWorkerNodeCreated = false
		md.cfg.Sync()
	}()

	if md.cfg.ClusterState.CFStackNodeGroupName == "" {
		return errors.New("cannot delete empty worker node")
	}

	_, err := md.cf.DeleteStack(&cloudformation.DeleteStackInput{
		StackName: aws.String(md.cfg.ClusterState.CFStackNodeGroupName),
	})
	if err != nil {
		md.cfg.ClusterState.CFStackNodeGroupStatus = err.Error()
		md.cfg.ClusterState.EC2NodeGroupStatus = err.Error()
		return err
	}

	md.cfg.Sync()

	md.lg.Info("waiting for 1-minute")
	time.Sleep(time.Minute)

	waitTime := 5*time.Minute + 2*time.Duration(md.cfg.WorkderNodeASGMax)*time.Minute
	md.lg.Info(
		"periodically fetching node stack status",
		zap.String("name", md.cfg.ClusterState.CFStackNodeGroupName),
		zap.String("stack-name", md.cfg.ClusterState.CFStackNodeGroupName),
		zap.Duration("duration", waitTime),
	)

	retryStart := time.Now().UTC()
	for time.Now().UTC().Sub(retryStart) < waitTime {
		var do *cloudformation.DescribeStacksOutput
		do, err = md.cf.DescribeStacks(&cloudformation.DescribeStacksInput{
			StackName: aws.String(md.cfg.ClusterState.CFStackNodeGroupName),
		})
		if err == nil {
			md.cfg.ClusterState.CFStackNodeGroupStatus = *do.Stacks[0].StackStatus
			md.cfg.ClusterState.EC2NodeGroupStatus = *do.Stacks[0].StackStatus
			md.lg.Info("deleting worker node", zap.String("request-started", humanize.RelTime(retryStart, time.Now().UTC(), "ago", "from now")))
			time.Sleep(5 * time.Second)
			continue
		}

		if isCFDeletedGoClient(md.cfg.ClusterState.CFStackNodeGroupName, err) {
			err = nil
			md.cfg.ClusterState.CFStackNodeGroupStatus = "DELETE_COMPLETE"
			md.cfg.ClusterState.EC2NodeGroupStatus = "DELETE_COMPLETE"
			break
		}

		md.cfg.ClusterState.CFStackNodeGroupStatus = err.Error()
		md.cfg.ClusterState.EC2NodeGroupStatus = err.Error()

		md.lg.Warn("failed to describe worker node", zap.Error(err))
		md.cfg.Sync()
		time.Sleep(10 * time.Second)
	}

	if err != nil {
		md.lg.Info("failed to delete worker node", zap.String("request-started", humanize.RelTime(retryStart, time.Now().UTC(), "ago", "from now")), zap.Error(err))
		return err
	}

	md.lg.Info(
		"deleted worker node",
		zap.String("name", md.cfg.ClusterState.CFStackNodeGroupName),
		zap.String("request-started", humanize.RelTime(retryStart, time.Now().UTC(), "ago", "from now")),
	)
	return md.cfg.Sync()
}
