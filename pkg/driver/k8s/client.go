package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MRGHOSJ/afon/pkg/driver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
)

const (
	// flowLabel and nodeLabel stamp ownership on every applied object.
	// Plan refuses objects whose labels name another owner; prune lists
	// by both labels and re-verifies them client-side before deleting.
	flowLabel = "afon.dev/flow"
	nodeLabel = "afon.dev/node"
	// fieldManager is the SSA field manager for every dry-run and
	// apply. Conflicts are therefore always afon-vs-someone-else, and
	// re-applies by the same manager never conflict with themselves.
	fieldManager = "afon"
)

// nsGVR is how the driver checks its target namespace exists. The
// spike and the tests share it.
var nsGVR = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}

// RESTMapper resolves a driver.Object's group/kind to the API's
// resource and scope. It is the narrow slice of meta.RESTMapper the
// driver needs: production passes the discovery-backed implementation,
// tests a static one.
type RESTMapper interface {
	RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error)
}

// Driver implements driver.Driver against one namespace. It holds no
// mutable state: the engine runs resource nodes in parallel and every
// call goes straight through the thread-safe dynamic client.
type Driver struct {
	dyn       dynamic.Interface
	mapper    RESTMapper
	namespace string
	force     bool
	prune     bool
}

// New binds a driver to a namespace. force mirrors --force-conflicts:
// when true, applies (and plan's dry-run classification) take field
// ownership instead of failing with driver.ErrConflict; it never
// overrides ownership refusal, which is Plan's job. prune mirrors
// --no-prune: when false, Plan skips the owner-scoped prune LIST, so
// plan and apply agree that nothing will be deleted. namespace must
// be non-empty — the driver manages exactly one, and cluster-scoped
// objects are refused with driver.ErrClusterScoped.
func New(dyn dynamic.Interface, mapper RESTMapper, namespace string, force, prune bool) (*Driver, error) {
	if dyn == nil || mapper == nil {
		return nil, fmt.Errorf("k8s driver: dynamic client and REST mapper are required")
	}
	if namespace == "" {
		return nil, fmt.Errorf("k8s driver: namespace is required")
	}
	return &Driver{dyn: dyn, mapper: mapper, namespace: namespace, force: force, prune: prune}, nil
}

// NewFromConfig builds discovery, a REST mapper, and a dynamic client
// from a rest config (the CLI wires this to its --context handling).
// The mapper snapshots the server's API groups at this moment.
func NewFromConfig(cfg *rest.Config, namespace string, force, prune bool) (*Driver, error) {
	if cfg == nil {
		return nil, fmt.Errorf("k8s driver: rest config is required")
	}
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("k8s driver: discovery client: %w", err)
	}
	groupResources, err := restmapper.GetAPIGroupResources(disco)
	if err != nil {
		return nil, fmt.Errorf("k8s driver: reading API groups: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("k8s driver: dynamic client: %w", err)
	}
	return New(dyn, restmapper.NewDiscoveryRESTMapper(groupResources), namespace, force, prune)
}

// mappingFor resolves the object's group/kind (and version, when set;
// empty picks the server's preferred version) to its API resource.
func (d *Driver) mappingFor(o driver.Object) (*meta.RESTMapping, error) {
	if o.Kind == "" {
		return nil, fmt.Errorf("object %q has no kind", o.Name)
	}
	gk := schema.GroupKind{Group: o.Group, Kind: o.Kind}
	var versions []string
	if o.Version != "" {
		versions = []string{o.Version}
	}
	mapping, err := d.mapper.RESTMapping(gk, versions...)
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", gk.String(), err)
	}
	return mapping, nil
}

// prepare resolves the object's API resource, refuses cluster-scoped
// kinds, and builds the exact body SSA will send: identity enforced
// from the Object fields, namespace defaulted to the driver's, owner
// labels stamped. Plan and Apply share it so they can never disagree
// about what an object is.
func (d *Driver) prepare(owner driver.Owner, o driver.Object) (schema.GroupVersionResource, map[string]any, error) {
	mapping, err := d.mappingFor(o)
	if err != nil {
		return schema.GroupVersionResource{}, nil, err
	}
	if mapping.Scope.Name() != meta.RESTScopeNameNamespace {
		gvk := mapping.GroupVersionKind
		return schema.GroupVersionResource{}, nil, &driver.ErrClusterScoped{
			Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind,
		}
	}
	body, err := d.applyBody(owner, o)
	if err != nil {
		return schema.GroupVersionResource{}, nil, err
	}
	return mapping.Resource, body, nil
}

// checkNamespace fails fast, with a human message, when the target
// namespace is missing: manifests are rendered with the namespace
// injected, so a missing namespace means nobody created it yet.
func (d *Driver) checkNamespace(ctx context.Context) error {
	_, err := d.dyn.Resource(nsGVR).Get(ctx, d.namespace, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return fmt.Errorf("target namespace %q does not exist; create it before applying", d.namespace)
	case err != nil:
		return fmt.Errorf("checking namespace %q: %w", d.namespace, err)
	}
	return nil
}

// applyBody builds the SSA payload from a desired object. The Object's
// identity fields are authoritative: when Data carries the same fields
// they must agree (a contradiction is an error, never a silent
// overwrite), and when Data omits them the driver fills them in.
// The namespace defaults to the driver's and must not disagree, and
// the ownership labels are always stamped last.
func (d *Driver) applyBody(owner driver.Owner, o driver.Object) (map[string]any, error) {
	id := o.Kind + "/" + o.Name
	if o.Name == "" {
		return nil, fmt.Errorf("object of kind %q has no name", o.Kind)
	}
	ns := o.Namespace
	if ns == "" {
		ns = d.namespace
	}
	if ns != d.namespace {
		return nil, fmt.Errorf("%s targets namespace %q but this driver manages %q", id, ns, d.namespace)
	}

	body := map[string]any{}
	if o.Data != nil {
		// JSON round-trip: a deep copy the driver can mutate without
		// touching the caller's map, and a serializability check the
		// server would make anyway.
		b, err := json.Marshal(o.Data)
		if err != nil {
			return nil, fmt.Errorf("%s: encoding data: %w", id, err)
		}
		if err := json.Unmarshal(b, &body); err != nil {
			return nil, fmt.Errorf("%s: decoding data: %w", id, err)
		}
	}

	apiVersion := o.Version
	if o.Group != "" {
		apiVersion = o.Group + "/" + o.Version
	}
	if err := checkStr(body, "apiVersion", apiVersion, id); err != nil {
		return nil, err
	}
	if err := checkStr(body, "kind", o.Kind, id); err != nil {
		return nil, err
	}

	var meta map[string]any
	switch v := body["metadata"].(type) {
	case map[string]any:
		meta = v
	case nil:
		meta = map[string]any{}
	default:
		return nil, fmt.Errorf("%s: data metadata must be a mapping, got %T", id, v)
	}
	if err := checkStr(meta, "name", o.Name, id); err != nil {
		return nil, err
	}
	if err := checkStr(meta, "namespace", ns, id); err != nil {
		return nil, err
	}

	var labels map[string]any
	switch v := meta["labels"].(type) {
	case map[string]any:
		labels = v
	case nil:
		labels = map[string]any{}
	default:
		return nil, fmt.Errorf("%s: data metadata.labels must be a mapping, got %T", id, v)
	}
	labels[flowLabel] = owner.Flow
	labels[nodeLabel] = owner.Node

	meta["name"] = o.Name
	meta["namespace"] = ns
	meta["labels"] = labels
	body["apiVersion"] = apiVersion
	body["kind"] = o.Kind
	body["metadata"] = meta
	return body, nil
}

// checkStr enforces that a Data field, when present and non-null,
// agrees with the identity the Object carries. Empty strings are
// treated as absent (an explicit null or "" in YAML means unset).
func checkStr(m map[string]any, key, want, id string) error {
	v, present := m[key]
	if !present || v == nil {
		return nil
	}
	s, ok := v.(string)
	if !ok || (s != "" && s != want) {
		return fmt.Errorf("%s: data %s %v contradicts %q", id, key, v, want)
	}
	return nil
}

// ssa is the driver's write primitive: ssaPatch with the driver's
// field manager and force setting.
func (d *Driver) ssa(ctx context.Context, gvr schema.GroupVersionResource, body map[string]any, dryRun bool) (*unstructured.Unstructured, error) {
	return ssaPatch(ctx, d.dyn, gvr, body, fieldManager, d.force, dryRun)
}

// ssaPatch is server-side apply through the dynamic client, scoped to
// the body's metadata.namespace (the same way the driver resolves it).
// Force is a *bool in PatchOptions — a local, so no dependency churn —
// and DryRun=All runs the full apply pipeline (admission, defaulting,
// conflict checks) without persisting. The Stage 2a spike validated
// this exact call against a live API server.
func ssaPatch(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource,
	body map[string]any, manager string, force, dryRun bool) (*unstructured.Unstructured, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshaling desired: %w", err)
	}
	ns, err := objNamespace(body)
	if err != nil {
		return nil, err
	}
	forceVar := force
	opts := metav1.PatchOptions{FieldManager: manager, Force: &forceVar}
	if dryRun {
		opts.DryRun = []string{"All"}
	}
	return dyn.Resource(gvr).Namespace(ns).Patch(ctx, objName(body), types.ApplyPatchType, data, opts)
}

func objName(obj map[string]any) string {
	meta, _ := obj["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	return name
}

func objNamespace(obj map[string]any) (string, error) {
	meta, _ := obj["metadata"].(map[string]any)
	ns, _ := meta["namespace"].(string)
	if ns == "" {
		return "", fmt.Errorf("object %q missing metadata.namespace", objName(obj))
	}
	return ns, nil
}

// foreignOwner reports whether the live object is claimed by a
// different flow or node. Unlabeled objects belong to no one — the
// next apply adopts them — so only a present label that disagrees
// counts as foreign.
func foreignOwner(live *unstructured.Unstructured, owner driver.Owner) (driver.Owner, bool) {
	labels := live.GetLabels()
	flow, hasFlow := labels[flowLabel]
	node, hasNode := labels[nodeLabel]
	if (hasFlow && flow != owner.Flow) || (hasNode && node != owner.Node) {
		return driver.Owner{Flow: flow, Node: node}, true
	}
	return driver.Owner{}, false
}

// ownedBy reports whether the live object carries exactly this
// owner's labels. Prune deletes only what passes this check, so an
// unlabeled or partially-labeled object is never touched even if a
// selector (or a buggy server) let it through.
func ownedBy(live *unstructured.Unstructured, owner driver.Owner) bool {
	labels := live.GetLabels()
	flow, hasFlow := labels[flowLabel]
	node, hasNode := labels[nodeLabel]
	return hasFlow && hasNode && flow == owner.Flow && node == owner.Node
}

// identityOf lifts a live object into the identity half of
// driver.Object. Data stays nil: a prune diff only needs to name what
// to delete.
func identityOf(u *unstructured.Unstructured) driver.Object {
	group, version := "", u.GetAPIVersion()
	if i := strings.Index(version, "/"); i >= 0 {
		group, version = version[:i], version[i+1:]
	}
	return driver.Object{
		Group: group, Version: version, Kind: u.GetKind(),
		Namespace: u.GetNamespace(), Name: u.GetName(),
	}
}
