package probe

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// ProbeTimeout bounds each individual probe so one slow section cannot hang
// the rest.
const ProbeTimeout = 15 * time.Second

// Input carries everything the probes need. Only the client-facing pieces live
// here; kubeconfig loading stays in cmd/.
type Input struct {
	Client      kubernetes.Interface
	Dynamic     dynamic.Interface
	RESTConfig  *rest.Config
	KubeContext string
	ClusterName string
	Namespace   string
	Full        bool             // include the full API resource list in discovery
	Now         func() time.Time // clock injection for deterministic output
}

// Probe collects every section in parallel and assembles a ClusterProfile.
// One denied or slow section never cancels the others: probes record errors
// instead of returning them, and each runs under its own timeout.
// The only hard failure is the cluster being unreachable (version probe
// failed), which is returned as an error instead of a profile.
func Probe(ctx context.Context, in Input) (ClusterProfile, error) {
	now := in.Now
	if now == nil {
		now = time.Now
	}
	prof := ClusterProfile{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   now().UTC(),
		Sections:      map[string]SectionStatus{},
	}

	p := &prober{in: in}
	start := time.Now()
	var mu sync.Mutex
	var g errgroup.Group // plain Group: goroutines return nil, errors are recorded
	run := func(section string, fn func(context.Context) (any, []ProbeError)) {
		g.Go(func() error {
			pctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
			defer cancel()
			t0 := time.Now()
			data, errs := fn(pctx)
			mu.Lock()
			defer mu.Unlock()
			prof.Errors = append(prof.Errors, errs...)
			st := classify(data, errs)
			st.DurationMs = time.Since(t0).Milliseconds()
			prof.Sections[section] = st
			assign(&prof, section, data)
			return nil
		})
	}

	run("identity", func(ctx context.Context) (any, []ProbeError) { return p.probeIdentity(ctx) })
	run("version", func(ctx context.Context) (any, []ProbeError) { return p.probeVersion(ctx) })
	run("discovery", func(ctx context.Context) (any, []ProbeError) { return p.probeDiscovery(ctx) })
	run("health", func(ctx context.Context) (any, []ProbeError) { return p.probeHealth(ctx) })
	run("nodes", func(ctx context.Context) (any, []ProbeError) { return p.probeNodes(ctx) })
	run("access", func(ctx context.Context) (any, []ProbeError) { return p.probeAccess(ctx) })
	run("namespace", func(ctx context.Context) (any, []ProbeError) { return p.probeNamespace(ctx) })
	run("networking", func(ctx context.Context) (any, []ProbeError) { return p.probeNetworking(ctx) })
	run("storage", func(ctx context.Context) (any, []ProbeError) { return p.probeStorage(ctx) })
	g.Wait()

	sortErrors(prof.Errors)
	prof.Identity.Flavor = detectFlavor(prof.Version, prof.Nodes.Nodes)
	prof.DurationMs = time.Since(start).Milliseconds()

	// The cluster is unreachable only if the version probe failed; anything
	// less is reported as a (partial) profile. The original error is kept so
	// the message can distinguish refused connections from auth failures.
	if p.versionErr != nil {
		return ClusterProfile{}, unreachableError(in, p.versionErr)
	}
	return prof, nil
}

type prober struct {
	in Input
	// versionErr holds the original version-probe failure for classification
	// after the probes finish. Only the version probe writes it, and g.Wait
	// in Probe establishes the happens-before edge before it is read.
	versionErr error
}

// assign writes a probe's result into its profile field.
func assign(prof *ClusterProfile, section string, data any) {
	switch section {
	case "identity":
		prof.Identity = data.(Identity)
	case "version":
		prof.Version = data.(VersionInfo)
	case "discovery":
		prof.Discovery = data.(DiscoveryInfo)
	case "health":
		prof.Health = data.(HealthInfo)
	case "nodes":
		prof.Nodes = data.(NodesSection)
	case "access":
		prof.Access = data.(AccessInfo)
	case "namespace":
		prof.Namespace = data.(NamespaceInfo)
	case "networking":
		prof.Networking = data.(NetworkingInfo)
	case "storage":
		prof.Storage = data.(StorageInfo)
	}
}
