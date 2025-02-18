package useractivity

import (
	"context"
	"fmt"
	"time"

	ext "github.com/rancher/rancher/pkg/apis/ext.cattle.io/v1"
	v3Legacy "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	v3 "github.com/rancher/rancher/pkg/generated/controllers/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/settings"
	"github.com/rancher/rancher/pkg/wrangler"
	extcore "github.com/rancher/steve/pkg/ext"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
)

const (
	UserActivityNamespace = "cattle-useractivity-data"
	tokenUserId           = "authn.management.cattle.io/token-userId"
	SingularName          = "useractivity"
	PluralName            = "useractivities"
)

// +k8s:openapi-gen=false
// +k8s:deepcopy-gen=false
type Store struct {
	tokenController v3.TokenController
	checker         userHandler
}

var GV = schema.GroupVersion{
	Group:   "ext.cattle.io",
	Version: "v1",
}

var GVK = schema.GroupVersionKind{
	Group:   GV.Group,
	Version: GV.Version,
	Kind:    "UserActivity",
}
var GVR = schema.GroupVersionResource{
	Group:    GV.Group,
	Version:  GV.Version,
	Resource: PluralName,
}

func NewFromWrangler(wranglerCtx *wrangler.Context) *Store {
	return &Store{
		tokenController: wranglerCtx.Mgmt.Token(),
		checker:         &tokenChecker{},
	}
}

// GroupVersionKind implements [rest.GroupVersionKindProvider]
func (t *Store) GroupVersionKind(_ schema.GroupVersion) schema.GroupVersionKind {
	return GVK
}

// NamespaceScoped implements [rest.Scoper]
func (t *Store) NamespaceScoped() bool {
	return false
}

// GetSingularName implements [rest.SingularNameProvider]
func (t *Store) GetSingularName() string {
	return SingularName
}

// New implements [rest.Storage]
func (t *Store) New() runtime.Object {
	obj := &ext.UserActivity{}
	obj.GetObjectKind().SetGroupVersionKind(GVK)
	return obj
}

// Destroy implements [rest.Storage]
func (t *Store) Destroy() {
}

// Create implements [rest.Creator]
func (uas *Store) Create(ctx context.Context,
	obj runtime.Object,
	createValidation rest.ValidateObjectFunc,
	options *metav1.CreateOptions) (runtime.Object, error) {

	// retrieving useractivity object from raw data
	objUserActivity, ok := obj.(*ext.UserActivity)
	if !ok {
		return nil, fmt.Errorf("error casting runtime object to useractivity")
	}
	// retrieve token information
	token, err := uas.tokenController.Get(objUserActivity.Spec.TokenId, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get token %s: %v", objUserActivity.Spec.TokenId, err)
	}
	// set when last activity happened
	lastActivity := metav1.Time{
		Time: time.Now().UTC(),
	}
	// retrieve setting for auth-user-session-idle-ttl-minutes
	idleTimeout := settings.AuthUserSessionIdleTTLMinutes.GetInt()

	return uas.create(ctx, objUserActivity, token, token.UserID, lastActivity, idleTimeout)
}

// create sets the LastActivity and CurrentTimeout fields on the UserActivity object
// provided by the user within the request.
func (uas *Store) create(_ context.Context,
	userActivity *ext.UserActivity,
	token *v3Legacy.Token,
	user string,
	lastActivity metav1.Time,
	authUserSessionIdleTTLMinutes int) (*ext.UserActivity, error) {

	expectedName, err := setUserActivityName(user, token.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to set useractivity name: %v", err)
	}
	// ensure the UserActivity object is crafted as expected.
	if userActivity.Name != expectedName {
		return nil, fmt.Errorf("useractivity name mismatch: have %s - expected %s", userActivity.Name, expectedName)
	}
	// ensure the token specified in the UserActivity is the same
	// we are using to do the request.
	if token.Name != userActivity.Spec.TokenId {
		return nil, fmt.Errorf("token name mismatch: have %s - expected %s", token.Name, userActivity.Spec.TokenId)
	}

	// once validated the request, we can define the lastActivity time.
	newIdleTimeout := metav1.Time{
		Time: lastActivity.Add(time.Minute * time.Duration(authUserSessionIdleTTLMinutes)).UTC(),
	}
	token.LastIdleTimeout = newIdleTimeout
	userActivity.Status.LastActivity = lastActivity.String()
	userActivity.Status.CurrentTimeout = newIdleTimeout.String()
	_, err = uas.tokenController.Update(token)
	if err != nil {
		return nil, fmt.Errorf("failed to update token: %v", err)
	}

	return userActivity, nil
}

// The rest of the methods will be left empty.

// Delete implements [rest.GracefulDeleter]
func (uas *Store) Delete(ctx context.Context,
	name string,
	deleteValidation rest.ValidateObjectFunc,
	options *metav1.DeleteOptions) (runtime.Object, bool, error) {
	return nil, false, fmt.Errorf("unable to delete useractivity")
}

// Get implements [rest.Getter]
func (uas *Store) Get(ctx context.Context,
	name string,
	options *metav1.GetOptions) (runtime.Object, error) {
	return uas.get(ctx, name, options)
}

// get returns the UserActivity based on the token name.
// It is used to know, from the frontend, how much time is
// remained before the idle timeout.
func (uas *Store) get(_ context.Context, uaname string, options *metav1.GetOptions) (runtime.Object, error) {
	user, token, err := getUserActivityName(uaname)
	if err != nil {
		return nil, fmt.Errorf("wrong useractivity name: %v", err)
	}
	// retrieve token information
	tokenId, err := uas.tokenController.Get(token, *options)
	if err != nil {
		return nil, fmt.Errorf("failed to get token %s: %v", token, err)
	}
	// verify user is the same
	if tokenId.UserID != user {
		return nil, fmt.Errorf("user provided mismatches from expected: %s - %s", user, tokenId.UserID)
	}

	// crafting UserActivity from requested Token name.
	ua := &ext.UserActivity{
		ObjectMeta: metav1.ObjectMeta{
			Name: uaname,
		},
		Spec: ext.UserActivitySpec{
			TokenId: tokenId.Name,
		},
		Status: ext.UserActivityStatus{
			CurrentTimeout: tokenId.LastIdleTimeout.String(),
		},
	}

	return ua, nil
}

// NewList implements [rest.Lister]
func (t *Store) NewList() runtime.Object {
	objList := &ext.UserActivityList{}
	objList.GetObjectKind().SetGroupVersionKind(GVK)
	return objList
}

// List implements [rest.Lister]
func (uas *Store) List(ctx context.Context,
	internaloptions *metainternalversion.ListOptions) (runtime.Object, error) {
	return nil, fmt.Errorf("unable to list useractivity")
}

// ConvertToTable implements [rest.Lister]
func (t *Store) ConvertToTable(
	ctx context.Context,
	object runtime.Object,
	tableOptions runtime.Object) (*metav1.Table, error) {

	return extcore.ConvertToTableDefault[*ext.UserActivity](ctx, object, tableOptions,
		GVR.GroupResource())
}

// Update implements [rest.Updater]
func (uas *Store) Update(ctx context.Context,
	name string,
	objInfo rest.UpdatedObjectInfo,
	createValidation rest.ValidateObjectFunc,
	updateValidation rest.ValidateObjectUpdateFunc,
	forceAllowCreate bool,
	options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	return nil, false, fmt.Errorf("unable to update useractivity")
}

// Watch implements [rest.Watcher]
func (uas *Store) Watch(ctx context.Context, internaloptions *metainternalversion.ListOptions) (watch.Interface, error) {
	return nil, fmt.Errorf("unable to watch useractivity")
}

// userHandler is an interface hiding the details of retrieving the user name
// from the store. This makes these operations mockable for store testing.
type userHandler interface {
	UserName(ctx context.Context) (string, error)
}

type tokenChecker struct{}

// UserName hides the details of extracting a user name from the request context
// TODO: move under dedicated package once Andrea's PR is merged, since both PRs implement the same methods.
// (https://github.com/rancher/rancher/pull/47643/files#top)
func (tp *tokenChecker) UserName(ctx context.Context) (string, error) {
	userInfo, ok := request.UserFrom(ctx)
	if !ok {
		return "", fmt.Errorf("context has no user info")
	}

	return userInfo.GetName(), nil
}
