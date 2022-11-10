package util

import (
	"crypto/x509/pkix"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/crossplane/provider-kops/apis/kops/v1alpha1"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/kops/cmd/kops/util"
	kopsapi "k8s.io/kops/pkg/apis/kops"
	kopsClient "k8s.io/kops/pkg/client/simple"
	"k8s.io/kops/pkg/kubeconfig"
	"k8s.io/kops/pkg/pki"
	"k8s.io/kops/pkg/rbac"
	"k8s.io/kops/pkg/validation"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

// GetKopsClientset returns a kops client set for a given configBase
func GetKopsClientset(stateBucket, clusterName, domain string) (kopsClient.Clientset, error) {
	configBase := fmt.Sprintf("%s/%s.%s", stateBucket, clusterName, domain)
	lastIndex := strings.LastIndex(configBase, "/")
	factoryOptions := &util.FactoryOptions{
		RegistryPath: configBase[:lastIndex],
	}

	factory := util.NewFactory(factoryOptions)

	kopsClientset, err := factory.Clientset()
	if err != nil {
		return nil, err
	}
	return kopsClientset, nil
}

// CreateClusterSpec creates a cluster spec from a cluster object
func CreateClusterSpec(cr *v1alpha1.Kops) *kopsapi.Cluster {
	clusterSpec := cr.Spec.ForProvider.ClusterSpec
	clusterSpec.ConfigBase = fmt.Sprintf("%s/%s.%s", cr.Spec.ForProvider.StateBucket, meta.GetExternalName(cr), cr.Spec.ForProvider.Domain)
	return &kopsapi.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%v.%v", meta.GetExternalName(cr), cr.Spec.ForProvider.Domain),
		},
		Spec: clusterSpec,
	}
}

// CreateInstanceGroupSpec creates an instance group spec from an instance group object
func CreateInstanceGroupSpec(cr kopsapi.InstanceGroupSpec) *kopsapi.InstanceGroup {
	return &kopsapi.InstanceGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name: cr.NodeLabels["kops.k8s.io/instancegroup"],
		},
		Spec: cr,
	}
}

// GetKubeconfigFromKopsState returns a kubeconfig for a given kops cluster
func GetKubeconfigFromKopsState(kopsCluster *kopsapi.Cluster, kopsClientset kopsClient.Clientset, kubernetesAPICertificateTTL time.Duration) (*rest.Config, error) {
	builder := kubeconfig.NewKubeconfigBuilder()

	keyStore, err := kopsClientset.KeyStore(kopsCluster)
	if err != nil {
		return nil, err
	}

	builder.Context = kopsCluster.ObjectMeta.Name
	builder.Server = fmt.Sprintf("https://api.%s", kopsCluster.ObjectMeta.Name)
	keySet, err := keyStore.FindKeyset(fi.CertificateIDCA)
	if err != nil {
		return nil, err
	}
	if keySet != nil {
		builder.CACerts, err = keySet.ToCertificateBytes()
		if err != nil {
			return nil, err
		}
	} else {
		return nil, fmt.Errorf("cannot find CA certificate")
	}
	// set default ttl to 18 hours
	if kubernetesAPICertificateTTL == 0 {
		kubernetesAPICertificateTTL = 18
	}
	req := pki.IssueCertRequest{
		Signer: fi.CertificateIDCA,
		Type:   "client",
		Subject: pkix.Name{
			CommonName:   "kops-operator",
			Organization: []string{rbac.SystemPrivilegedGroup},
		},
		Validity: kubernetesAPICertificateTTL * time.Hour,
	}
	cert, privateKey, _, err := pki.IssueCert(&req, keyStore)
	if err != nil {
		return nil, err
	}
	builder.ClientCert, err = cert.AsBytes()
	if err != nil {
		return nil, err
	}
	builder.ClientKey, err = privateKey.AsBytes()
	if err != nil {
		return nil, err
	}

	config, err := builder.BuildRestConfig()
	if err != nil {
		return nil, err
	}

	return config, nil
}

// ValidateKopsCluster validates a kops cluster
func ValidateKopsCluster(kopsClientset kopsClient.Clientset, kopsCluster *kopsapi.Cluster, igs *kopsapi.InstanceGroupList, kubernetesAPICertificateTTL time.Duration) (*validation.ValidationCluster, error) {
	config, err := GetKubeconfigFromKopsState(kopsCluster, kopsClientset, kubernetesAPICertificateTTL)
	if err != nil {
		return nil, err
	}

	k8sClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	cloud, err := cloudup.BuildCloud(kopsCluster)
	if err != nil {
		return nil, err
	}

	validator, err := validation.NewClusterValidator(kopsCluster, cloud, igs, fmt.Sprintf("https://api.%s:443", kopsCluster.ObjectMeta.Name), k8sClient)
	if err != nil {
		return nil, fmt.Errorf("unexpected error creating validator: %v", err)
	}

	result, err := validator.Validate()
	if err != nil {
		return nil, fmt.Errorf("%v", err)
	}
	return result, nil
}

// EvaluateKopsValidationResult evaluates a kops validation result
func EvaluateKopsValidationResult(validation *validation.ValidationCluster) (bool, []string) {
	result := true
	var errorMessages []string

	failures := validation.Failures
	if len(failures) > 0 {
		result = false
		for _, failure := range failures {
			errorMessages = append(errorMessages, failure.Message)
		}
	}

	nodes := validation.Nodes
	for _, node := range nodes {
		if node.Status == corev1.ConditionFalse {
			result = false
			errorMessages = append(errorMessages, fmt.Sprintf("node %s condition is %s", node.Hostname, node.Status))
		}
	}

	return result, errorMessages
}

// GenerateKubeConfig generates a kubeconfig for a given kops cluster
func GenerateKubeConfig(kopsCluster *kopsapi.Cluster, kopsClientset kopsClient.Clientset, kubernetesAPICertificateTTL time.Duration) ([]byte, error) {
	config, err := GetKubeconfigFromKopsState(kopsCluster, kopsClientset, kubernetesAPICertificateTTL)
	if err != nil {
		return nil, err
	}

	clusterName := kopsCluster.GetName()

	kc := api.Config{
		APIVersion: clusterv1.GroupVersion.String(),
		Kind:       "Cluster",
		Clusters: map[string]*api.Cluster{
			clusterName: {
				Server:                   config.Host,
				CertificateAuthorityData: config.CAData,
			},
		},
		Contexts: map[string]*api.Context{
			clusterName: {
				Cluster:  clusterName,
				AuthInfo: clusterName,
			},
		},
		CurrentContext: kopsCluster.ObjectMeta.Name,
		AuthInfos: map[string]*api.AuthInfo{
			clusterName: {
				ClientCertificateData: config.CertData,
				ClientKeyData:         config.KeyData,
			},
		},
	}

	out, err := clientcmd.Write(kc)
	if err != nil {
		return nil, errors.Wrap(err, "failed to serialize config to yaml")
	}

	return out, nil
}

// ClusterResourceUpToDate checks if the cluster resource is up to date
func ClusterResourceUpToDate(old, new *kopsapi.ClusterSpec) bool {
	new.ConfigBase = ""
	new.MasterPublicName = ""
	return reflect.DeepEqual(old, new)
}

// InstanceGroupListResourceUpToDate checks if the instance group list resource is up to date
func InstanceGroupListResourceUpToDate(old []kopsapi.InstanceGroupSpec, new *kopsapi.InstanceGroupList) bool {
	for _, oldInstance := range old {
		for _, newInstance := range new.Items {
			if oldInstance.NodeLabels["kops.k8s.io/instancegroup"] == newInstance.Spec.NodeLabels["kops.k8s.io/instancegroup"] {
				if !reflect.DeepEqual(oldInstance, newInstance.Spec) {
					return false
				}
			}
		}
	}
	return true
}

// GetClusterStatus returns the cluster status
func GetClusterStatus(kopsCluster *kopsapi.Cluster, cloud fi.Cloud) (*kopsapi.ClusterStatus, error) {
	status, err := cloud.FindClusterStatus(kopsCluster)
	if err != nil {
		return nil, err
	}
	return status, nil
}

// ErrNotFound is an error indicating that the resource was not found
func ErrNotFound(err error) bool {
	return strings.Contains(err.Error(), "not found")
}
