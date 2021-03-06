package k8s

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/portworx/torpedo/drivers/node"
	"github.com/portworx/torpedo/drivers/scheduler"
	"github.com/portworx/torpedo/drivers/scheduler/spec"
	"github.com/portworx/torpedo/pkg/k8sutils"
	"github.com/portworx/torpedo/pkg/task"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/pkg/api/v1"
	apps_api "k8s.io/client-go/pkg/apis/apps/v1beta1"
	storage_api "k8s.io/client-go/pkg/apis/storage/v1"
)

// SchedName is the name of the kubernetes scheduler driver implementation
const SchedName = "k8s"

type k8s struct {
	nodes       map[string]node.Node
	specFactory *spec.Factory
}

func (k *k8s) GetNodes() []node.Node {
	var ret []node.Node
	for _, val := range k.nodes {
		ret = append(ret, val)
	}
	return ret
}

func (k *k8s) IsNodeReady(n node.Node) error {
	t := func() (interface{}, error) {
		if err := k8sutils.IsNodeReady(n.Name); err != nil {
			return "", &ErrNodeNotReady{
				Node:  n,
				Cause: err.Error(),
			}
		}

		return "", nil
	}

	if _, err := task.DoRetryWithTimeout(t, 5*time.Minute, 10*time.Second); err != nil {
		logrus.Infof("[debug] node timed out. %#v", n)
		return err
	}

	return nil
}

// String returns the string name of this driver.
func (k *k8s) String() string {
	return SchedName
}

func (k *k8s) Init(specDir string) error {
	nodes, err := k8sutils.GetNodes()
	if err != nil {
		return err
	}

	for _, n := range nodes.Items {
		k.nodes[n.Name] = k.parseK8SNode(n)
	}

	k.specFactory, err = spec.NewFactory(specDir, k)
	if err != nil {
		return err
	}

	return nil
}

func (k *k8s) ParseCoreSpecs(specDir string) ([]interface{}, error) {
	parser := func(in interface{}) (interface{}, error) {
		return coreParser(in)
	}

	return k.parseSpecs(specDir, parser)
}

func (k *k8s) ParseStorageSpecs(specDir string) ([]interface{}, error) {
	parser := func(in interface{}) (interface{}, error) {
		return storageParser(in)
	}

	return k.parseSpecs(specDir, parser)
}

func (k *k8s) parseSpecs(specDir string, parser func(in interface{}) (interface{}, error)) ([]interface{}, error) {
	fileList := []string{}
	if err := filepath.Walk(specDir, func(path string, f os.FileInfo, err error) error {
		if !f.IsDir() {
			fileList = append(fileList, path)
		}

		return nil
	}); err != nil {
		return nil, err
	}

	var coreSpecs []interface{}
	for _, file := range fileList {
		specContents, err := ioutil.ReadFile(file)
		if err != nil {
			return nil, err
		}

		obj, _, err := scheme.Codecs.UniversalDeserializer().Decode([]byte(specContents), nil, nil)
		if err != nil {
			return nil, err
		}

		specObj, err := parser(obj)
		if err != nil {
			return nil, err
		}

		if specObj == nil {
			continue
		}

		coreSpecs = append(coreSpecs, specObj)
	}

	return coreSpecs, nil
}

func coreParser(in interface{}) (interface{}, error) {
	if specObj, ok := in.(*apps_api.Deployment); ok {
		return specObj, nil
	} else if specObj, ok := in.(*apps_api.StatefulSet); ok {
		return specObj, nil
	} else if specObj, ok := in.(*v1.Service); ok {
		return specObj, nil
	}

	return nil, nil
}

func storageParser(in interface{}) (interface{}, error) {
	if specObj, ok := in.(*v1.PersistentVolumeClaim); ok {
		return specObj, nil
	} else if specObj, ok := in.(*storage_api.StorageClass); ok {
		return specObj, nil
	}

	return nil, nil
}

func (k *k8s) getAddressesForNode(n v1.Node) []string {
	var addrs []string
	for _, addr := range n.Status.Addresses {
		if addr.Type == v1.NodeExternalIP || addr.Type == v1.NodeInternalIP {
			addrs = append(addrs, addr.Address)
		}
	}
	return addrs
}

func (k *k8s) parseK8SNode(n v1.Node) node.Node {
	var nodeType node.Type
	if k8sutils.IsNodeMaster(n) {
		nodeType = node.TypeMaster
	} else {
		nodeType = node.TypeWorker
	}

	return node.Node{
		Name:      n.Name,
		Addresses: k.getAddressesForNode(n),
		Type:      nodeType,
	}
}

func getAppNamespaceName(app *spec.AppSpec, instanceID string) string {
	return fmt.Sprintf("%s-%s", app.Key, instanceID)
}

func (k *k8s) Schedule(instanceID string, options scheduler.ScheduleOptions) ([]*scheduler.Context, error) {
	var apps []*spec.AppSpec
	if options.AppKeys != nil && len(options.AppKeys) > 0 {
		for _, key := range options.AppKeys {
			spec, err := k.specFactory.Get(key)
			if err != nil {
				return nil, err
			}
			apps = append(apps, spec)
		}
	} else {
		apps = k.specFactory.GetAll()
	}

	var contexts []*scheduler.Context
	for _, app := range apps {
		appNamespace := getAppNamespaceName(app, instanceID)
		ns, err := k8sutils.CreateNamespace(appNamespace, map[string]string{
			"creater": "torpedo",
			"app":     app.Key,
		})
		if err != nil {
			return nil, &ErrFailedToScheduleApp{
				App:   app,
				Cause: fmt.Sprintf("Failed to create namespace: %v. Err: %v", appNamespace, err),
			}
		}

		var coreObjects []interface{}
		var storageObjects []interface{}

		for _, storage := range app.Storage {
			if obj, ok := storage.(*storage_api.StorageClass); ok {
				obj.Namespace = ns.Name
				sc, err := k8sutils.CreateStorageClass(obj)
				if err != nil {
					if matched, _ := regexp.MatchString(".+ already exists", err.Error()); !matched {
						return nil, &ErrFailedToScheduleApp{
							App:   app,
							Cause: fmt.Sprintf("Failed to create storage class: %v. Err: %v", sc.Name, err),
						}
					}

					sc, err = k8sutils.ValidateStorageClass(obj.Name)
					if err != nil {
						return nil, &ErrFailedToScheduleApp{
							App:   app,
							Cause: fmt.Sprintf("Failed to create storage class: %v. Err: %v", sc.Name, err),
						}
					}
				}
				storageObjects = append(storageObjects, sc)

				logrus.Infof("[%v] Created storage class: %v", app.Key, sc.Name)
			} else if obj, ok := storage.(*v1.PersistentVolumeClaim); ok {
				obj.Namespace = ns.Name
				pvc, err := k8sutils.CreatePersistentVolumeClaim(obj)
				if err != nil {
					return nil, &ErrFailedToScheduleApp{
						App:   app,
						Cause: fmt.Sprintf("Failed to create PVC: %v. Err: %v", pvc.Name, err),
					}
				}
				storageObjects = append(storageObjects, pvc)

				logrus.Infof("[%v] Created PVC: %v", app.Key, pvc.Name)
			} else {
				return nil, &ErrFailedToScheduleApp{
					App:   app,
					Cause: fmt.Sprintf("Failed to create unsupported storage component: %#v.", storage),
				}
			}
		}

		for _, core := range app.Core {
			if obj, ok := core.(*apps_api.Deployment); ok {
				obj.Namespace = ns.Name
				dep, err := k8sutils.CreateDeployment(obj)
				if err != nil {
					return nil, &ErrFailedToScheduleApp{
						App:   app,
						Cause: fmt.Sprintf("Failed to create Deployment: %v. Err: %v", dep.Name, err),
					}
				}
				coreObjects = append(coreObjects, dep)

				logrus.Infof("[%v] Created deployment: %v", app.Key, dep.Name)
			} else if obj, ok := core.(*apps_api.StatefulSet); ok {
				obj.Namespace = ns.Name
				ss, err := k8sutils.CreateStatefulSet(obj)
				if err != nil {
					return nil, &ErrFailedToScheduleApp{
						App:   app,
						Cause: fmt.Sprintf("Failed to create StatefulSet: %v. Err: %v", ss.Name, err),
					}
				}
				coreObjects = append(coreObjects, ss)

				logrus.Infof("[%v] Created StatefulSet: %v", app.Key, ss.Name)
			} else if obj, ok := core.(*v1.Service); ok {
				obj.Namespace = ns.Name
				svc, err := k8sutils.CreateService(obj)
				if err != nil {
					return nil, &ErrFailedToScheduleApp{
						App:   app,
						Cause: fmt.Sprintf("Failed to create Service: %v. Err: %v", svc.Name, err),
					}
				}
				coreObjects = append(coreObjects, svc)

				logrus.Infof("[%v] Created Service: %v", app.Key, svc.Name)
			} else {
				return nil, &ErrFailedToScheduleApp{
					App:   app,
					Cause: fmt.Sprintf("Failed to create unsupported core component: %#v.", core),
				}
			}
		}

		ctx := &scheduler.Context{
			UID: instanceID,
			App: &spec.AppSpec{
				Key:     app.Key,
				Core:    coreObjects,
				Storage: storageObjects,
				Enabled: app.Enabled,
			},
			// Status: TODO
			// Stdout: TODO
			// Stderr: TODO
		}

		contexts = append(contexts, ctx)
	}

	return contexts, nil
}

func (k *k8s) WaitForRunning(ctx *scheduler.Context) error {
	for _, core := range ctx.App.Core {
		if obj, ok := core.(*apps_api.Deployment); ok {
			if err := k8sutils.ValidateDeployment(obj); err != nil {
				return &ErrFailedToValidateApp{
					App:   ctx.App,
					Cause: fmt.Sprintf("Failed to validate Deployment: %v. Err: %v", obj.Name, err),
				}
			}

			logrus.Infof("[%v] Validated deployment: %v", ctx.App.Key, obj.Name)
		} else if obj, ok := core.(*apps_api.StatefulSet); ok {
			if err := k8sutils.ValidateStatefulSet(obj); err != nil {
				return &ErrFailedToValidateApp{
					App:   ctx.App,
					Cause: fmt.Sprintf("Failed to validate StatefulSet: %v. Err: %v", obj.Name, err),
				}
			}

			logrus.Infof("[%v] Validated statefulset: %v", ctx.App.Key, obj.Name)
		} else if obj, ok := core.(*v1.Service); ok {
			svc, err := k8sutils.GetService(obj.Name, obj.Namespace)
			if err != nil {
				return &ErrFailedToValidateApp{
					App:   ctx.App,
					Cause: fmt.Sprintf("Failed to validate Service: %v. Err: %v", obj.Name, err),
				}
			}

			logrus.Infof("[%v] Validated Service: %v", ctx.App.Key, svc.Name)
		} else {
			return &ErrFailedToValidateApp{
				App:   ctx.App,
				Cause: fmt.Sprintf("Failed to validate unsupported core component: %#v.", core),
			}
		}
	}

	return nil
}

func (k *k8s) Destroy(ctx *scheduler.Context) error {
	for _, core := range ctx.App.Core {
		if obj, ok := core.(*apps_api.Deployment); ok {
			if err := k8sutils.DeleteDeployment(obj); err != nil {
				return &ErrFailedToDestroyApp{
					App:   ctx.App,
					Cause: fmt.Sprintf("Failed to destroy Deployment: %v. Err: %v", obj.Name, err),
				}
			}

			logrus.Infof("[%v] Destroyed deployment: %v", ctx.App.Key, obj.Name)
		} else if obj, ok := core.(*apps_api.StatefulSet); ok {
			if err := k8sutils.DeleteStatefulSet(obj); err != nil {
				return &ErrFailedToDestroyApp{
					App:   ctx.App,
					Cause: fmt.Sprintf("Failed to destroy StatefulSet: %v. Err: %v", obj.Name, err),
				}
			}

			logrus.Infof("[%v]Destroyed StatefulSet: %v", ctx.App.Key, obj.Name)
		} else if obj, ok := core.(*v1.Service); ok {
			if err := k8sutils.DeleteService(obj); err != nil {
				return &ErrFailedToDestroyApp{
					App:   ctx.App,
					Cause: fmt.Sprintf("Failed to destroy Service: %v. Err: %v", obj.Name, err),
				}
			}

			logrus.Infof("[%v] Destroyed Service: %v", ctx.App.Key, obj.Name)
		} else {
			return &ErrFailedToDestroyApp{
				App:   ctx.App,
				Cause: fmt.Sprintf("Failed to destroy unsupported core component: %#v.", core),
			}
		}
	}

	appNamespace := getAppNamespaceName(ctx.App, ctx.UID)
	if err := k8sutils.DeleteNamespace(appNamespace); err != nil {
		return &ErrFailedToDestroyApp{
			App:   ctx.App,
			Cause: fmt.Sprintf("Failed to destroy namespace: %#v. Err: %v", appNamespace, err),
		}
	}

	logrus.Infof("[%v] Destroyed Namespace: %v", ctx.App.Key, appNamespace)

	return nil
}

func (k *k8s) WaitForDestroy(ctx *scheduler.Context) error {
	for _, core := range ctx.App.Core {
		if obj, ok := core.(*apps_api.Deployment); ok {
			if err := k8sutils.ValidateTerminatedDeployment(obj); err != nil {
				return &ErrFailedToValidateAppDestroy{
					App:   ctx.App,
					Cause: fmt.Sprintf("Failed to validate destroy of deployment: %v. Err: %v", obj.Name, err),
				}
			}

			logrus.Infof("[%v] Validated destroy of Deployment: %v", ctx.App.Key, obj.Name)
		} else if obj, ok := core.(*apps_api.StatefulSet); ok {
			if err := k8sutils.ValidateTerminatedStatefulSet(obj); err != nil {
				return &ErrFailedToValidateAppDestroy{
					App:   ctx.App,
					Cause: fmt.Sprintf("Failed to validate destroy of statefulset: %v. Err: %v", obj.Name, err),
				}
			}

			logrus.Infof("[%v] Validated destroy of StatefulSet: %v", ctx.App.Key, obj.Name)
		} else if obj, ok := core.(*v1.Service); ok {
			if err := k8sutils.ValidateDeletedService(obj.Name, obj.Namespace); err != nil {
				return &ErrFailedToValidateAppDestroy{
					App:   ctx.App,
					Cause: fmt.Sprintf("Failed to validate destroy of service: %v. Err: %v", obj.Name, err),
				}
			}

			logrus.Infof("[%v] Validated destroy of Service: %v", ctx.App.Key, obj.Name)
		} else {
			return &ErrFailedToValidateAppDestroy{
				App:   ctx.App,
				Cause: fmt.Sprintf("Failed to validate destroy of unsupported core component: %#v.", core),
			}
		}
	}
	return nil
}

func (k *k8s) DeleteTasks(ctx *scheduler.Context) error {
	var pods []v1.Pod
	var err error
	for _, core := range ctx.App.Core {
		if obj, ok := core.(*apps_api.Deployment); ok {
			pods, err = k8sutils.GetDeploymentPods(obj)
			if err != nil {
				return &ErrFailedToDeleteTasks{
					App:   ctx.App,
					Cause: fmt.Sprintf("failed to get pods due to: %v", err),
				}
			}
		} else if obj, ok := core.(*apps_api.StatefulSet); ok {
			pods, err = k8sutils.GetStatefulSetPods(obj)
			if err != nil {
				return &ErrFailedToDeleteTasks{
					App:   ctx.App,
					Cause: fmt.Sprintf("failed to get pods due to: %v", err),
				}
			}
		}
	}

	if err := k8sutils.DeletePods(pods); err != nil {
		return &ErrFailedToDeleteTasks{
			App:   ctx.App,
			Cause: fmt.Sprintf("failed to delete pods due to: %v", err),
		}
	}
	return nil
}

func (k *k8s) GetVolumes(ctx *scheduler.Context) ([]string, error) {
	var volumes []string
	for _, storage := range ctx.App.Storage {
		if obj, ok := storage.(*v1.PersistentVolumeClaim); ok {
			vol, err := k8sutils.GetVolumeForPersistentVolumeClaim(obj)
			if err != nil {
				return nil, &ErrFailedToGetVolumesForApp{
					App:   ctx.App,
					Cause: fmt.Sprintf("Failed to get volume for PVC: %v. Err: %v", obj.Name, err),
				}
			}

			volumes = append(volumes, vol)
		}
	}

	return volumes, nil
}

func (k *k8s) GetVolumeParameters(ctx *scheduler.Context) (map[string]map[string]string, error) {
	result := make(map[string]map[string]string)

	for _, storage := range ctx.App.Storage {
		if obj, ok := storage.(*v1.PersistentVolumeClaim); ok {
			vol, err := k8sutils.GetVolumeForPersistentVolumeClaim(obj)
			if err != nil {
				return nil, &ErrFailedToGetVolumesParameters{
					App:   ctx.App,
					Cause: fmt.Sprintf("failed to get volume for PVC: %v. Err: %v", obj.Name, err),
				}
			}

			params, err := k8sutils.GetPersistentVolumeClaimParams(obj)
			if err != nil {
				return nil, &ErrFailedToGetVolumesParameters{
					App:   ctx.App,
					Cause: fmt.Sprintf("failed to get params for volume: %v. Err: %v", obj.Name, err),
				}
			}
			result[vol] = params
		}
	}

	return result, nil
}

func (k *k8s) InspectVolumes(ctx *scheduler.Context) error {
	for _, storage := range ctx.App.Storage {
		if obj, ok := storage.(*storage_api.StorageClass); ok {
			if _, err := k8sutils.ValidateStorageClass(obj.Name); err != nil {
				return &ErrFailedToValidateStorage{
					App:   ctx.App,
					Cause: fmt.Sprintf("Failed to validate StorageClass: %v. Err: %v", obj.Name, err),
				}
			}

			logrus.Infof("[%v] Validated storage class: %v", ctx.App.Key, obj.Name)
		} else if obj, ok := storage.(*v1.PersistentVolumeClaim); ok {
			if err := k8sutils.ValidatePersistentVolumeClaim(obj); err != nil {
				return &ErrFailedToValidateStorage{
					App:   ctx.App,
					Cause: fmt.Sprintf("Failed to validate PVC: %v. Err: %v", obj.Name, err),
				}
			}

			logrus.Infof("[%v] Validated PVC: %v", ctx.App.Key, obj.Name)
		} else {
			return &ErrFailedToValidateStorage{
				App:   ctx.App,
				Cause: fmt.Sprintf("Failed to validate unsupported storage component: %#v.", storage),
			}
		}
	}

	return nil
}

func (k *k8s) DeleteVolumes(ctx *scheduler.Context) error {
	for _, storage := range ctx.App.Storage {
		if obj, ok := storage.(*storage_api.StorageClass); ok {
			if err := k8sutils.DeleteStorageClass(obj.Name); err != nil {
				return &ErrFailedToDestroyStorage{
					App:   ctx.App,
					Cause: fmt.Sprintf("Failed to destroy storage class: %v. Err: %v", obj.Name, err),
				}
			}
			logrus.Infof("[%v] Destroyed storage class: %v", ctx.App.Key, obj.Name)
		} else if obj, ok := storage.(*v1.PersistentVolumeClaim); ok {
			if err := k8sutils.DeletePersistentVolumeClaim(obj); err != nil {
				if matched, _ := regexp.MatchString(".+ not found", err.Error()); !matched {
					return &ErrFailedToDestroyStorage{
						App:   ctx.App,
						Cause: fmt.Sprintf("Failed to destroy PVC: %v. Err: %v", obj.Name, err),
					}
				}
			}
			logrus.Infof("[%v] Destroyed PVC: %v", ctx.App.Key, obj.Name)
		} else {
			return &ErrFailedToDestroyStorage{
				App:   ctx.App,
				Cause: fmt.Sprintf("Failed to destroy unsupported storage component: %#v.", storage),
			}
		}
	}

	return nil
}

func (k *k8s) GetNodesForApp(ctx *scheduler.Context) ([]node.Node, error) {
	var result []node.Node
	var pods []v1.Pod
	var err error
	for _, core := range ctx.App.Core {
		if obj, ok := core.(*apps_api.Deployment); ok {
			pods, err = k8sutils.GetDeploymentPods(obj)
			if err != nil {
				return nil, &ErrFailedToGetNodesForApp{
					App:   ctx.App,
					Cause: fmt.Sprintf("failed to get pods due to: %v", err),
				}
			}
		} else if obj, ok := core.(*apps_api.StatefulSet); ok {
			pods, err = k8sutils.GetStatefulSetPods(obj)
			if err != nil {
				return nil, &ErrFailedToGetNodesForApp{
					App:   ctx.App,
					Cause: fmt.Sprintf("Failed to get pods due to: %v", err),
				}
			}
		}
	}

	//We should have pods from a supported application at this point
	for _, p := range pods {
		if len(p.Spec.NodeName) > 0 {
			n, ok := k.nodes[p.Spec.NodeName]
			if !ok {
				return nil, &ErrFailedToGetNodesForApp{
					App:   ctx.App,
					Cause: fmt.Sprintf("node: %v not present in k8s map", p.Spec.NodeName),
				}
			}

			if contains(result, n) {
				continue
			}

			result = append(result, n)
		}
	}

	return result, nil
}

func contains(nodes []node.Node, n node.Node) bool {
	for _, value := range nodes {
		if value.Name == n.Name {
			return true
		}
	}
	return false
}

func init() {
	k := &k8s{
		nodes: make(map[string]node.Node),
	}
	scheduler.Register(SchedName, k)
}
