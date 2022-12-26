package crd

import (
	"fmt"
	"strings"
	"time"

	"github.com/AliyunContainerService/ack-ram-authenticator/pkg/config"
	ramauthenticatorv1alpha1 "github.com/AliyunContainerService/ack-ram-authenticator/pkg/mapper/crd/apis/ramauthenticator/v1alpha1"
	clientset "github.com/AliyunContainerService/ack-ram-authenticator/pkg/mapper/crd/generated/clientset/versioned"
	informers "github.com/AliyunContainerService/ack-ram-authenticator/pkg/mapper/crd/generated/informers/externalversions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/aws-iam-authenticator/pkg/mapper"
	"sigs.k8s.io/aws-iam-authenticator/pkg/mapper/crd/controller"
)

type CRDMapper struct {
	*controller.Controller
	// ramInformerFactory is an informer factory that must be Started
	ramInformerFactory informers.SharedInformerFactory
	// ramMappingsSynced is a function to get if the informers have synced
	ramMappingsSynced cache.InformerSynced
	// ramMappingsIndex is a custom indexer which allows for indexing on canonical arns
	ramMappingsIndex cache.Indexer
}

var _ mapper.Mapper = &CRDMapper{}

func NewCRDMapper(cfg config.Config) (*CRDMapper, error) {
	var err error
	var k8sconfig *rest.Config
	var kubeClient kubernetes.Interface
	var iamClient clientset.Interface
	var ramInformerFactory informers.SharedInformerFactory

	if cfg.Master != "" || cfg.Kubeconfig != "" {
		k8sconfig, err = clientcmd.BuildConfigFromFlags(cfg.Master, cfg.Kubeconfig)
	} else {
		k8sconfig, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("can't create kubernetes config: %v", err)
	}

	kubeClient, err = kubernetes.NewForConfig(k8sconfig)
	if err != nil {
		return nil, fmt.Errorf("can't create kubernetes client: %v", err)
	}

	iamClient, err = clientset.NewForConfig(k8sconfig)
	if err != nil {
		return nil, fmt.Errorf("can't create authenticator client: %v", err)
	}

	ramInformerFactory = informers.NewSharedInformerFactory(iamClient, time.Second*36000)

	ramMappingInformer := ramInformerFactory.Iamauthenticator().V1alpha1().RAMIdentityMappings()
	ramMappingsSynced := ramMappingInformer.Informer().HasSynced
	ramMappingsIndex := ramMappingInformer.Informer().GetIndexer()

	ctrl := controller.New(kubeClient, iamClient, ramMappingInformer)

	return &CRDMapper{ctrl, ramInformerFactory, ramMappingsSynced, ramMappingsIndex}, nil
}

func NewCRDMapperWithIndexer(ramMappingsIndex cache.Indexer) *CRDMapper {
	return &CRDMapper{ramMappingsIndex: ramMappingsIndex}
}

func (m *CRDMapper) Name() string {
	return mapper.ModeCRD
}

func (m *CRDMapper) Start(stopCh <-chan struct{}) error {
	m.ramInformerFactory.Start(stopCh)
	go func() {
		// Run starts worker goroutines and blocks
		if err := m.Controller.Run(2, stopCh); err != nil {
			panic(err)
		}
	}()

	return nil
}

func (m *CRDMapper) Map(canonicalARN string) (*config.IdentityMapping, error) {
	canonicalARN = strings.ToLower(canonicalARN)

	var iamidentity *ramauthenticatorv1alpha1.RAMIdentityMapping
	var ok bool
	objects, err := m.ramMappingsIndex.ByIndex("canonicalARN", canonicalARN)
	if err != nil {
		return nil, err
	}

	if len(objects) > 0 {
		for _, obj := range objects {
			iamidentity, ok = obj.(*ramauthenticatorv1alpha1.RAMIdentityMapping)
			if ok {
				break
			}
		}

		if iamidentity != nil {
			return &config.IdentityMapping{
				IdentityARN: canonicalARN,
				Username:    iamidentity.Spec.Username,
				Groups:      iamidentity.Spec.Groups,
			}, nil
		}
	}

	return nil, mapper.ErrNotMapped
}

func (m *CRDMapper) IsAccountAllowed(accountID string) bool {
	return false
}