package generator

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"log"
	"strings"
	"time"

	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/argoproj/argo-cd/v3/hack/gen-resources/util"
	argoappv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v3/util/db"
	"github.com/argoproj/argo-cd/v3/util/helm"
)

const POD_PREFIX = "vcluster"

type Cluster struct {
	Server                   string `yaml:"server"`
	CertificateAuthorityData string `yaml:"certificate-authority-data,omitempty"`
}

type AuthInfo struct {
	ClientCertificateData string `yaml:"client-certificate-data,omitempty"`
	ClientKeyData         string `yaml:"client-key-data,omitempty"`
}

type NamedCluster struct {
	// Name is the nickname for this Cluster
	Name string `yaml:"name"`
	// Cluster holds the cluster information
	Cluster Cluster `yaml:"cluster"`
}

type NamedAuthInfo struct {
	// Name is the nickname for this AuthInfo
	Name string `yaml:"name"`
	// AuthInfo holds the auth information
	AuthInfo AuthInfo `yaml:"user"`
}

type Config struct {
	Clusters  []NamedCluster  `yaml:"clusters"`
	AuthInfos []NamedAuthInfo `yaml:"users"`
}

type ClusterGenerator struct {
	db        db.ArgoDB
	clientSet *kubernetes.Clientset
	config    *rest.Config
}

func NewClusterGenerator(db db.ArgoDB, clientSet *kubernetes.Clientset, config *rest.Config) Generator {
	return &ClusterGenerator{db, clientSet, config}
}

func (cg *ClusterGenerator) getClusterCredentials(namespace string, releaseSuffix string) ([]byte, []byte, []byte, error) {
	cmd := []string{
		"sh",
		"-c",
		"cat /root/.kube/config",
	}

	var stdout, stderr, stdin bytes.Buffer
	option := &corev1.PodExecOptions{
		Command:   cmd,
		Container: "syncer",
		Stdin:     true,
		Stdout:    true,
		Stderr:    true,
		TTY:       true,
	}

	req := cg.clientSet.CoreV1().RESTClient().Post().Resource("pods").Name(POD_PREFIX + "-" + releaseSuffix + "-0").
		Namespace(namespace).SubResource("exec")

	req.VersionedParams(
		option,
		scheme.ParameterCodec,
	)

	exec, err := remotecommand.NewSPDYExecutor(cg.config, "POST", req.URL())
	if err != nil {
		return nil, nil, nil, err
	}

	err = exec.StreamWithContext(context.Background(), remotecommand.StreamOptions{
		Stdin:  &stdin,
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return nil, nil, nil, err
	}

	var config Config

	err = yaml.Unmarshal(stdout.Bytes(), &config)
	if err != nil {
		return nil, nil, nil, err
	}

	if len(config.Clusters) == 0 {
		return nil, nil, nil, errors.New("clusters empty")
	}

	caData, err := base64.StdEncoding.DecodeString(config.Clusters[0].Cluster.CertificateAuthorityData)
	if err != nil {
		return nil, nil, nil, err
	}

	cert, err := base64.StdEncoding.DecodeString(config.AuthInfos[0].AuthInfo.ClientCertificateData)
	if err != nil {
		return nil, nil, nil, err
	}

	key, err := base64.StdEncoding.DecodeString(config.AuthInfos[0].AuthInfo.ClientKeyData)
	if err != nil {
		return nil, nil, nil, err
	}

	return caData, cert, key, nil
}

// TODO: also should provision service for vcluster pod
func (cg *ClusterGenerator) installVCluster(opts *util.GenerateOpts, namespace string, releaseName string) error {
	cmd, err := helm.NewCmd("/tmp", "v3", "", "")
	if err != nil {
		return err
	}
	log.Print("Execute helm install command")
	_, err = cmd.Freestyle("upgrade", "--install", releaseName, "vcluster", "--values", opts.ClusterOpts.ValuesFilePath, "--repo", "https://charts.loft.sh", "--namespace", namespace, "--repository-config", "", "--create-namespace", "--wait")
	if err != nil {
		return err
	}
	return nil
}

func (cg *ClusterGenerator) getClusterServerURI(namespace string, releaseSuffix string) (string, error) {
	pod, err := cg.clientSet.CoreV1().Pods(namespace).Get(context.TODO(), POD_PREFIX+"-"+releaseSuffix+"-0", metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	// TODO: should be moved to service instead pod
	log.Printf("Get service for https://%s:8443", pod.Status.PodIP)
	return "https://" + pod.Status.PodIP + ":8443", nil
}

func (cg *ClusterGenerator) retrieveClusterURI(namespace, releaseSuffix string) string {
	for i := 0; i < 10; i++ {
		log.Print("Attempting to get cluster uri")
		uri, err := cg.getClusterServerURI(namespace, releaseSuffix)
		if err != nil {
			log.Printf("Failed to get cluster uri due to %s", err.Error())
			time.Sleep(10 * time.Second)
			continue
		}
		return uri
	}
	return ""
}

func (cg *ClusterGenerator) generate(i int, opts *util.GenerateOpts) error {
	log.Printf("Generate cluster #%v of #%v", i, opts.ClusterOpts.Samples)

	namespace := opts.ClusterOpts.NamespacePrefix + "-" + util.GetRandomString()

	log.Printf("Namespace is %s", namespace)

	releaseSuffix := util.GetRandomString()

	log.Printf("Release suffix is %s", namespace)

	err := cg.installVCluster(opts, namespace, POD_PREFIX+"-"+releaseSuffix)
	if err != nil {
		log.Printf("Skip cluster installation due error %v", err.Error())
	}

	log.Print("Get cluster credentials")
	caData, cert, key, err := cg.getClusterCredentials(namespace, releaseSuffix)

	for o := 0; o < 5; o++ {
		if err == nil {
			break
		}
		log.Printf("Failed to get cluster credentials %s, retrying...", releaseSuffix)
		time.Sleep(10 * time.Second)
		caData, cert, key, err = cg.getClusterCredentials(namespace, releaseSuffix)
	}
	if err != nil {
		return err
	}

	log.Print("Get cluster server uri")

	uri := cg.retrieveClusterURI(namespace, releaseSuffix)
	log.Printf("Cluster server uri is %s", uri)

	log.Print("Create cluster")
	_, err = cg.db.CreateCluster(context.TODO(), &argoappv1.Cluster{
		Server: uri,
		Name:   opts.ClusterOpts.ClusterNamePrefix + "-" + util.GetRandomString(),
		Config: argoappv1.ClusterConfig{
			TLSClientConfig: argoappv1.TLSClientConfig{
				Insecure:   false,
				ServerName: "kubernetes.default.svc",
				CAData:     caData,
				CertData:   cert,
				KeyData:    key,
			},
		},
		ConnectionState: argoappv1.ConnectionState{},
		ServerVersion:   "1.18",
		Namespaces:      []string{opts.ClusterOpts.DestinationNamespace},
		Labels:          labels,
	})
	if err != nil {
		return err
	}
	return nil
}

func (cg *ClusterGenerator) Generate(opts *util.GenerateOpts) error {
	log.Printf("Excute in parallel with %v", opts.ClusterOpts.Concurrency)

	wg := util.New(opts.ClusterOpts.Concurrency)
	for l := 1; l <= opts.ClusterOpts.Samples; l++ {
		wg.Add()
		go func(i int) {
			defer wg.Done()
			err := cg.generate(i, opts)
			if err != nil {
				log.Printf("Failed to generate cluster #%v due to : %s", i, err.Error())
			}
		}(l)
	}
	wg.Wait()
	return nil
}

func (cg *ClusterGenerator) Clean(opts *util.GenerateOpts) error {
	log.Printf("Clean clusters")
	namespaces, err := cg.clientSet.CoreV1().Namespaces().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return err
	}

	for _, ns := range namespaces.Items {
		if strings.HasPrefix(ns.Name, POD_PREFIX) {
			log.Printf("Delete namespace %s", ns.Name)
			err = cg.clientSet.CoreV1().Namespaces().Delete(context.TODO(), ns.Name, metav1.DeleteOptions{})
			if err != nil {
				log.Printf("Delete namespace failed due: %s", err.Error())
			}
		}
	}

	secrets := cg.clientSet.CoreV1().Secrets(opts.Namespace)
	return secrets.DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/generated-by=argocd-generator",
	})
}
