module github.com/kubernetes-sigs/ack-ram-authenticator

go 1.16

require (
	github.com/AliyunContainerService/ack-ram-authenticator v1.0.2
	github.com/aliyun/alibaba-cloud-sdk-go v0.0.0-20190916104532-daf2d24ce8d4
	github.com/aws/aws-sdk-go v1.44.107
	github.com/fsnotify/fsnotify v1.4.9
	github.com/gofrs/flock v0.7.0
	github.com/google/gnostic v0.6.9 // indirect
	github.com/manifoldco/promptui v0.9.0
	github.com/prometheus/client_golang v1.11.0
	github.com/satori/go.uuid v1.2.0
	github.com/sirupsen/logrus v1.8.1
	github.com/spf13/cobra v1.1.3
	github.com/spf13/viper v1.7.0
	golang.org/x/time v0.0.0-20210723032227-1f47c861a9ac
	gopkg.in/yaml.v2 v2.4.0
	k8s.io/api v0.22.1
	k8s.io/apimachinery v0.22.1
	k8s.io/client-go v0.22.1
	k8s.io/code-generator v0.22.1 // indirect
	k8s.io/component-base v0.22.1
	k8s.io/kubernetes v1.22.0 // indirect
	k8s.io/sample-controller v0.22.1 // indirect
	sigs.k8s.io/aws-iam-authenticator v0.6.0
	sigs.k8s.io/yaml v1.2.0
)

replace github.com/googleapis/gnostic => github.com/google/gnostic v0.6.9

replace k8s.io/api => k8s.io/api v0.22.0

replace k8s.io/apiextensions-apiserver => k8s.io/apiextensions-apiserver v0.22.0

replace k8s.io/apimachinery => k8s.io/apimachinery v0.23.0-alpha.0

replace k8s.io/apiserver => k8s.io/apiserver v0.22.0

replace k8s.io/cli-runtime => k8s.io/cli-runtime v0.22.0

replace k8s.io/client-go => k8s.io/client-go v0.22.0

replace k8s.io/cloud-provider => k8s.io/cloud-provider v0.22.0

replace k8s.io/cluster-bootstrap => k8s.io/cluster-bootstrap v0.22.0

replace k8s.io/code-generator => k8s.io/code-generator v0.22.2-rc.0

replace k8s.io/component-base => k8s.io/component-base v0.22.0

replace k8s.io/component-helpers => k8s.io/component-helpers v0.22.0

replace k8s.io/controller-manager => k8s.io/controller-manager v0.22.0

replace k8s.io/cri-api => k8s.io/cri-api v0.23.0-alpha.0

replace k8s.io/csi-translation-lib => k8s.io/csi-translation-lib v0.22.0

replace k8s.io/kube-aggregator => k8s.io/kube-aggregator v0.22.0

replace k8s.io/kube-controller-manager => k8s.io/kube-controller-manager v0.22.0

replace k8s.io/kube-proxy => k8s.io/kube-proxy v0.22.0

replace k8s.io/kube-scheduler => k8s.io/kube-scheduler v0.22.0

replace k8s.io/kubectl => k8s.io/kubectl v0.22.0

replace k8s.io/kubelet => k8s.io/kubelet v0.22.0

replace k8s.io/legacy-cloud-providers => k8s.io/legacy-cloud-providers v0.22.0

replace k8s.io/metrics => k8s.io/metrics v0.22.0

replace k8s.io/mount-utils => k8s.io/mount-utils v0.22.1-rc.0

replace k8s.io/pod-security-admission => k8s.io/pod-security-admission v0.22.0

replace k8s.io/sample-apiserver => k8s.io/sample-apiserver v0.22.0

replace k8s.io/sample-cli-plugin => k8s.io/sample-cli-plugin v0.22.0

replace k8s.io/sample-controller => k8s.io/sample-controller v0.22.0