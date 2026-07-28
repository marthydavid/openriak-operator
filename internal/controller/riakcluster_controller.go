/*
Copyright 2026 OpenRiak Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
)

const (
	riakClusterFinalizerName = "riak.openriak.io/cluster-finalizer"
	defaultRiakImage         = "ghcr.io/marthydavid/riak:3.2.6"

	// defaultStorageSize backs each node's data volume when spec.storageSize is
	// unset.
	defaultStorageSize = "10Gi"
)

// RiakClusterReconciler reconciles a RiakCluster object
type RiakClusterReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	DefaultImage string // fallback image when spec.image is empty; defaults to defaultRiakImage
}

// +kubebuilder:rbac:groups=riak.openriak.io,resources=riakclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=riak.openriak.io,resources=riakclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=riak.openriak.io,resources=riakclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=riak.openriak.io,resources=riakbuckets,verbs=get;list;watch
// +kubebuilder:rbac:groups=riak.openriak.io,resources=riakusers,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods/exec,verbs=create
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch;create;update;patch;delete

// Reconcile moves the current state of the cluster closer to the desired state.
func (r *RiakClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	cluster := &riakv1.RiakCluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion
	if !cluster.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, cluster)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(cluster, riakClusterFinalizerName) {
		controllerutil.AddFinalizer(cluster, riakClusterFinalizerName)
		if err := r.Update(ctx, cluster); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Initialize status if needed
	if cluster.Status.Phase == "" {
		cluster.Status.Phase = riakv1.PhaseCreating
		if err := r.Status().Update(ctx, cluster); err != nil {
			log.Error(err, "failed to update cluster status")
			return ctrl.Result{}, err
		}
	}

	// Issue TLS certificates via cert-manager when TLS is enabled
	if err := r.reconcileTLSCertificate(ctx, cluster); err != nil {
		log.Error(err, "failed to reconcile TLS certificate")
		cluster.Status.Phase = riakv1.PhaseFailed
		if updateErr := r.Status().Update(ctx, cluster); updateErr != nil {
			log.Error(updateErr, "failed to update cluster status")
		}
		return ctrl.Result{}, err
	}

	// Reconcile the metrics exporter ConfigMap before the StatefulSet so the
	// sidecar's config volume exists when the pod starts.
	if monitoringEnabled(cluster) {
		if err := r.reconcileMonitoringConfigMap(ctx, cluster); err != nil {
			log.Error(err, "failed to reconcile monitoring ConfigMap")
			return ctrl.Result{}, err
		}
	}

	// Create StatefulSet
	if err := r.reconcileStatefulSet(ctx, cluster); err != nil {
		log.Error(err, "failed to reconcile StatefulSet")
		cluster.Status.Phase = riakv1.PhaseFailed
		if updateErr := r.Status().Update(ctx, cluster); updateErr != nil {
			log.Error(updateErr, "failed to update cluster status")
		}
		return ctrl.Result{}, err
	}

	// Create Service
	if err := r.reconcileService(ctx, cluster); err != nil {
		log.Error(err, "failed to reconcile Service")
		return ctrl.Result{}, err
	}

	// Create the ServiceMonitor when monitoring is on. Missing Prometheus
	// Operator CRDs are tolerated (logged and skipped, not an error).
	if monitoringEnabled(cluster) {
		if err := r.reconcileServiceMonitor(ctx, cluster); err != nil {
			log.Error(err, "failed to reconcile ServiceMonitor")
			return ctrl.Result{}, err
		}
	}

	// Update cluster status based on pods
	if err := r.updateClusterStatus(ctx, cluster); err != nil {
		log.Error(err, "failed to update cluster status")
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// reconcileTLSCertificate creates or updates the cert-manager Certificate for the cluster
// when spec.tls.enabled is true. It is a no-op when TLS is disabled.
func (r *RiakClusterReconciler) reconcileTLSCertificate(ctx context.Context, cluster *riakv1.RiakCluster) error {
	if cluster.Spec.TLS == nil || !cluster.Spec.TLS.Enabled {
		return nil
	}
	if cluster.Spec.TLS.CertManager == nil || cluster.Spec.TLS.CertManager.IssuerName == "" {
		return fmt.Errorf("spec.tls.certManager.issuerName must be set when tls.enabled is true")
	}

	cert := buildClusterCertificate(cluster)

	// Set owner reference so the Certificate is garbage-collected with the cluster.
	if err := controllerutil.SetControllerReference(cluster, cert, r.Scheme); err != nil {
		return fmt.Errorf("setting controller reference on TLS certificate: %w", err)
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   certManagerGroup,
		Version: certManagerVersion,
		Kind:    certManagerKind,
	})

	err := r.Get(ctx, client.ObjectKey{Name: cert.GetName(), Namespace: cert.GetNamespace()}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, cert)
	}
	return err
}

func (r *RiakClusterReconciler) reconcileStatefulSet(ctx context.Context, cluster *riakv1.RiakCluster) error {
	log := log.FromContext(ctx)

	image := cluster.Spec.Image
	if image == "" {
		image = r.DefaultImage
	}
	if image == "" {
		image = defaultRiakImage
	}

	pullPolicy := cluster.Spec.ImagePullPolicy
	if pullPolicy == "" {
		pullPolicy = corev1.PullIfNotPresent
	}

	storageSize := resource.MustParse(defaultStorageSize)
	if cluster.Spec.StorageSize != nil {
		storageSize = *cluster.Spec.StorageSize
	}

	// Build environment variables from RiakConfig
	env := []corev1.EnvVar{
		{
			Name: "POD_NAME",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
			},
		},
		{
			Name: "POD_IP",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"},
			},
		},
		{
			Name: "POD_NAMESPACE",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
			},
		},
		{
			Name:  "RIAK_CLUSTER_NAME",
			Value: cluster.Name,
		},
	}

	// Pass riak.conf keys through the entrypoint's RIAK_CONFIG_* scheme: dots
	// become double underscores (the entrypoint reverses this), so any riak.conf
	// key works — storage backends, memory_backend.ttl, multi_backend
	// definitions, etc. Keys are sorted because CreateOrUpdate diffs the pod
	// template every reconcile: map-random env order would roll pods for nothing.
	keys := make([]string, 0, len(cluster.Spec.RiakConfig))
	for key := range cluster.Spec.RiakConfig {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		env = append(env, corev1.EnvVar{
			Name:  "RIAK_CONFIG_" + strings.ToUpper(strings.ReplaceAll(key, ".", "__")),
			Value: cluster.Spec.RiakConfig[key],
		})
	}

	// Inject TLS config via RIAK_CONFIG_* env vars (entrypoint maps these to riak.conf).
	// ssl.certfile / ssl.keyfile / ssl.cacertfile configure Riak's SSL stack.
	// listener.https.internal adds a TLS-enabled HTTPS endpoint on port 8443 alongside
	// the existing plain HTTP listener on 8098 (used for health probes and backwards compat).
	var extraVolumes []corev1.Volume
	var extraVolumeMounts []corev1.VolumeMount
	if cluster.Spec.TLS != nil && cluster.Spec.TLS.Enabled {
		env = append(env,
			corev1.EnvVar{Name: "RIAK_CONFIG_SSL__CERTFILE", Value: riakTLSCertFile},
			corev1.EnvVar{Name: "RIAK_CONFIG_SSL__KEYFILE", Value: riakTLSKeyFile},
			corev1.EnvVar{Name: "RIAK_CONFIG_SSL__CACERTFILE", Value: riakTLSCACertFile},
			corev1.EnvVar{Name: "RIAK_CONFIG_LISTENER__HTTPS__INTERNAL", Value: "0.0.0.0:8443"},
			// cert-manager (and plain openssl) client certs have no CRL distribution
			// point. Riak's default CRL check crashes the protobuf STARTTLS handshake
			// on such certs ({case_clause,{no_crl,...}} in ssl_handshake:certify), so
			// disable it. check_crl is a hidden riak_api key.
			corev1.EnvVar{Name: "RIAK_CONFIG_CHECK_CRL", Value: "off"},
		)
		extraVolumes = []corev1.Volume{
			{
				Name: riakTLSVolumeName,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: clusterTLSSecretName(cluster.Name),
					},
				},
			},
		}
		extraVolumeMounts = []corev1.VolumeMount{
			{
				Name:      riakTLSVolumeName,
				MountPath: riakTLSMountPath,
				ReadOnly:  true,
			},
		}
	}

	resources := &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		},
	}
	if cluster.Spec.Resources != nil {
		resources = cluster.Spec.Resources
	}

	volumeMounts := append([]corev1.VolumeMount{{Name: "data", MountPath: "/var/lib/riak"}}, extraVolumeMounts...)

	containerPorts := []corev1.ContainerPort{
		{Name: "protobuf", ContainerPort: 8087},
		{Name: "http", ContainerPort: 8098},
	}
	if cluster.Spec.TLS != nil && cluster.Spec.TLS.Enabled {
		containerPorts = append(containerPorts, corev1.ContainerPort{Name: riakTLSPortName, ContainerPort: riakTLSPort})
	}

	// Metrics sidecar: a json_exporter container translating Riak's JSON /stats
	// into Prometheus metrics, plus its config ConfigMap volume.
	podVolumes := extraVolumes
	if monitoringEnabled(cluster) {
		podVolumes = append(podVolumes, exporterConfigVolume(cluster))
	}
	// Ephemeral storage backs the data dir with an emptyDir instead of a PVC, for
	// test clusters without a dynamic provisioner. emptyDir is one of the few
	// volume types OpenShift's restricted-v2 SCC permits without extra grants.
	if cluster.Spec.EphemeralStorage {
		podVolumes = append(podVolumes, corev1.Volume{
			Name:         "data",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
	}

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
		},
	}

	// A PersistentVolumeClaim template is used only for durable storage; ephemeral
	// clusters get their emptyDir "data" volume from podVolumes above instead.
	var volumeClaimTemplates []corev1.PersistentVolumeClaim
	if !cluster.Spec.EphemeralStorage {
		volumeClaimTemplates = []corev1.PersistentVolumeClaim{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "data"},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					StorageClassName: &cluster.Spec.StorageClassName,
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: storageSize,
						},
					},
				},
			},
		}
	}

	// StatefulSet.spec.volumeClaimTemplates is immutable, so toggling
	// EphemeralStorage on an existing cluster (durable↔ephemeral) can never take
	// effect: CreateOrUpdate would loop forever on a "Forbidden: updates to
	// statefulset spec ... are forbidden" error. Detect the mismatch up front and
	// fail fast with an actionable message instead. An existing StatefulSet with no
	// "data" PVC template is the ephemeral case.
	existing := &appsv1.StatefulSet{}
	if getErr := r.Get(ctx, client.ObjectKeyFromObject(sts), existing); getErr == nil {
		existingEphemeral := !hasDataVolumeClaim(existing.Spec.VolumeClaimTemplates)
		if existingEphemeral != cluster.Spec.EphemeralStorage {
			return fmt.Errorf(
				"storage mode is immutable: StatefulSet %q was created with ephemeralStorage=%t "+
					"and cannot be switched to %t (StatefulSet volumeClaimTemplates are immutable); "+
					"delete and recreate the cluster to change storage modes",
				cluster.Name, existingEphemeral, cluster.Spec.EphemeralStorage)
		}
	} else if !apierrors.IsNotFound(getErr) {
		return getErr
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		sts.Spec = appsv1.StatefulSetSpec{
			ServiceName: cluster.Name + "-headless",
			Replicas:    &cluster.Spec.Size,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app":     "riak",
					"cluster": cluster.Name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":     "riak",
						"cluster": cluster.Name,
					},
				},
				Spec: corev1.PodSpec{
					NodeSelector: cluster.Spec.NodeSelector,
					Volumes:      podVolumes,
					Containers: append([]corev1.Container{
						{
							Name:            "riak",
							Image:           image,
							ImagePullPolicy: pullPolicy,
							// Keep stdin/tty allocated for backward compatibility with Riak
							// images whose entrypoint runs `riak console` (which attaches an
							// Erlang shell to stdin and exits 0 on EOF, crash-looping the pod).
							// The current image uses `riak start` + tail and does not need
							// these, but they are harmless there.
							Stdin: true,
							TTY:   true,
							Ports: containerPorts,
							Env:   env,
							Resources: corev1.ResourceRequirements{
								Requests: resources.Requests,
								Limits:   resources.Limits,
							},
							VolumeMounts: volumeMounts,
							// Probe the protobuf listener with a TCP check rather than
							// `riak ping` / `riak-admin status`: those each spawn a full
							// temporary Erlang VM per invocation, so running them every few
							// seconds is very CPU/memory heavy (enough to OOM small nodes).
							// A TCP connect to 8087 succeeds once Riak is accepting client
							// connections, which is the signal we actually want.
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									TCPSocket: &corev1.TCPSocketAction{
										Port: intstr.FromString("protobuf"),
									},
								},
								// Generous: 60s warm-up + 6×10s = 60s grace, so a briefly
								// busy node (GC/AAE/security ops under load) is not SIGKILLed.
								InitialDelaySeconds: 60,
								PeriodSeconds:       10,
								TimeoutSeconds:      5,
								FailureThreshold:    6,
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									TCPSocket: &corev1.TCPSocketAction{
										Port: intstr.FromString("protobuf"),
									},
								},
								InitialDelaySeconds: 20,
								PeriodSeconds:       10,
								TimeoutSeconds:      5,
								FailureThreshold:    2,
							},
						},
					}, monitoringSidecars(cluster)...),
					TerminationGracePeriodSeconds: ptr(int64(60)),
					Affinity: &corev1.Affinity{
						PodAntiAffinity: &corev1.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
								{
									LabelSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"cluster": cluster.Name,
										},
									},
									TopologyKey: "kubernetes.io/hostname",
								},
							},
						},
					},
				},
			},
			VolumeClaimTemplates: volumeClaimTemplates,
		}

		return controllerutil.SetControllerReference(cluster, sts, r.Scheme)
	})

	if err != nil {
		log.Error(err, "failed to reconcile StatefulSet")
		return err
	}

	return nil
}

// hasDataVolumeClaim reports whether a StatefulSet's volumeClaimTemplates
// includes the "data" PVC, i.e. it was provisioned with durable storage. Its
// absence identifies an ephemeral (emptyDir-backed) cluster.
func hasDataVolumeClaim(templates []corev1.PersistentVolumeClaim) bool {
	for _, t := range templates {
		if t.Name == "data" {
			return true
		}
	}
	return false
}

func (r *RiakClusterReconciler) reconcileService(ctx context.Context, cluster *riakv1.RiakCluster) error {
	log := log.FromContext(ctx)

	port := int32(8087)
	if cluster.Spec.ServicePort != 0 {
		port = cluster.Spec.ServicePort
	}

	// Extra service ports when TLS is enabled: expose the HTTPS listener on 8443.
	tlsEnabled := cluster.Spec.TLS != nil && cluster.Spec.TLS.Enabled
	basePorts := func() []corev1.ServicePort {
		ports := []corev1.ServicePort{
			{Name: "protobuf", Port: port, TargetPort: intstr.FromString("protobuf")},
			{Name: "http", Port: 8098, TargetPort: intstr.FromString("http")},
		}
		if tlsEnabled {
			ports = append(ports, corev1.ServicePort{
				Name:       riakTLSPortName,
				Port:       riakTLSPort,
				TargetPort: intstr.FromString(riakTLSPortName),
			})
		}
		if monitoringEnabled(cluster) {
			ports = append(ports, corev1.ServicePort{
				Name:       riakMetricsPortName,
				Port:       riakMetricsPort,
				TargetPort: intstr.FromString(riakMetricsPortName),
			})
		}
		return ports
	}

	// Headless service for inter-node Erlang distribution
	headlessSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name + "-headless",
			Namespace: cluster.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, headlessSvc, func() error {
		headlessSvc.Spec = corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			Selector:                 map[string]string{"app": "riak", "cluster": cluster.Name},
			Ports:                    basePorts(),
			PublishNotReadyAddresses: true,
		}
		return controllerutil.SetControllerReference(cluster, headlessSvc, r.Scheme)
	})

	if err != nil {
		log.Error(err, "failed to reconcile headless Service")
		return err
	}

	// Client-facing service
	clientSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
		},
	}

	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, clientSvc, func() error {
		clientSvc.Spec = corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{"app": "riak", "cluster": cluster.Name},
			Ports:    basePorts(),
		}
		return controllerutil.SetControllerReference(cluster, clientSvc, r.Scheme)
	})

	if err != nil {
		log.Error(err, "failed to reconcile client Service")
		return err
	}

	return nil
}

func (r *RiakClusterReconciler) updateClusterStatus(ctx context.Context, cluster *riakv1.RiakCluster) error {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(cluster.Namespace),
		client.MatchingLabels{"cluster": cluster.Name}); err != nil {
		return err
	}

	// Pods come back in arbitrary order; sort by name so members and
	// nodeConditions are stable across reconciles instead of shuffling on every
	// status write.
	sort.Slice(pods.Items, func(i, j int) bool { return pods.Items[i].Name < pods.Items[j].Name })

	storageClassName, storageSize := clusterStorage(cluster)

	// Reuse the previous per-node conditions so LastTransitionTime survives when a
	// node's state has not actually changed.
	previousConditions := make(map[string][]metav1.Condition, len(cluster.Status.NodeConditions))
	for _, nc := range cluster.Status.NodeConditions {
		previousConditions[nc.Name] = nc.Conditions
	}

	readyCount := int32(0)
	members := make([]riakv1.RiakNodeMember, 0, len(pods.Items))
	nodeConditions := make([]riakv1.NodeCondition, 0, len(pods.Items))

	for i := range pods.Items {
		pod := &pods.Items[i]

		ready := podIsReady(pod)
		if ready {
			readyCount++
		}
		health := podHealth(pod, ready)
		storageReady := r.nodeStorageReady(ctx, cluster, pod)

		conds := append([]metav1.Condition(nil), previousConditions[pod.Name]...)
		if ready {
			setCondition(&conds, conditionReady, true, cluster.Generation, "PodReady", "Riak node is ready")
		} else {
			setCondition(&conds, conditionReady, false, cluster.Generation, "PodNotReady",
				fmt.Sprintf("pod %s is in phase %s and not ready", pod.Name, pod.Status.Phase))
		}
		if storageReady {
			setCondition(&conds, conditionStorageReady, true, cluster.Generation, "StorageBound",
				"the node's data volume is available")
		} else {
			setCondition(&conds, conditionStorageReady, false, cluster.Generation, "StorageUnavailable",
				"the node's data volume is not available yet")
		}

		members = append(members, riakv1.RiakNodeMember{
			Name:         pod.Name,
			Pod:          pod.Name,
			Ready:        ready,
			Health:       health,
			Phase:        string(pod.Status.Phase),
			StorageReady: storageReady,
			Conditions:   conds,
		})
		nodeConditions = append(nodeConditions, riakv1.NodeCondition{
			Name:             pod.Name,
			PodName:          pod.Name,
			Ready:            ready,
			Health:           health,
			Phase:            string(pod.Status.Phase),
			StorageReady:     storageReady,
			StorageClassName: storageClassName,
			StorageSize:      storageSize,
			Conditions:       conds,
		})
	}

	buckets, err := r.bucketRefs(ctx, cluster)
	if err != nil {
		return err
	}
	users, err := r.userRefs(ctx, cluster)
	if err != nil {
		return err
	}

	allReady := readyCount == cluster.Spec.Size

	cluster.Status.ReadyNodes = readyCount
	// TotalNodes is the desired size, so readyNodes/totalNodes reads as progress
	// towards the spec rather than towards however many pods currently exist.
	cluster.Status.TotalNodes = cluster.Spec.Size
	cluster.Status.Members = members
	cluster.Status.NodeConditions = nodeConditions
	cluster.Status.StorageClassName = storageClassName
	cluster.Status.StorageSize = storageSize
	cluster.Status.EphemeralStorage = cluster.Spec.EphemeralStorage
	cluster.Status.TLSStatus = r.tlsStatus(ctx, cluster, allReady)
	cluster.Status.MonitoringStatus = r.monitoringStatus(ctx, cluster, pods.Items)
	cluster.Status.Buckets = buckets
	cluster.Status.Users = users

	if allReady {
		cluster.Status.Phase = riakv1.PhaseReady
		setCondition(&cluster.Status.Conditions, conditionReady, true, cluster.Generation,
			"ClusterReady", "Riak cluster is ready")
	} else {
		cluster.Status.Phase = riakv1.PhaseCreating
		setCondition(&cluster.Status.Conditions, conditionReady, false, cluster.Generation,
			"AwaitingPods", fmt.Sprintf("Waiting for %d pods", cluster.Spec.Size-readyCount))
	}

	cluster.Status.LastUpdateTime = &metav1.Time{Time: time.Now()}

	return r.Status().Update(ctx, cluster)
}

// clusterStorage reports the storage class and size backing the cluster's data
// volumes, mirroring what reconcileStatefulSet provisions. Ephemeral clusters use
// an emptyDir, so neither applies and both are reported empty.
func clusterStorage(cluster *riakv1.RiakCluster) (className, size string) {
	if cluster.Spec.EphemeralStorage {
		return "", ""
	}
	size = defaultStorageSize
	if cluster.Spec.StorageSize != nil {
		size = cluster.Spec.StorageSize.String()
	}
	return cluster.Spec.StorageClassName, size
}

// nodeStorageReady reports whether a node's data volume is usable. Ephemeral nodes
// carry an emptyDir that exists as soon as the pod runs; durable nodes depend on
// the StatefulSet's "data" PVC being Bound.
func (r *RiakClusterReconciler) nodeStorageReady(ctx context.Context, cluster *riakv1.RiakCluster, pod *corev1.Pod) bool {
	if cluster.Spec.EphemeralStorage {
		return pod.Status.Phase == corev1.PodRunning
	}

	pvc := &corev1.PersistentVolumeClaim{}
	key := client.ObjectKey{Namespace: pod.Namespace, Name: "data-" + pod.Name}
	if err := r.Get(ctx, key, pvc); err != nil {
		return false
	}
	return pvc.Status.Phase == corev1.ClaimBound
}

// tlsStatus reports the observed state of the cluster's TLS configuration. Riak
// serves inter-node and client TLS from the same certificate, so both become ready
// together: once cert-manager has issued the certificate and every node is running
// with it mounted.
func (r *RiakClusterReconciler) tlsStatus(ctx context.Context, cluster *riakv1.RiakCluster, allNodesReady bool) riakv1.TLSStatus {
	if cluster.Spec.TLS == nil || !cluster.Spec.TLS.Enabled {
		return riakv1.TLSStatus{}
	}

	status := riakv1.TLSStatus{Enabled: true}
	ready, reason := fetchCertificateReadiness(ctx, r.Client, clusterCertName(cluster.Name), cluster.Namespace)
	status.CertManagerReady = ready
	status.CertManagerError = reason
	status.InterNodeReady = ready && allNodesReady
	status.ClientReady = status.InterNodeReady
	return status
}

// monitoringStatus reports the observed state of the metrics exporter sidecar and
// its ServiceMonitor.
func (r *RiakClusterReconciler) monitoringStatus(ctx context.Context, cluster *riakv1.RiakCluster, pods []corev1.Pod) riakv1.MonitoringStatus {
	if !monitoringEnabled(cluster) {
		return riakv1.MonitoringStatus{}
	}

	status := riakv1.MonitoringStatus{Enabled: true}

	// The exporter counts as ready only when every node exposes metrics; a single
	// pod without a working sidecar leaves the cluster's metrics incomplete.
	status.ExporterReady = len(pods) > 0
	for i := range pods {
		ready, reason := containerReady(&pods[i], exporterContainerName)
		if !ready {
			status.ExporterReady = false
			status.ExporterError = fmt.Sprintf("pod %s: %s", pods[i].Name, reason)
			break
		}
	}
	if len(pods) == 0 {
		status.ExporterError = "no Riak pods exist yet"
	}

	sm := &unstructured.Unstructured{}
	sm.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "monitoring.coreos.com", Version: "v1", Kind: "ServiceMonitor",
	})
	key := client.ObjectKey{Name: cluster.Name + "-metrics", Namespace: cluster.Namespace}
	status.ServiceMonitorReady = r.Get(ctx, key, sm) == nil

	return status
}

// bucketRefs lists the RiakBuckets targeting this cluster with their readiness.
//
// The spec.clusterName match is done here rather than through a field index and
// client.MatchingFields: that only works against the manager's cached client, and
// these helpers also run under a plain client.New client (the whole envtest suite,
// and any caller constructing the reconciler directly), where the API server
// rejects the selector with "field label not supported: spec.clusterName" —
// CRDs only support metadata.name/namespace selectors unless the CRD declares
// selectableFields. The list is already served from the cache in production, so
// the filter is an in-memory scan either way.
func (r *RiakClusterReconciler) bucketRefs(ctx context.Context, cluster *riakv1.RiakCluster) ([]riakv1.RiakBucketRef, error) {
	buckets := &riakv1.RiakBucketList{}
	if err := r.List(ctx, buckets, client.InNamespace(cluster.Namespace)); err != nil {
		return nil, err
	}

	refs := []riakv1.RiakBucketRef{}
	for i := range buckets.Items {
		b := &buckets.Items[i]
		if b.Spec.ClusterName != cluster.Name {
			continue
		}
		refs = append(refs, riakv1.RiakBucketRef{
			Name:  b.Name,
			Ready: b.Status.Phase == riakv1.BucketPhaseReady,
		})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs, nil
}

// userRefs lists the RiakUsers targeting this cluster with their readiness. It
// filters in memory for the same reason as bucketRefs.
func (r *RiakClusterReconciler) userRefs(ctx context.Context, cluster *riakv1.RiakCluster) ([]riakv1.RiakUserRef, error) {
	users := &riakv1.RiakUserList{}
	if err := r.List(ctx, users, client.InNamespace(cluster.Namespace)); err != nil {
		return nil, err
	}

	refs := []riakv1.RiakUserRef{}
	for i := range users.Items {
		u := &users.Items[i]
		if u.Spec.ClusterName != cluster.Name {
			continue
		}
		refs = append(refs, riakv1.RiakUserRef{
			Name:  u.Name,
			Ready: u.Status.Phase == riakv1.UserPhaseReady,
		})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs, nil
}

func (r *RiakClusterReconciler) handleDeletion(ctx context.Context, cluster *riakv1.RiakCluster) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(cluster, riakClusterFinalizerName) {
		controllerutil.RemoveFinalizer(cluster, riakClusterFinalizerName)
		if err := r.Update(ctx, cluster); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func ptr[T any](v T) *T {
	return &v
}

// SetupWithManager sets up the controller with the Manager. maxConcurrent sets
// MaxConcurrentReconciles; values < 1 fall back to controller-runtime's default.
func (r *RiakClusterReconciler) SetupWithManager(mgr ctrl.Manager, maxConcurrent int) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&riakv1.RiakCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		WithOptions(controllerOptions(maxConcurrent)).
		Named("riakcluster").
		Complete(r)
}

// controllerOptions builds controller.Options from a max-concurrency value. A
// value < 1 leaves MaxConcurrentReconciles zero, so controller-runtime applies
// its default (1).
func controllerOptions(maxConcurrent int) controller.Options {
	opts := controller.Options{}
	if maxConcurrent > 0 {
		opts.MaxConcurrentReconciles = maxConcurrent
	}
	return opts
}
