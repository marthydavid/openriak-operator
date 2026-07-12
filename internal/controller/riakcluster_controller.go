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
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
)

const (
	riakClusterFinalizerName = "riak.openriak.io/cluster-finalizer"
	defaultRiakImage         = "ghcr.io/marthydavid/riak:3.2.6"
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
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods/exec,verbs=create
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete

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

	storageSize := resource.MustParse("10Gi")
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

	// Add custom Riak config as environment variables
	for key, value := range cluster.Spec.RiakConfig {
		env = append(env, corev1.EnvVar{
			Name:  fmt.Sprintf("RIAK_%s", strings.ToUpper(strings.ReplaceAll(key, ".", "_"))),
			Value: value,
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
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
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

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
		},
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
					Volumes:      extraVolumes,
					Containers: []corev1.Container{
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
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{
										// `ping` is a subcommand of `riak`, not `riak-admin`
										// (`riak-admin ping` exits non-zero with a usage error,
										// which fails the probe and crash-loops the pod).
										Command: []string{"riak", "ping"},
									},
								},
								InitialDelaySeconds: 30,
								PeriodSeconds:       10,
								TimeoutSeconds:      5,
								FailureThreshold:    3,
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"riak-admin", "status"},
									},
								},
								InitialDelaySeconds: 20,
								PeriodSeconds:       10,
								TimeoutSeconds:      5,
								FailureThreshold:    2,
							},
						},
					},
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
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
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
			},
		}

		return controllerutil.SetControllerReference(cluster, sts, r.Scheme)
	})

	if err != nil {
		log.Error(err, "failed to reconcile StatefulSet")
		return err
	}

	return nil
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

	readyCount := int32(0)
	members := []riakv1.RiakNodeMember{}

	for _, pod := range pods.Items {
		member := riakv1.RiakNodeMember{
			Pod:  pod.Name,
			Name: pod.Name,
		}

		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				readyCount++
				member.Ready = true
				break
			}
		}

		members = append(members, member)
	}

	cluster.Status.ReadyNodes = readyCount
	cluster.Status.Members = members

	if readyCount == cluster.Spec.Size {
		cluster.Status.Phase = riakv1.PhaseReady
		meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cluster.Generation,
			Reason:             "ClusterReady",
			Message:            "Riak cluster is ready",
		})
	} else {
		cluster.Status.Phase = riakv1.PhaseCreating
		meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cluster.Generation,
			Reason:             "AwaitingPods",
			Message:            fmt.Sprintf("Waiting for %d pods", cluster.Spec.Size-readyCount),
		})
	}

	cluster.Status.LastUpdateTime = &metav1.Time{Time: time.Now()}

	return r.Status().Update(ctx, cluster)
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

// SetupWithManager sets up the controller with the Manager.
func (r *RiakClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&riakv1.RiakCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Named("riakcluster").
		Complete(r)
}
