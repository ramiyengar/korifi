package repositories

import (
	"context"
	"fmt"

	"code.cloudfoundry.org/korifi/api/apierrors"
	"code.cloudfoundry.org/korifi/api/authorization"
	servicesv1alpha1 "code.cloudfoundry.org/korifi/controllers/apis/services/v1alpha1"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	LabelServiceBindingProvisionedService = "servicebinding.io/provisioned-service"
	ServiceBindingResourceType            = "Service Binding"
	ServiceBindingTypeApp                 = "app"
)

type ServiceBindingRepo struct {
	userClientFactory    UserK8sClientFactory
	namespacePermissions *authorization.NamespacePermissions
	namespaceRetriever   NamespaceRetriever
}

func NewServiceBindingRepo(
	namespaceRetriever NamespaceRetriever,
	userClientFactory UserK8sClientFactory,
	namespacePermissions *authorization.NamespacePermissions,
) *ServiceBindingRepo {
	return &ServiceBindingRepo{
		userClientFactory:    userClientFactory,
		namespacePermissions: namespacePermissions,
		namespaceRetriever:   namespaceRetriever,
	}
}

type ServiceBindingRecord struct {
	GUID                string
	Type                string
	Name                *string
	AppGUID             string
	ServiceInstanceGUID string
	SpaceGUID           string
	CreatedAt           string
	UpdatedAt           string
	LastOperation       ServiceBindingLastOperation
}

type ServiceBindingLastOperation struct {
	Type        string
	State       string
	Description *string
	CreatedAt   string
	UpdatedAt   string
}

type CreateServiceBindingMessage struct {
	Name                *string
	ServiceInstanceGUID string
	AppGUID             string
	SpaceGUID           string
}

type DeleteServiceBindingMessage struct {
	GUID string
}

type ListServiceBindingsMessage struct {
	AppGUIDs             []string
	ServiceInstanceGUIDs []string
}

func (m CreateServiceBindingMessage) toCFServiceBinding() servicesv1alpha1.CFServiceBinding {
	guid := uuid.NewString()
	return servicesv1alpha1.CFServiceBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      guid,
			Namespace: m.SpaceGUID,
			Labels:    map[string]string{LabelServiceBindingProvisionedService: "true"},
		},
		Spec: servicesv1alpha1.CFServiceBindingSpec{
			Name: m.Name,
			Service: corev1.ObjectReference{
				Kind:       "CFServiceInstance",
				APIVersion: servicesv1alpha1.GroupVersion.Identifier(),
				Name:       m.ServiceInstanceGUID,
			},
			AppRef: corev1.LocalObjectReference{Name: m.AppGUID},
		},
	}
}

func (r *ServiceBindingRepo) CreateServiceBinding(ctx context.Context, authInfo authorization.Info, message CreateServiceBindingMessage) (ServiceBindingRecord, error) {
	userClient, err := r.userClientFactory.BuildClient(authInfo)
	if err != nil {
		return ServiceBindingRecord{}, fmt.Errorf("failed to build user client: %w", err)
	}

	cfServiceBinding := message.toCFServiceBinding()
	err = userClient.Create(ctx, &cfServiceBinding)
	if err != nil {
		return ServiceBindingRecord{}, apierrors.FromK8sError(err, ServiceBindingResourceType)
	}

	return cfServiceBindingToRecord(cfServiceBinding), err
}

func (r *ServiceBindingRepo) DeleteServiceBinding(ctx context.Context, authInfo authorization.Info, guid string) error {
	userClient, err := r.userClientFactory.BuildClient(authInfo)
	if err != nil {
		return fmt.Errorf("failed to build user client: %w", err)
	}

	namespace, err := r.namespaceRetriever.NamespaceFor(ctx, guid, ServiceBindingResourceType)
	if err != nil {
		return err
	}

	binding := &servicesv1alpha1.CFServiceBinding{}

	err = userClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: guid}, binding)
	if err != nil {
		return apierrors.ForbiddenAsNotFound(apierrors.FromK8sError(err, ServiceBindingResourceType))
	}

	err = userClient.Delete(ctx, binding)
	if err != nil {
		return apierrors.FromK8sError(err, ServiceBindingResourceType)
	}
	return nil
}

func (r *ServiceBindingRepo) ServiceBindingExists(ctx context.Context, authInfo authorization.Info, spaceGUID, appGUID, serviceInstanceGUID string) (bool, error) {
	userClient, err := r.userClientFactory.BuildClient(authInfo)
	if err != nil {
		return false, fmt.Errorf("failed to build user client: %w", err)
	}

	serviceBindingList := new(servicesv1alpha1.CFServiceBindingList)
	err = userClient.List(ctx, serviceBindingList, client.InNamespace(spaceGUID))
	if err != nil {
		return false, apierrors.FromK8sError(err, ServiceBindingResourceType)
	}

	for _, serviceBinding := range serviceBindingList.Items {
		if serviceBinding.Spec.AppRef.Name == appGUID &&
			serviceBinding.Spec.Service.Name == serviceInstanceGUID {
			return true, nil
		}
	}

	return false, nil
}

func cfServiceBindingToRecord(binding servicesv1alpha1.CFServiceBinding) ServiceBindingRecord {
	createdAt := binding.CreationTimestamp.UTC().Format(TimestampFormat)
	updatedAt, _ := getTimeLastUpdatedTimestamp(&binding.ObjectMeta)
	return ServiceBindingRecord{
		GUID:                binding.Name,
		Type:                ServiceBindingTypeApp,
		Name:                binding.Spec.Name,
		AppGUID:             binding.Spec.AppRef.Name,
		ServiceInstanceGUID: binding.Spec.Service.Name,
		SpaceGUID:           binding.Namespace,
		CreatedAt:           createdAt,
		UpdatedAt:           updatedAt,
		LastOperation: ServiceBindingLastOperation{
			Type:        "create",
			State:       "succeeded",
			Description: nil,
			CreatedAt:   createdAt,
			UpdatedAt:   updatedAt,
		},
	}
}

func (r *ServiceBindingRepo) ListServiceBindings(ctx context.Context, authInfo authorization.Info, message ListServiceBindingsMessage) ([]ServiceBindingRecord, error) {
	nsList, err := r.namespacePermissions.GetAuthorizedSpaceNamespaces(ctx, authInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to list namespaces for spaces with user role bindings: %w", err)
	}

	userClient, err := r.userClientFactory.BuildClient(authInfo)
	if err != nil {
		return []ServiceBindingRecord{}, fmt.Errorf("failed to build user client: %w", err)
	}

	var filteredServiceBindings []servicesv1alpha1.CFServiceBinding
	for ns := range nsList {
		serviceInstanceList := new(servicesv1alpha1.CFServiceBindingList)
		err = userClient.List(ctx, serviceInstanceList, client.InNamespace(ns))
		if k8serrors.IsForbidden(err) {
			continue
		}
		if err != nil {
			return []ServiceBindingRecord{}, fmt.Errorf("failed to list service instances in namespace %s: %w",
				ns,
				apierrors.FromK8sError(err, ServiceBindingResourceType),
			)
		}
		filteredServiceBindings = append(filteredServiceBindings, applyServiceBindingListFilter(serviceInstanceList.Items, message)...)
	}

	return toServiceBindingRecords(filteredServiceBindings), nil
}

func applyServiceBindingListFilter(serviceBindingList []servicesv1alpha1.CFServiceBinding, message ListServiceBindingsMessage) []servicesv1alpha1.CFServiceBinding {
	var filtered []servicesv1alpha1.CFServiceBinding
	for _, serviceBinding := range serviceBindingList {
		if matchesFilter(serviceBinding.Spec.Service.Name, message.ServiceInstanceGUIDs) &&
			matchesFilter(serviceBinding.Spec.AppRef.Name, message.AppGUIDs) {
			filtered = append(filtered, serviceBinding)
		}
	}

	return filtered
}

func toServiceBindingRecords(serviceInstanceList []servicesv1alpha1.CFServiceBinding) []ServiceBindingRecord {
	serviceInstanceRecords := make([]ServiceBindingRecord, 0, len(serviceInstanceList))

	for _, serviceInstance := range serviceInstanceList {
		serviceInstanceRecords = append(serviceInstanceRecords, cfServiceBindingToRecord(serviceInstance))
	}
	return serviceInstanceRecords
}
