package repositories

import (
	"context"
	"fmt"
	"time"

	"code.cloudfoundry.org/korifi/api/apierrors"
	"code.cloudfoundry.org/korifi/api/authorization"
	workloads "code.cloudfoundry.org/korifi/controllers/apis/workloads/v1alpha1"
	"code.cloudfoundry.org/korifi/controllers/webhooks"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/hierarchical-namespaces/api/v1alpha2"
)

// TODO: confirm that these can be removed
//+kubebuilder:rbac:groups=workloads.cloudfoundry.org,resources=cforgs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=workloads.cloudfoundry.org,resources=cforgs/status,verbs=get

//+kubebuilder:rbac:groups=hnc.x-k8s.io,resources=subnamespaceanchors,verbs=list;create;delete;watch
//+kubebuilder:rbac:groups=hnc.x-k8s.io,resources=hierarchyconfigurations,verbs=get;list;watch;update

//+kubebuilder:rbac:groups="",resources=serviceaccounts;secrets,verbs=get;list;create;delete;watch

//counterfeiter:generate -o fake -fake-name CFSpaceRepository . CFSpaceRepository

type CFSpaceRepository interface {
	CreateSpace(context.Context, authorization.Info, CreateSpaceMessage) (SpaceRecord, error)
	ListSpaces(context.Context, authorization.Info, ListSpacesMessage) ([]SpaceRecord, error)
	GetSpace(context.Context, authorization.Info, string) (SpaceRecord, error)
	DeleteSpace(context.Context, authorization.Info, DeleteSpaceMessage) error
}

const (
	OrgNameLabel          = "cloudfoundry.org/org-name"
	SpaceNameLabel        = "cloudfoundry.org/space-name"
	hierarchyMetadataName = "hierarchy"

	OrgResourceType   = "Org"
	SpaceResourceType = "Space"
	OrgPrefix         = "cf-org-"
	SpacePrefix       = "cf-space-"
)

type CreateOrgMessage struct {
	Name        string
	Suspended   bool
	Labels      map[string]string
	Annotations map[string]string
}

type CreateSpaceMessage struct {
	Name                     string
	OrganizationGUID         string
	ImageRegistryCredentials string
}

type ListOrgsMessage struct {
	Names []string
	GUIDs []string
}

type ListSpacesMessage struct {
	Names             []string
	GUIDs             []string
	OrganizationGUIDs []string
}

type DeleteOrgMessage struct {
	GUID string
}

type DeleteSpaceMessage struct {
	GUID             string
	OrganizationGUID string
}

type OrgRecord struct {
	Name        string
	GUID        string
	Suspended   bool
	Labels      map[string]string
	Annotations map[string]string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type SpaceRecord struct {
	Name             string
	GUID             string
	OrganizationGUID string
	Labels           map[string]string
	Annotations      map[string]string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type OrgRepo struct {
	rootNamespace     string
	privilegedClient  client.WithWatch
	userClientFactory UserK8sClientFactory
	nsPerms           *authorization.NamespacePermissions
	timeout           time.Duration
}

func NewOrgRepo(
	rootNamespace string,
	privilegedClient client.WithWatch,
	userClientFactory UserK8sClientFactory,
	nsPerms *authorization.NamespacePermissions,
	timeout time.Duration,
) *OrgRepo {
	return &OrgRepo{
		rootNamespace:     rootNamespace,
		privilegedClient:  privilegedClient,
		userClientFactory: userClientFactory,
		nsPerms:           nsPerms,
		timeout:           timeout,
	}
}

func (r *OrgRepo) CreateOrg(ctx context.Context, info authorization.Info, org CreateOrgMessage) (OrgRecord, error) {
	userClient, err := r.userClientFactory.BuildClient(info)
	if err != nil {
		return OrgRecord{}, fmt.Errorf("failed to build user client: %w", err)
	}
	var orgCR *workloads.CFOrg
	orgCR, err = r.createOrgCR(ctx, info, userClient, &workloads.CFOrg{
		ObjectMeta: metav1.ObjectMeta{
			Name:      OrgPrefix + uuid.NewString(),
			Namespace: r.rootNamespace,
		},
		Spec: workloads.CFOrgSpec{
			Name: org.Name,
		},
	})

	if err != nil {
		if webhookError, ok := webhooks.WebhookErrorToValidationError(err); ok { // untested
			return OrgRecord{}, apierrors.NewUnprocessableEntityError(err, webhookError.Error())
		}
		return OrgRecord{}, err
	}

	orgRecord := OrgRecord{
		Name:        org.Name,
		GUID:        orgCR.Name,
		Suspended:   org.Suspended,
		Labels:      org.Labels,
		Annotations: org.Annotations,
		CreatedAt:   orgCR.CreationTimestamp.Time,
		UpdatedAt:   orgCR.CreationTimestamp.Time,
	}

	return orgRecord, nil
}

func (r *OrgRepo) createOrgCR(ctx context.Context,
	info authorization.Info,
	userClient client.Client,
	org *workloads.CFOrg,
) (*workloads.CFOrg, error) {
	err := userClient.Create(ctx, org)
	if err != nil {
		return nil, fmt.Errorf("failed to create cf org: %w", apierrors.FromK8sError(err, OrgResourceType))
	}

	timeoutCtx, cancelFn := context.WithTimeout(ctx, r.timeout)
	defer cancelFn()

	watch, err := r.privilegedClient.Watch(timeoutCtx, &workloads.CFOrgList{},
		client.InNamespace(org.Namespace),
		client.MatchingFields{"metadata.name": org.Name},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to set up watch on cf org: %w", apierrors.FromK8sError(err, OrgResourceType))
	}

	conditionReady := false
	var createdOrg *workloads.CFOrg
	for res := range watch.ResultChan() {
		var ok bool
		createdOrg, ok = res.Object.(*workloads.CFOrg)
		if !ok {
			// should never happen, but avoids panic above
			continue
		}
		if meta.IsStatusConditionTrue(createdOrg.Status.Conditions, "Ready") {
			watch.Stop()
			conditionReady = true
			break
		}
	}

	if !conditionReady {
		return nil, fmt.Errorf("cf org did not get Condition `Ready`: 'True' within timeout period %d ms", r.timeout.Milliseconds())
	}

	// wait for the namespace to be created and user to have permissions

	timeoutChan := time.After(r.timeout)

	t1 := time.Now()
outer:
	for {
		select {
		case <-timeoutChan:
			// HNC is broken
			return nil, fmt.Errorf("failed establishing permissions in new namespace after %s: %w", time.Since(t1), err)
		default:
			var authorizedNamespaces map[string]bool
			authorizedNamespaces, err = r.nsPerms.GetAuthorizedOrgNamespaces(ctx, info)
			if err != nil {
				return nil, err
			}

			if _, ok := authorizedNamespaces[org.Name]; ok {
				break outer
			}

			time.Sleep(500 * time.Millisecond)
		}
	}

	return createdOrg, nil
}

// TODO: refactor
func (r *OrgRepo) CreateSpace(ctx context.Context, info authorization.Info, message CreateSpaceMessage) (SpaceRecord, error) {
	_, err := r.GetOrg(ctx, info, message.OrganizationGUID)
	if err != nil {
		return SpaceRecord{}, fmt.Errorf("failed to get parent organization: %w", err)
	}

	spaceGUID := SpacePrefix + uuid.NewString()
	userClient, err := r.userClientFactory.BuildClient(info)
	if err != nil {
		return SpaceRecord{}, fmt.Errorf("failed to build user client: %w", err)
	}

	anchor, err := r.createSubnamespaceAnchor(
		ctx,
		info,
		userClient,
		&v1alpha2.SubnamespaceAnchor{
			ObjectMeta: metav1.ObjectMeta{
				Name:      spaceGUID,
				Namespace: message.OrganizationGUID,
				Labels: map[string]string{
					SpaceNameLabel: message.Name,
				},
			},
		},
		SpaceResourceType,
	)
	if err != nil {
		if webhookError, ok := webhooks.WebhookErrorToValidationError(err); ok {
			return SpaceRecord{}, apierrors.NewUnprocessableEntityError(err, webhookError.Error())
		}
		return SpaceRecord{}, err
	}

	kpackServiceAccount := corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kpack-service-account",
			Namespace: spaceGUID,
		},
		ImagePullSecrets: []corev1.LocalObjectReference{
			{Name: message.ImageRegistryCredentials},
		},
		Secrets: []corev1.ObjectReference{
			{Name: message.ImageRegistryCredentials},
		},
	}
	err = r.privilegedClient.Create(ctx, &kpackServiceAccount)
	if err != nil {
		return SpaceRecord{}, apierrors.FromK8sError(err, SpaceResourceType)
	}

	eiriniServiceAccount := corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "eirini",
			Namespace: spaceGUID,
		},
	}
	err = r.privilegedClient.Create(ctx, &eiriniServiceAccount)
	if err != nil {
		return SpaceRecord{}, apierrors.FromK8sError(err, SpaceResourceType)
	}

	record := SpaceRecord{
		Name:             message.Name,
		GUID:             anchor.Name,
		OrganizationGUID: message.OrganizationGUID,
		CreatedAt:        anchor.CreationTimestamp.Time,
		UpdatedAt:        anchor.CreationTimestamp.Time,
	}

	return record, nil
}

func (r *OrgRepo) createSubnamespaceAnchor(ctx context.Context,
	info authorization.Info,
	userClient client.Client,
	anchor *v1alpha2.SubnamespaceAnchor,
	resourceType string,
) (*v1alpha2.SubnamespaceAnchor, error) {
	err := userClient.Create(ctx, anchor)
	if err != nil {
		return nil, fmt.Errorf("failed to create subnamespaceanchor: %w", apierrors.FromK8sError(err, resourceType))
	}

	timeoutCtx, cancelFn := context.WithTimeout(ctx, r.timeout)
	defer cancelFn()

	watch, err := r.privilegedClient.Watch(timeoutCtx, &v1alpha2.SubnamespaceAnchorList{},
		client.InNamespace(anchor.Namespace),
		client.MatchingFields{"metadata.name": anchor.Name},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to set up watch on subnamespaceanchors: %w", apierrors.FromK8sError(err, resourceType))
	}

	stateOK := false
	var createdAnchor *v1alpha2.SubnamespaceAnchor
	for res := range watch.ResultChan() {
		var ok bool
		createdAnchor, ok = res.Object.(*v1alpha2.SubnamespaceAnchor)
		if !ok {
			// should never happen, but avoids panic above
			continue
		}
		if createdAnchor.Status.State == v1alpha2.Ok {
			watch.Stop()
			stateOK = true
			break
		}
	}

	if !stateOK {
		return nil, fmt.Errorf("subnamespaceanchor did not get state 'ok' within timeout period %d ms", r.timeout.Milliseconds())
	}

	// wait for the namespace to be created and user to have permissions

	timeoutChan := time.After(r.timeout)

	t1 := time.Now()
outer:
	for {
		select {
		case <-timeoutChan:
			// HNC is broken
			return nil, fmt.Errorf("failed establishing permissions in new namespace after %s: %w", time.Since(t1), err)
		default:
			var authorizedNamespaces map[string]bool
			if resourceType == OrgResourceType {
				authorizedNamespaces, err = r.nsPerms.GetAuthorizedOrgNamespaces(ctx, info)
			} else {
				authorizedNamespaces, err = r.nsPerms.GetAuthorizedSpaceNamespaces(ctx, info)
			}
			if err != nil {
				return nil, err
			}
			if _, ok := authorizedNamespaces[anchor.Name]; ok {
				break outer
			}

			time.Sleep(500 * time.Millisecond)
		}
	}

	return createdAnchor, nil
}

func (r *OrgRepo) ListOrgs(ctx context.Context, info authorization.Info, filter ListOrgsMessage) ([]OrgRecord, error) {
	cfOrgList := &workloads.CFOrgList{}
	err := r.privilegedClient.List(ctx, cfOrgList, client.InNamespace(r.rootNamespace))
	if err != nil {
		return nil, apierrors.FromK8sError(err, OrgResourceType)
	}

	var records []OrgRecord
	for _, cfOrg := range cfOrgList.Items {
		if !meta.IsStatusConditionTrue(cfOrg.Status.Conditions, "Ready") { // TODO: use a constant
			continue
		}
		if matchesFilter(cfOrg.Name, filter.GUIDs) && matchesFilter(cfOrg.Spec.Name, filter.Names) {
			records = append(records, OrgRecord{
				Name:      cfOrg.Spec.Name,
				GUID:      cfOrg.Name,
				CreatedAt: cfOrg.CreationTimestamp.Time,
				UpdatedAt: cfOrg.CreationTimestamp.Time, // TODO: use the pattern used elsewhere
			})
		}
	}

	authorizedNamespaces, err := r.nsPerms.GetAuthorizedOrgNamespaces(ctx, info)
	if err != nil {
		return nil, err
	}

	var result []OrgRecord
	for _, org := range records {
		if !authorizedNamespaces[org.GUID] {
			continue
		}

		result = append(result, org)
	}

	return result, nil
}

func (r *OrgRepo) ListSpaces(ctx context.Context, info authorization.Info, message ListSpacesMessage) ([]SpaceRecord, error) {
	subnamespaceAnchorList := &v1alpha2.SubnamespaceAnchorList{}

	err := r.privilegedClient.List(ctx, subnamespaceAnchorList)
	if err != nil {
		return nil, apierrors.FromK8sError(err, SpaceResourceType)
	}

	orgsFilter := toMap(message.OrganizationGUIDs)
	orgUIDs := map[string]struct{}{}
	for _, anchor := range subnamespaceAnchorList.Items {
		if anchor.Namespace != r.rootNamespace {
			continue
		}

		if !matchFilter(orgsFilter, anchor.Name) {
			continue
		}

		orgUIDs[anchor.Name] = struct{}{}
	}

	nameFilter := toMap(message.Names)
	guidFilter := toMap(message.GUIDs)
	records := []SpaceRecord{}
	for _, anchor := range subnamespaceAnchorList.Items {
		if anchor.Status.State != v1alpha2.Ok {
			continue
		}

		spaceName := anchor.Labels[SpaceNameLabel]
		if !matchFilter(nameFilter, spaceName) {
			continue
		}

		if !matchFilter(guidFilter, anchor.Name) {
			continue
		}

		if _, ok := orgUIDs[anchor.Namespace]; !ok {
			continue
		}

		records = append(records, SpaceRecord{
			Name:             spaceName,
			GUID:             anchor.Name,
			OrganizationGUID: anchor.Namespace,
			CreatedAt:        anchor.CreationTimestamp.Time,
			UpdatedAt:        anchor.CreationTimestamp.Time,
		})
	}

	authorizedNamespaces, err := r.nsPerms.GetAuthorizedSpaceNamespaces(ctx, info)
	if err != nil {
		return nil, err
	}

	result := []SpaceRecord{}
	for _, space := range records {
		if !authorizedNamespaces[space.GUID] {
			continue
		}

		result = append(result, space)
	}

	return result, nil
}

func matchFilter(filter map[string]struct{}, value string) bool {
	if len(filter) == 0 {
		return true
	}

	_, ok := filter[value]
	return ok
}

func toMap(elements []string) map[string]struct{} {
	result := map[string]struct{}{}
	for _, element := range elements {
		result[element] = struct{}{}
	}

	return result
}

func (r *OrgRepo) GetSpace(ctx context.Context, info authorization.Info, spaceGUID string) (SpaceRecord, error) {
	spaceRecords, err := r.ListSpaces(ctx, info, ListSpacesMessage{GUIDs: []string{spaceGUID}})
	if err != nil {
		return SpaceRecord{}, err
	}

	if len(spaceRecords) == 0 {
		return SpaceRecord{}, apierrors.NewNotFoundError(fmt.Errorf("space %q not found", spaceGUID), SpaceResourceType)
	}

	return spaceRecords[0], nil
}

func (r *OrgRepo) GetOrg(ctx context.Context, info authorization.Info, orgGUID string) (OrgRecord, error) {
	orgRecords, err := r.ListOrgs(ctx, info, ListOrgsMessage{GUIDs: []string{orgGUID}})
	if err != nil {
		return OrgRecord{}, err
	}

	if len(orgRecords) == 0 {
		return OrgRecord{}, apierrors.NewNotFoundError(nil, OrgResourceType)
	}

	return orgRecords[0], nil
}

func (r *OrgRepo) DeleteOrg(ctx context.Context, info authorization.Info, message DeleteOrgMessage) error {
	var err error
	hierarchyObj := v1alpha2.HierarchyConfiguration{}
	err = r.privilegedClient.Get(ctx, types.NamespacedName{Name: hierarchyMetadataName, Namespace: message.GUID}, &hierarchyObj)
	if err != nil {
		return apierrors.FromK8sError(err, OrgResourceType)
	}

	userClient, err := r.userClientFactory.BuildClient(info)
	if err != nil {
		return fmt.Errorf("failed to build user client: %w", err)
	}

	err = userClient.Delete(ctx, &v1alpha2.SubnamespaceAnchor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      message.GUID,
			Namespace: r.rootNamespace,
		},
	})

	return apierrors.FromK8sError(err, OrgResourceType)
}

func (r *OrgRepo) DeleteSpace(ctx context.Context, info authorization.Info, message DeleteSpaceMessage) error {
	userClient, err := r.userClientFactory.BuildClient(info)
	if err != nil {
		return fmt.Errorf("failed to build user client: %w", err)
	}
	err = userClient.Delete(ctx, &v1alpha2.SubnamespaceAnchor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      message.GUID,
			Namespace: message.OrganizationGUID,
		},
	})
	return apierrors.FromK8sError(err, SpaceResourceType)
}
