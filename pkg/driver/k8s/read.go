package k8s

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/MRGHOSJ/afon/pkg/driver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Read implements driver.ObjectReader: GET every requested object and
// report its identity, UID, and canonical content hash. Objects not
// found are omitted (absence is data), and an object owned by a
// different flow/node is omitted too — inventory must never record a
// foreign UID. owner therefore scopes the read; it is never an error.
func (d *Driver) Read(ctx context.Context, owner driver.Owner, objs []driver.Object) ([]driver.LiveObject, error) {
	out := make([]driver.LiveObject, 0, len(objs))
	for _, o := range objs {
		mapping, err := d.mappingFor(o)
		if err != nil {
			return nil, err
		}
		if mapping.Scope.Name() != meta.RESTScopeNameNamespace {
			gvk := mapping.GroupVersionKind
			return nil, &driver.ErrClusterScoped{Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind}
		}
		ns := o.Namespace
		if ns == "" {
			ns = d.namespace
		}
		u, err := d.dyn.Resource(mapping.Resource).Namespace(ns).Get(ctx, o.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s/%s: %w", o.Kind, o.Name, err)
		}
		if _, foreign := foreignOwner(u, owner); foreign {
			continue
		}
		canon, err := canonical(u.Object)
		if err != nil {
			return nil, fmt.Errorf("hashing %s/%s: %w", o.Kind, o.Name, err)
		}
		digest := sha256.Sum256(canon)
		out = append(out, driver.LiveObject{
			Object:          identityOf(u),
			UID:             string(u.GetUID()),
			ResourceVersion: u.GetResourceVersion(),
			Hash:            hex.EncodeToString(digest[:]),
		})
	}
	return out, nil
}

// ListOwned implements driver.ObjectLister: live objects carrying the
// owner's flow label (and node label when owner.Node is set) among
// the requested kinds, all in the driver's namespace. A kind the API
// no longer serves is skipped — its objects are gone with it — while
// any other failure surfaces. Flow-wide listings trust the server-side
// selector: an empty owner.Node means "every node of this flow", so a
// node label mismatch is not foreignness here.
func (d *Driver) ListOwned(ctx context.Context, owner driver.Owner, kinds []driver.ObjectKind) ([]driver.Object, error) {
	if owner.Flow == "" {
		return nil, fmt.Errorf("list owned: flow is required")
	}
	selector := labels.Set{flowLabel: owner.Flow}
	if owner.Node != "" {
		selector = labels.Set{flowLabel: owner.Flow, nodeLabel: owner.Node}
	}
	var out []driver.Object
	for _, k := range kinds {
		mapping, err := d.mapper.RESTMapping(schema.GroupKind{Group: k.Group, Kind: k.Kind})
		if err != nil {
			var noKind *meta.NoKindMatchError
			if errors.As(err, &noKind) {
				continue
			}
			return nil, fmt.Errorf("resolving %s/%s: %w", k.Group, k.Kind, err)
		}
		if mapping.Scope.Name() != meta.RESTScopeNameNamespace {
			// Records never contain cluster-scoped objects (Plan
			// refuses them), so nothing owned can live here.
			continue
		}
		list, err := d.dyn.Resource(mapping.Resource).Namespace(d.namespace).
			List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
		if err != nil {
			if apierrors.IsNotFound(err) {
				// The resource itself is gone (removed CRD).
				continue
			}
			return nil, fmt.Errorf("listing %s owned by flow %q: %w", k.Kind, owner.Flow, err)
		}
		for i := range list.Items {
			out = append(out, identityOf(&list.Items[i]))
		}
	}
	return out, nil
}
