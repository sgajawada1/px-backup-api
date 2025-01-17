package aws

import (
	"context"
	"encoding/base64"
	"fmt"

	api "github.com/portworx/px-backup-api/pkg/apis/v1"
	"github.com/portworx/px-backup-api/pkg/kubeauth"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/aws-iam-authenticator/pkg/token"

	awsapi "github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/eks"
	awscredentials "github.com/libopenstorage/secrets/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
)

const (
	pluginName = "aws"
)

type aws struct {
}

// Init initializes the gcp auth plugin
func (a *aws) Init() error {
	return nil
}

func (a *aws) UpdateClient(
	conn *grpc.ClientConn,
	ctx context.Context,
	cloudCredentialName string,
	cloudCredentialUID string,
	orgID string,
	client *rest.Config,
	clientConfig *clientcmdapi.Config,
) (bool, string, error) {
	// AWS does not support returning kubeconfigs
	var emptyKubeconfig string
	if client.ExecProvider != nil {
		if client.ExecProvider.Command == "aws-iam-authenticator" || client.ExecProvider.Command == "aws" {
			if cloudCredentialName == "" {
				return false, emptyKubeconfig, fmt.Errorf("CloudCredential not provided for EKS cluster")
			}

			cloudCredentialClient := api.NewCloudCredentialClient(conn)
			resp, err := cloudCredentialClient.Inspect(
				ctx,
				&api.CloudCredentialInspectRequest{
					Name:           cloudCredentialName,
					Uid: cloudCredentialUID,
					OrgId:          orgID,
					IncludeSecrets: true,
				},
			)
			if err != nil {
				return false, emptyKubeconfig, err
			}
			cloudCredential := resp.GetCloudCredential()
			if err := a.updateClient(cloudCredential, client); err != nil {
				return false, emptyKubeconfig, err
			}
			return true, emptyKubeconfig, nil
		} // else not an aws kubeauth provider
	}
	return false, emptyKubeconfig, nil
}

func (a *aws) UpdateClientByCredObject(
	cloudCred *api.CloudCredentialObject,
	client *rest.Config,
	clientConfig *clientcmdapi.Config,
) (bool, string, error) {
	// AWS does not support returning kubeconfigs
	var emptyKubeconfig string
	if client.ExecProvider != nil {
		if client.ExecProvider.Command == "aws-iam-authenticator" || client.ExecProvider.Command == "aws" {
			if err := a.updateClient(cloudCred, client); err != nil {
				return false, emptyKubeconfig, err
			}
			return true, emptyKubeconfig, nil
		} // else not an aws kubeauth provider
	}

	return false, emptyKubeconfig, nil
}

// updateClient assumes that the provided rest client is not nil
// and has the aws exec provider field set
func (a *aws) updateClient(
	cloudCredential *api.CloudCredentialObject,
	client *rest.Config,
) error {
	if cloudCredential == nil {
		return fmt.Errorf("CloudCredential not provided for EKS cluster")
	}
	if cloudCredential.GetCloudCredentialInfo().GetType() != api.CloudCredentialInfo_AWS {
		return fmt.Errorf("need AWS CloudCredential for EKS cluster. Provided %v", cloudCredential.GetCloudCredentialInfo().GetType())
	}

	if client.ExecProvider.Env == nil {
		client.ExecProvider.Env = make([]clientcmdapi.ExecEnvVar, 0)
	}
	client.ExecProvider.Env = append(client.ExecProvider.Env, clientcmdapi.ExecEnvVar{
		Name:  "AWS_ACCESS_KEY",
		Value: cloudCredential.GetCloudCredentialInfo().GetAwsConfig().GetAccessKey(),
	})
	client.ExecProvider.Env = append(client.ExecProvider.Env, clientcmdapi.ExecEnvVar{
		Name:  "AWS_ACCESS_KEY_ID",
		Value: cloudCredential.GetCloudCredentialInfo().GetAwsConfig().GetAccessKey(),
	})
	client.ExecProvider.Env = append(client.ExecProvider.Env, clientcmdapi.ExecEnvVar{
		Name:  "AWS_SECRET_KEY",
		Value: cloudCredential.GetCloudCredentialInfo().GetAwsConfig().GetSecretKey(),
	})
	client.ExecProvider.Env = append(client.ExecProvider.Env, clientcmdapi.ExecEnvVar{
		Name:  "AWS_SECRET_ACCESS_KEY",
		Value: cloudCredential.GetCloudCredentialInfo().GetAwsConfig().GetSecretKey(),
	})

	// Remove the profile env if present since we are passing in the creds through env
	tempEnv := make([]clientcmdapi.ExecEnvVar, 0)
	for _, env := range client.ExecProvider.Env {
		if env.Name == "AWS_PROFILE" {
			continue
		}
		tempEnv = append(tempEnv, env)
	}
	client.ExecProvider.Env = tempEnv
	return nil
}

func (a *aws) GetClient(
	cloudCredential *api.CloudCredentialObject,
	clusterName string,
	region string,
) (*kubeauth.PluginClient, error) {
	awsConfig := cloudCredential.GetCloudCredentialInfo().GetAwsConfig()
	if awsConfig == nil {
		return nil, fmt.Errorf("cloud credentials are not for aws")
	}
	return GetRestConfigForCluster(clusterName, awsConfig, region)

}

func (a *aws) GetAllClients(
	cloudCredential *api.CloudCredentialObject,
	maxResults int64,
	config interface{},
) (map[string]*kubeauth.PluginClient, *string, error) {
	awsConfig := cloudCredential.GetCloudCredentialInfo().GetAwsConfig()
	if awsConfig == nil {
		return nil, nil, fmt.Errorf("cloud credentials are not for aws")
	}
	return GetRestConfigForAllClusters(awsConfig, maxResults, config)

}

func runningOnEc2() bool {
	// Check if we are running on EC2 instance
	var runningOnEc2 bool
	s, err := session.NewSession()
	if err != nil {
		return runningOnEc2
	}
	c := ec2metadata.New(s)
	_, err = c.GetMetadata("mac")
	if err == nil {
		runningOnEc2 = true
	}
	return runningOnEc2
}

func GetRestConfigForCluster(clusterName string, awsConfig *api.AWSConfig, region string) (*kubeauth.PluginClient, error) {
	awsCreds, err := awscredentials.NewAWSCredentials(
		awsConfig.GetAccessKey(),
		awsConfig.GetSecretKey(),
		"",
		runningOnEc2(),
	)
	if err != nil {
		return nil, err
	}
	creds, err := awsCreds.Get()
	if err != nil {
		return nil, err
	}
	sess := session.Must(session.NewSession(&awsapi.Config{
		Region:      awsapi.String(region),
		Credentials: creds,
	}))
	eksSvc := eks.New(sess)

	describeClusterOutput, err := eksSvc.DescribeCluster(&eks.DescribeClusterInput{
		Name: &clusterName,
	})
	if err != nil {
		logrus.Errorf("Failed to describe cluster %v: %v", clusterName, err)
		return nil, err
	}
	restConfig, kubeConfig, err := getRestConfig(describeClusterOutput.Cluster, sess)
	if err != nil {
		logrus.Infof("Failed to create a clientset for cluster %v: %v", clusterName, err)
		return nil, err
	}
	return &kubeauth.PluginClient{
		Kubeconfig: kubeConfig,
		Rest:       restConfig, 
		Uid:        clusterName, // aws does not have uid
	}, nil
}

//func GetRestConfigForAllClusters(awsConfig *api.AWSConfig, region string) (map[string]*kubeauth.PluginClient, error) {
func GetRestConfigForAllClusters(
	awsConfig *api.AWSConfig,
	maxResults int64,
	config interface{},
) (map[string]*kubeauth.PluginClient, *string, error) {
	funct := "GetRestConfigForAllClusters"
	awsCfg := config.(*api.ManagedClusterEnumerateRequest_AWSConfig)
	awsCreds, err := awscredentials.NewAWSCredentials(
		awsConfig.GetAccessKey(),
		awsConfig.GetSecretKey(),
		"",
		runningOnEc2(),
	)
	if err != nil {
		return nil, nil, err
	}
	creds, err := awsCreds.Get()
	if err != nil {
		return nil, nil, err
	}
	sess := session.Must(session.NewSession(&awsapi.Config{
		Region:      awsapi.String(awsCfg.Region),
		Credentials: creds,
	}))
	eksSvc := eks.New(sess)
	listClustersInput := eks.ListClustersInput{}
	if maxResults != 0 {
		listClustersInput.MaxResults = &maxResults
	}
	
	if len(awsCfg.NextToken) != 0 {
		listClustersInput.NextToken = &awsCfg.NextToken
	}

	listClusterOutput, err := eksSvc.ListClusters(&listClustersInput)
	if err != nil {
		return nil, nil, err
	}
	logrus.Tracef("%s: listClusterOutput: %v", funct, listClusterOutput)
	restConfigs := make(map[string]*kubeauth.PluginClient)
	for _, clusterName := range listClusterOutput.Clusters {
		describeClusterOutput, err := eksSvc.DescribeCluster(&eks.DescribeClusterInput{
			Name: clusterName,
		})
		if err != nil {
			logrus.Errorf("Failed to describe cluster %v: %v", *clusterName, err)
			continue
		}
		restConfig, kubeConfig, err := getRestConfig(describeClusterOutput.Cluster, sess)
		// On error continue to next cluster as we don't want to stop the 
		// scan for one cluster error.
		// Error could be genuine where IAM user doesn't have permission to access
		// a cluster.
		if err != nil {
			logrus.Infof("skipping cluster %v",  awsapi.StringValue(clusterName))
			continue
		}
		restConfigs[awsapi.StringValue(clusterName)] = &kubeauth.PluginClient{
			Kubeconfig: kubeConfig,
			Rest:       restConfig,
			Uid:        awsapi.StringValue(clusterName), // aws does not have uid
			Version:    awsapi.StringValue(describeClusterOutput.Cluster.Version),
		}
	}
	return restConfigs, listClusterOutput.NextToken, nil

}

func getRestConfig(cluster *eks.Cluster, sess *session.Session) (*rest.Config, string, error) {
	gen, err := token.NewGenerator(true, false)
	if err != nil {
		return nil, "", err
	}
	opts := &token.GetTokenOptions{
		ClusterID: awsapi.StringValue(cluster.Name),
		Session:   sess,
	}
	tok, err := gen.GetWithOptions(opts)
	if err != nil {
		return nil, "", err
	}
	ca, err := base64.StdEncoding.DecodeString(awsapi.StringValue(cluster.CertificateAuthority.Data))
	if err != nil {

		return nil, "", err
	}

	restConfig := &rest.Config{
		Host:        awsapi.StringValue(cluster.Endpoint),
		BearerToken: tok.Token,
		TLSClientConfig: rest.TLSClientConfig{
			CAData: ca,
		},
	}

	if err != nil {
		return nil, "", err
	}

	// Copy cert data as is kubeconfig
	caData := awsapi.StringValue(cluster.CertificateAuthority.Data)
	return restConfig, buildKubeconfig(awsapi.StringValue(cluster.Endpoint), awsapi.StringValue(cluster.Name), caData), nil
}

// the kubeconfig spec taken from - https://docs.aws.amazon.com/eks/latest/userguide/create-kubeconfig.html#create-kubeconfig-manually
func buildKubeconfig(
	clusterEndpoint string,
	clusterName string,
	certData string,
) string {
	return fmt.Sprintf(`
apiVersion: v1
clusters:
- cluster:
    server: %s
    certificate-authority-data: %s
  name: kubernetes
contexts:
- context:
    cluster: kubernetes
    user: aws
  name: aws
current-context: aws
kind: Config
preferences: {}
users:
- name: aws
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1alpha1
      command: aws-iam-authenticator
      args:
        - "token"
        - "-i"
        - "%s"
`, clusterEndpoint, string(certData), clusterName)
}

func init() {
	if err := kubeauth.Register(pluginName, &aws{}); err != nil {
		logrus.Panicf("Error registering aws auth plugin: %v", err)
	}
}
