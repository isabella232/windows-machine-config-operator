package controllers

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	oconfig "github.com/openshift/api/config/v1"
	mapi "github.com/openshift/machine-api-operator/pkg/apis/machine/v1beta1"
	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh"
	core "k8s.io/api/core/v1"
	k8sapierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubeTypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/openshift/windows-machine-config-operator/pkg/cluster"
	"github.com/openshift/windows-machine-config-operator/pkg/metrics"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig"
	"github.com/openshift/windows-machine-config-operator/pkg/secrets"
	"github.com/openshift/windows-machine-config-operator/pkg/signer"
	"github.com/openshift/windows-machine-config-operator/pkg/windows"
	"github.com/openshift/windows-machine-config-operator/version"
)

const (
	// maxUnhealthyCount is the maximum number of nodes that are not ready to serve at a given time.
	// TODO: https://issues.redhat.com/browse/WINC-524
	maxUnhealthyCount = 1
	// MachineOSLabel is the label used to identify the Windows Machines.
	MachineOSLabel = "machine.openshift.io/os-id"
)

// WindowsMachineReconciler is used to create a controller which manages Windows Machine objects
type WindowsMachineReconciler struct {
	client client.Client
	log    logr.Logger
	scheme *runtime.Scheme
	// k8sclientset holds the kube client that we can re-use for all kube objects other than custom resources.
	k8sclientset *kubernetes.Clientset
	// clusterServiceCIDR holds the cluster network service CIDR
	clusterServiceCIDR string
	// signer is a signer created from the user's private key
	signer ssh.Signer
	// vxlanPort is the custom VXLAN port
	vxlanPort string
	// recorder to generate events
	recorder record.EventRecorder
	// watchNamespace is the namespace the operator is watching as defined by the operator CSV
	watchNamespace string
	// prometheusConfig stores information required to configure Prometheus
	prometheusNodeConfig *metrics.PrometheusNodeConfig
	// platform indicates the cloud on which OpenShift cluster is running
	// TODO: Remove this once we figure out how to be provider agnostic. This is specific to proper usage of userData
	// 		 in vSphere
	//		 https://bugzilla.redhat.com/show_bug.cgi?id=1876987
	platform oconfig.PlatformType
}

// NewWindowsMachineReconciler returns a pointer to a WindowsMachineReconciler
func NewWindowsMachineReconciler(mgr manager.Manager, clusterConfig cluster.Config, watchNamespace string) (*WindowsMachineReconciler, error) {
	// The client provided by the GetClient() method of the manager is a split client that will always hit the API
	// server when writing. When reading, the client will either use a cache populated by the informers backing the
	// controllers, or in certain cases read directly from the API server. It will read from the server both for
	// unstructured types, as well as exceptions specified when initializing the manager. All other times it will read
	// from the cache. Read operations using the default client should only be done against resources that are
	// specifically being watched by controllers in the operator.
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return nil, errors.Wrap(err, "error creating kubernetes clientset")
	}

	serviceCIDR, err := clusterConfig.Network().GetServiceCIDR()
	if err != nil {
		return nil, errors.Wrap(err, "error getting service CIDR")
	}

	// Initialize prometheus configuration
	pc, err := metrics.NewPrometheusNodeConfig(clientset, watchNamespace)
	if err != nil {
		return nil, errors.Wrap(err, "unable to initialize Prometheus configuration")
	}

	return &WindowsMachineReconciler{
		client:               mgr.GetClient(),
		log:                  ctrl.Log.WithName("controller").WithName("windowsmachine"),
		scheme:               mgr.GetScheme(),
		k8sclientset:         clientset,
		clusterServiceCIDR:   serviceCIDR,
		vxlanPort:            clusterConfig.Network().VXLANPort(),
		recorder:             mgr.GetEventRecorderFor("windowsmachine"),
		watchNamespace:       watchNamespace,
		prometheusNodeConfig: pc,
		platform:             clusterConfig.Platform(),
	}, nil
}

// SetupWithManager sets up a new Secret controller
func (r *WindowsMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Watch for the Machine objects with label defined by MachineOSLabel
	machinePredicate := predicate.Funcs{
		// We need the create event to account for Machines that are in provisioned state but were created
		// before WMCO started running
		CreateFunc: func(e event.CreateEvent) bool {
			return r.isValidMachine(e.Object) && isWindowsMachine(e.Object.GetLabels())
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return r.isValidMachine(e.ObjectNew) && isWindowsMachine(e.ObjectNew.GetLabels())
		},
		// ignore delete event for all Machines as WMCO does not react to node getting deleted
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
	}

	nodePredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			if e.Object.GetLabels()[core.LabelOSStable] != "windows" {
				return false
			}
			if e.Object.GetAnnotations()[nodeconfig.VersionAnnotation] != version.Get() {
				return true
			}
			return false
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectNew.GetLabels()[core.LabelOSStable] != "windows" {
				return false
			}
			if e.ObjectNew.GetAnnotations()[nodeconfig.VersionAnnotation] != version.Get() ||
				e.ObjectNew.GetAnnotations()[nodeconfig.PubKeyHashAnnotation] !=
					e.ObjectOld.GetAnnotations()[nodeconfig.PubKeyHashAnnotation] {
				return true
			}
			return false
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&mapi.Machine{}, builder.WithPredicates(machinePredicate)).
		Watches(&source.Kind{Type: &core.Node{}}, handler.EnqueueRequestsFromMapFunc(r.mapNodeToMachine),
			builder.WithPredicates(nodePredicate)).
		Complete(r)
}

// mapNodeToMachine maps the given Windows node to its associated Machine
func (r *WindowsMachineReconciler) mapNodeToMachine(object client.Object) []reconcile.Request {
	node := core.Node{}

	// If for some reason this mapper is called on an object which is not a Node, return
	if kind := object.GetObjectKind().GroupVersionKind(); kind.Kind != node.Kind {
		return nil
	}
	if object.GetLabels()[core.LabelOSStable] != "windows" {
		return nil
	}

	// Map the Node to the associated Machine through the Node's UID
	machines := &mapi.MachineList{}
	err := r.client.List(context.TODO(), machines,
		client.MatchingLabels(map[string]string{MachineOSLabel: "Windows"}))
	if err != nil {
		r.log.Error(err, "could not get a list of machines")
	}
	for _, machine := range machines.Items {
		ok := machine.Status.Phase != nil &&
			len(machine.Status.Addresses) > 0 &&
			machine.Status.NodeRef != nil &&
			machine.Status.NodeRef.UID == object.GetUID()
		if ok {
			return []reconcile.Request{
				{
					NamespacedName: types.NamespacedName{
						Namespace: machine.GetNamespace(),
						Name:      machine.GetName(),
					},
				},
			}
		}
	}

	// Node doesn't match a machine, return
	return nil
}

// isWindowsMachine checks if the machine is a Windows machine or not
func isWindowsMachine(labels map[string]string) bool {
	if value, ok := labels[MachineOSLabel]; ok {
		if value == "Windows" {
			return true
		}
	}
	return false
}

// isValidMachine returns true if the Machine given object is a Machine with a properly populated status
func (r *WindowsMachineReconciler) isValidMachine(obj client.Object) bool {
	machine := &mapi.Machine{}

	// If this function is called on an object that equals nil, return false
	if obj == nil {
		r.log.Error(errors.New("expected machine object to not equal nil"), "invalid Machine", "object", obj)
		return false
	}

	var ok bool
	machine, ok = obj.(*mapi.Machine)
	if !ok {
		r.log.Error(errors.New("unable to typecast object to machine"), "invalid Machine", "object", obj)
		return false
	}
	if machine.Status.Phase == nil {
		r.log.V(1).Info("machine object has no phase associated with it", "name", machine.Name)
		return false
	}
	if len(machine.Status.Addresses) == 0 {
		r.log.V(1).Info("machine object has no address associated with it", "name", machine.Name)
		return false
	}

	return true
}

// Reconcile reads that state of the cluster for a Windows Machine object and makes changes based on the state read
// and what is in the Machine.Spec
// Note: The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *WindowsMachineReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	log := r.log.WithValues("windowsmachine", request.NamespacedName)
	log.V(1).Info("reconciling")

	// Get the private key that will be used to configure the instance
	// Doing this before fetching the machine allows us to warn the user better about the missing private key
	privateKey, err := secrets.GetPrivateKey(kubeTypes.NamespacedName{Namespace: r.watchNamespace,
		Name: secrets.PrivateKeySecret}, r.client)
	if err != nil {
		if k8sapierrors.IsNotFound(err) {
			// Private key was removed, requeue
			return ctrl.Result{}, errors.Wrapf(err, "%s does not exist, please create it", secrets.PrivateKeySecret)
		}
		return ctrl.Result{}, errors.Wrapf(err, "unable to get secret %s", request.NamespacedName)
	}
	// Update the signer with the current privateKey
	r.signer, err = signer.Create(privateKey)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "error creating signer")
	}

	// Fetch the Machine instance
	machine := &mapi.Machine{}
	if err := r.client.Get(ctx, request.NamespacedName, machine); err != nil {
		if k8sapierrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return ctrl.Result{}, err
	}
	// provisionedPhase is the status of the machine when it is in the `Provisioned` state
	provisionedPhase := "Provisioned"
	// runningPhase is the status of the machine when it is in the `Running` state, indicating that it is configured into a node
	runningPhase := "Running"
	if machine.Status.Phase == nil {
		// This condition should never be true as machine objects without a phase will be filtered out via the predicate functions
		return ctrl.Result{}, fmt.Errorf("could not get the phase associated with machine %s", machine.Name)
	} else if *machine.Status.Phase == runningPhase {
		// Machine has been configured into a node, we need to ensure that the version annotation exists. If it doesn't
		// the machine was not fully configured and needs to be configured properly.
		if machine.Status.NodeRef == nil {
			// NodeRef missing. Requeue and hope it is created. It never being created indicates an issue with the
			// machine api operator
			return ctrl.Result{}, fmt.Errorf("ready Windows machine %s missing NodeRef", machine.GetName())
		}

		node := &core.Node{}
		err := r.client.Get(ctx, kubeTypes.NamespacedName{Namespace: machine.Status.NodeRef.Namespace,
			Name: machine.Status.NodeRef.Name}, node)
		if err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "could not get node associated with machine %s", machine.GetName())
		}

		if _, present := node.Annotations[nodeconfig.VersionAnnotation]; present {
			// If either the version annotation doesn't match the current operator version, or the private key used
			// to configure the machine is out of date, the machine should be deleted
			if node.Annotations[nodeconfig.VersionAnnotation] != version.Get() ||
				node.Annotations[nodeconfig.PubKeyHashAnnotation] != nodeconfig.CreatePubKeyHashAnnotation(r.signer.PublicKey()) {
				log.Info("deleting machine")
				deletionAllowed, err := r.isAllowedDeletion(machine)
				if err != nil {
					return ctrl.Result{}, errors.Wrapf(err, "unable to determine if Machine can be deleted")
				}
				if !deletionAllowed {
					log.Info("machine deletion restricted", "maxUnhealthyCount", maxUnhealthyCount)
					r.recorder.Eventf(machine, core.EventTypeWarning, "MachineDeletionRestricted",
						"Machine %v deletion restricted as the maximum unhealthy machines can`t exceed %v count",
						machine.Name, maxUnhealthyCount)
					return ctrl.Result{Requeue: true}, nil
				}
				return ctrl.Result{}, r.deleteMachine(machine)
			}
			log.Info("machine has current version", "version", node.Annotations[nodeconfig.VersionAnnotation])
			// version annotation exists with a valid value, node is fully configured.
			// configure Prometheus when we have already configured Windows Nodes. This is required to update Endpoints object if
			// it gets reverted when the operator pod restarts.
			if err := r.prometheusNodeConfig.Configure(); err != nil {
				return ctrl.Result{}, errors.Wrap(err, "unable to configure Prometheus")
			}
			return ctrl.Result{}, nil
		}
	} else if *machine.Status.Phase != provisionedPhase {
		log.V(1).Info("machine not provisioned", "phase", *machine.Status.Phase)
		// configure Prometheus when a machine is not in `Running` or `Provisioned` phase. This configuration is
		// required to update Endpoints object when Windows machines are being deleted.
		if err := r.prometheusNodeConfig.Configure(); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "unable to configure Prometheus")
		}
		// Machine is not in provisioned or running state, nothing we should do as of now
		return ctrl.Result{}, nil
	}

	// validate userData secret
	if err := r.validateUserData(privateKey); err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "error validating userData secret")
	}

	// Get the IP address associated with the Windows machine, if not error out to requeue again
	if len(machine.Status.Addresses) == 0 {
		return ctrl.Result{}, errors.Errorf("machine %s doesn't have any ip addresses defined",
			machine.Name)
	}
	ipAddress := ""
	for _, address := range machine.Status.Addresses {
		if address.Type == core.NodeInternalIP {
			ipAddress = address.Address
		}
	}
	if len(ipAddress) == 0 {
		return ctrl.Result{}, errors.Errorf("no internal ip address associated with machine %s",
			machine.Name)
	}

	// Get the instance ID associated with the Windows machine.
	providerID := *machine.Spec.ProviderID
	if len(providerID) == 0 {
		return ctrl.Result{}, errors.Errorf("empty provider ID associated with machine %s", machine.Name)
	}
	// Ex: aws:///us-east-1e/i-078285fdadccb2eaa
	// We always want the last entry which is the instanceID, and the first which is the provider name.
	providerTokens := strings.Split(providerID, "/")
	instanceID := providerTokens[len(providerTokens)-1]
	if len(instanceID) == 0 {
		return ctrl.Result{}, errors.Errorf("unable to get instance ID from provider ID for machine %s", machine.Name)
	}

	log.Info("processing")
	// Make the Machine a Windows Worker node
	if err := r.addWorkerNode(ipAddress, instanceID, machine.Name, r.platform); err != nil {
		var authErr *windows.AuthErr
		if errors.As(err, &authErr) {
			// SSH authentication errors with the Machine are non recoverable, stemming from a mismatch with the
			// userdata used to provision the machine and the current private key secret. The machine must be deleted and
			// re-provisioned.
			r.recorder.Eventf(machine, core.EventTypeWarning, "MachineSetupFailure",
				"Machine %s authentication failure", machine.Name)
			return ctrl.Result{}, r.deleteMachine(machine)
		}
		r.recorder.Eventf(machine, core.EventTypeWarning, "MachineSetupFailure",
			"Machine %s configuration failure", machine.Name)
		return ctrl.Result{}, err
	}
	r.recorder.Eventf(machine, core.EventTypeNormal, "MachineSetup",
		"Machine %s configured successfully", machine.Name)
	// configure Prometheus after a Windows machine is configured as a Node.
	if err := r.prometheusNodeConfig.Configure(); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "unable to configure Prometheus")
	}
	return ctrl.Result{}, nil
}

// deleteMachine deletes the specified Machine
func (r *WindowsMachineReconciler) deleteMachine(machine *mapi.Machine) error {
	if !machine.GetDeletionTimestamp().IsZero() {
		// Delete already initiated
		return nil
	}

	if err := r.client.Delete(context.TODO(), machine); err != nil {
		r.recorder.Eventf(machine, core.EventTypeWarning, "MachineDeletionFailed",
			"Machine %v deletion failed: %v", machine.Name, err)
		return err
	}
	r.log.Info("machine has been remediated by deletion", "name", machine.GetName())
	r.recorder.Eventf(machine, core.EventTypeNormal, "MachineDeleted",
		"Machine %v has been remediated by deleting the Machine object", machine.Name)
	return nil
}

// addWorkerNode configures the given Windows VM, adding it as a node object to the cluster
func (r *WindowsMachineReconciler) addWorkerNode(ipAddress, instanceID, machineName string, platform oconfig.PlatformType) error {
	nc, err := nodeconfig.NewNodeConfig(r.k8sclientset, ipAddress, instanceID, machineName, r.clusterServiceCIDR,
		r.vxlanPort, r.signer, platform)
	if err != nil {
		return errors.Wrapf(err, "failed to configure Windows VM %s", instanceID)
	}
	if err := nc.Configure(); err != nil {
		// TODO: Unwrap to extract correct error
		return errors.Wrapf(err, "failed to configure Windows VM %s", instanceID)
	}

	r.log.Info("Windows VM has been configured as a worker node", "ID", nc.ID())
	return nil
}

// validateUserData validates userData secret. It returns error if the secret doesn`t
// contain expected public key bytes.
func (r *WindowsMachineReconciler) validateUserData(privateKey []byte) error {
	userDataSecret := &core.Secret{}
	err := r.client.Get(context.TODO(), kubeTypes.NamespacedName{Name: "windows-user-data", Namespace: "openshift-machine-api"}, userDataSecret)

	if err != nil {
		return errors.Errorf("could not find Windows userData secret in required namespace: %v", err)
	}

	secretData := string(userDataSecret.Data["userData"][:])
	desiredUserDataSecret, err := secrets.GenerateUserData(privateKey)
	if err != nil {
		return err
	}
	if string(desiredUserDataSecret.Data["userData"][:]) != secretData {
		return errors.Errorf("invalid content for userData secret")
	}
	return nil
}

// isAllowedDeletion determines if the number of machines after deletion of the given machine doesn`t fall below the
// minHealthyCount
func (r *WindowsMachineReconciler) isAllowedDeletion(machine *mapi.Machine) (bool, error) {
	if len(machine.OwnerReferences) == 0 {
		return false, errors.New("Machine has no owner reference")
	}
	machinesetName := machine.OwnerReferences[0].Name

	machines := &mapi.MachineList{}
	err := r.client.List(context.TODO(), machines,
		client.MatchingLabels(map[string]string{MachineOSLabel: "Windows"}))
	if err != nil {
		return false, errors.Wrap(err, "cannot list Machines")
	}

	// get Windows MachineSet
	windowsMachineSet := &mapi.MachineSet{}
	err = r.client.Get(context.TODO(), types.NamespacedName{Name: machinesetName,
		Namespace: "openshift-machine-api"}, windowsMachineSet)
	if err != nil {
		return false, errors.Wrap(err, "cannot get MachineSet")
	}

	// Allow deletion if there is only one machine in the Windows MachineSet
	totalWindowsMachineCount := *windowsMachineSet.Spec.Replicas
	if maxUnhealthyCount == totalWindowsMachineCount {
		return true, nil
	}

	totalHealthy := 0
	for _, ma := range machines.Items {
		// Increment the count if the machine is identified as healthy and is a part of given Windows MachineSet and
		// on which deletion is not already initiated.
		if len(machine.OwnerReferences) != 0 && ma.OwnerReferences[0].Name == machinesetName &&
			r.isWindowsMachineHealthy(&ma) && ma.DeletionTimestamp.IsZero() {
			totalHealthy += 1
		}
	}

	unhealthyMachineCount := totalWindowsMachineCount - int32(totalHealthy)
	r.log.Info("unhealthy machine count for machineset", "name", machinesetName, "total", totalWindowsMachineCount,
		"unhealthy", unhealthyMachineCount)

	return unhealthyMachineCount < maxUnhealthyCount, nil
}

// isWindowsMachineHealthy determines if the given Machine object is healthy. A Windows machine is considered
// unhealthy if -
// 1. Machine is not in a 'Running' phase
// 2. Machine is not associated with a Node object
// 3. Associated Node object doesn't have a Version annotation
func (r *WindowsMachineReconciler) isWindowsMachineHealthy(machine *mapi.Machine) bool {
	if (machine.Status.Phase == nil || *machine.Status.Phase != "Running") &&
		machine.Status.NodeRef == nil {
		return false
	}

	// Get node associated with the machine
	node, err := r.k8sclientset.CoreV1().Nodes().Get(context.TODO(), machine.Status.NodeRef.Name, meta.GetOptions{})
	if err != nil {
		return false
	}
	_, present := node.Annotations[nodeconfig.VersionAnnotation]
	if !present {
		return false
	}

	return true
}
