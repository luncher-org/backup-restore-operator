package restore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"k8s.io/apimachinery/pkg/api/meta"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	v1 "github.com/mrajashree/backup/pkg/apis/backupper.cattle.io/v1"
	util "github.com/mrajashree/backup/pkg/controllers"
	backupControllers "github.com/mrajashree/backup/pkg/generated/controllers/backupper.cattle.io/v1"
	lasso "github.com/rancher/lasso/pkg/client"
	"github.com/sirupsen/logrus"

	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	//"k8s.io/apimachinery/pkg/api/meta"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	k8sv1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/storage/value"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
)

const (
	metadataMapKey  = "metadata"
	ownerRefsMapKey = "ownerReferences"
)

type handler struct {
	ctx                     context.Context
	restores                backupControllers.RestoreController
	backups                 backupControllers.BackupController
	backupEncryptionConfigs backupControllers.BackupEncryptionConfigController
	discoveryClient         discovery.DiscoveryInterface
	dynamicClient           dynamic.Interface
	sharedClientFactory     lasso.SharedClientFactory
	restmapper              meta.RESTMapper
}

type restoreObj struct {
	Name               string
	Namespace          string
	GVR                schema.GroupVersionResource
	ResourceConfigPath string
	Data               *unstructured.Unstructured
}

//var RestoreObjCreated = make(map[types.UID]map[*restoreObj]bool)
//var RestoreObjAdjacencyList = make(map[types.UID]map[*restoreObj][]restoreObj)

func Register(
	ctx context.Context,
	restores backupControllers.RestoreController,
	backups backupControllers.BackupController,
	backupEncryptionConfigs backupControllers.BackupEncryptionConfigController,
	clientSet *clientset.Clientset,
	dynamicInterface dynamic.Interface,
	sharedClientFactory lasso.SharedClientFactory,
	restmapper meta.RESTMapper) {

	controller := &handler{
		ctx:                     ctx,
		restores:                restores,
		backups:                 backups,
		backupEncryptionConfigs: backupEncryptionConfigs,
		dynamicClient:           dynamicInterface,
		discoveryClient:         clientSet.Discovery(),
		sharedClientFactory:     sharedClientFactory,
		restmapper:              restmapper,
	}

	// Register handlers
	restores.OnChange(ctx, "restore", controller.OnRestoreChange)
}

func (h *handler) OnRestoreChange(_ string, restore *v1.Restore) (*v1.Restore, error) {
	created := make(map[string]bool)
	ownerToDependentsList := make(map[string][]restoreObj)
	var toRestore []restoreObj
	numOwnerReferences := make(map[string]int)

	backupName := restore.Spec.BackupFileName

	backupPath, err := ioutil.TempDir("", strings.TrimSuffix(backupName, ".tar.gz"))
	if err != nil {
		return restore, err
	}
	logrus.Infof("Temporary path for un-tar/gzip backup data during restore: %v", backupPath)

	if restore.Spec.Local != "" {
		// if local, backup tar.gz must be added to the "Local" path
		backupFilePath := filepath.Join(restore.Spec.Local, backupName)
		if err := util.LoadFromTarGzip(backupFilePath, backupPath); err != nil {
			removeDirErr := os.RemoveAll(backupPath)
			if removeDirErr != nil {
				return restore, errors.New(err.Error() + removeDirErr.Error())
			}
			return restore, err
		}
	} else if restore.Spec.ObjectStore != nil {
		backupFilePath, err := h.downloadFromS3(restore)
		if err != nil {
			removeDirErr := os.RemoveAll(backupPath)
			if removeDirErr != nil {
				return restore, errors.New(err.Error() + removeDirErr.Error())
			}
			removeFileErr := os.Remove(backupFilePath)
			if removeFileErr != nil {
				return restore, errors.New(err.Error() + removeFileErr.Error())
			}
			return restore, err
		}
		if err := util.LoadFromTarGzip(backupFilePath, backupPath); err != nil {
			removeDirErr := os.RemoveAll(backupPath)
			if removeDirErr != nil {
				return restore, errors.New(err.Error() + removeDirErr.Error())
			}
			removeFileErr := os.Remove(backupFilePath)
			if removeFileErr != nil {
				return restore, errors.New(err.Error() + removeFileErr.Error())
			}
			return restore, err
		}
		// remove the downloaded gzip file from s3 as contents are untar/unzipped at the temp location by this point
		removeFileErr := os.Remove(backupFilePath)
		if removeFileErr != nil {
			return restore, errors.New(err.Error() + removeFileErr.Error())
		}
	}
	backupPath = strings.TrimSuffix(backupPath, ".tar.gz")
	logrus.Infof("Untar/Ungzip backup at %v", backupPath)
	config, err := h.backupEncryptionConfigs.Get(restore.Spec.EncryptionConfigNamespace, restore.Spec.EncryptionConfigName, k8sv1.GetOptions{})
	if err != nil {
		removeDirErr := os.RemoveAll(backupPath)
		if removeDirErr != nil {
			return restore, errors.New(err.Error() + removeDirErr.Error())
		}
		return restore, err
	}
	transformerMap, err := util.GetEncryptionTransformers(config)
	if err != nil {
		removeDirErr := os.RemoveAll(backupPath)
		if removeDirErr != nil {
			return restore, errors.New(err.Error() + removeDirErr.Error())
		}
		return restore, err
	}

	// first restore CRDs
	//_, err = os.Stat(filepath.Join(backupPath, "customresourcedefinitions.apiextensions.k8s.io#v1"))
	//if err == nil {
	startTime := time.Now()
	fmt.Printf("\nStart time: %v\n", startTime)
	if err := h.restoreCRDs(backupPath, transformerMap, created); err != nil {
		logrus.Errorf("\nerror during restoreCRDs: %v\n", err)
		removeDirErr := os.RemoveAll(backupPath)
		if removeDirErr != nil {
			return restore, errors.New(err.Error() + removeDirErr.Error())
		}
		panic(err)
		return restore, err
	}
	timeForRestoringCRDs := time.Since(startTime)
	fmt.Printf("\ntime taken to restore CRDs: %v\n", timeForRestoringCRDs)
	doneRestoringCRDTime := time.Now()

	// generate adjacency lists for dependents and ownerRefs
	if err := h.generateDependencyGraph(backupPath, transformerMap, ownerToDependentsList, &toRestore, numOwnerReferences); err != nil {
		logrus.Errorf("\nerror during generateDependencyGraph: %v\n", err)
		removeDirErr := os.RemoveAll(backupPath)
		if removeDirErr != nil {
			return restore, errors.New(err.Error() + removeDirErr.Error())
		}
		panic(err)
		return restore, err
	}
	timeForGeneratingGraph := time.Since(doneRestoringCRDTime)
	fmt.Printf("\ntime taken to generate graph: %v\n", timeForGeneratingGraph)
	doneGeneratingGraphTime := time.Now()
	logrus.Infof("No-goroutines-2 time right before starting to create from graph: %v", doneGeneratingGraphTime)
	if err := h.createFromDependencyGraph(ownerToDependentsList, created, numOwnerReferences, toRestore); err != nil {
		logrus.Errorf("\nerror during createFromDependencyGraph: %v\n", err)
		removeDirErr := os.RemoveAll(backupPath)
		if removeDirErr != nil {
			return restore, errors.New(err.Error() + removeDirErr.Error())
		}
		panic(err)
		return restore, err
	}
	timeForRestoringResources := time.Since(doneGeneratingGraphTime)
	fmt.Printf("\ntime taken to restore resources: %v\n", timeForRestoringResources)
	//err = os.RemoveAll(backupPath)

	//if dependentRestoreErr != nil {
	//	return restore, dependentRestoreErr
	//}

	if err := h.prune(strings.TrimSuffix(backupName, ".tar.gz"), backupPath, restore.Spec.ForcePruneTimeout, transformerMap); err != nil {
		panic(err)
		return restore, fmt.Errorf("error pruning during restore: %v", err)
	}
	logrus.Infof("Done restoring")
	if err := os.RemoveAll(backupPath); err != nil {
		return restore, err
	}
	return restore, nil
}

func (h *handler) restoreCRDs(backupPath string, transformerMap map[schema.GroupResource]value.Transformer, created map[string]bool) error {
	for _, resourceGVK := range []string{"customresourcedefinitions.apiextensions.k8s.io#v1", "customresourcedefinitions.apiextensions.k8s.io#v1beta1"} {
		resourceDirPath := path.Join(backupPath, resourceGVK)
		if _, err := os.Stat(resourceDirPath); err != nil && os.IsNotExist(err) {
			continue
		}
		gvr := getGVR(resourceGVK)
		gr := gvr.GroupResource()
		decryptionTransformer, _ := transformerMap[gr]
		dirContents, err := ioutil.ReadDir(resourceDirPath)
		if err != nil {
			return err
		}
		for _, resFile := range dirContents {
			resConfigPath := filepath.Join(resourceDirPath, resFile.Name())
			crdContent, err := ioutil.ReadFile(resConfigPath)
			if err != nil {
				return err
			}
			var crdData map[string]interface{}
			if err := json.Unmarshal(crdContent, &crdData); err != nil {
				return err
			}
			crdName := strings.TrimSuffix(resFile.Name(), ".json")
			if decryptionTransformer != nil {
				var encryptedBytes []byte
				if err := json.Unmarshal(crdContent, &encryptedBytes); err != nil {
					return err
				}
				decrypted, _, err := decryptionTransformer.TransformFromStorage(encryptedBytes, value.DefaultContext(crdName))
				if err != nil {
					return err
				}
				crdContent = decrypted
			}
			restoreObjKey := restoreObj{
				Name:               crdName,
				ResourceConfigPath: resConfigPath,
				GVR:                gvr,
				Data:               &unstructured.Unstructured{Object: crdData},
			}
			err = h.restoreResource(restoreObjKey, gvr)
			if err != nil {
				return fmt.Errorf("restoreCRDs: %v", err)
			}

			created[restoreObjKey.ResourceConfigPath] = true
		}
	}
	return nil
}

func (h *handler) generateDependencyGraph(backupPath string, transformerMap map[schema.GroupResource]value.Transformer,
	ownerToDependentsList map[string][]restoreObj, toRestore *[]restoreObj, numOwnerReferences map[string]int) error {
	backupEntries, err := ioutil.ReadDir(backupPath)
	if err != nil {
		return err
	}

	for _, backupEntry := range backupEntries {
		if backupEntry.Name() == "filters" {
			continue
		}

		// example catalogs.management.cattle.io#v3
		resourceGVK := backupEntry.Name()
		resourceDirPath := path.Join(backupPath, resourceGVK)
		gvr := getGVR(resourceGVK)
		gr := gvr.GroupResource()
		resourceFiles, err := ioutil.ReadDir(resourceDirPath)
		if err != nil {
			return err
		}

		for _, resourceFile := range resourceFiles {
			resManifestPath := filepath.Join(resourceDirPath, resourceFile.Name())
			if err := h.addToOwnersToDependentsList(backupPath, resManifestPath, resourceFile.Name(), gvr, transformerMap[gr],
				ownerToDependentsList, toRestore, numOwnerReferences); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h *handler) addToOwnersToDependentsList(backupPath, resConfigPath, aad string, gvr schema.GroupVersionResource, decryptionTransformer value.Transformer,
	ownerToDependentsList map[string][]restoreObj, toRestore *[]restoreObj, numOwnerReferences map[string]int) error {
	logrus.Infof("Processing %v for adjacency list", resConfigPath)
	resBytes, err := ioutil.ReadFile(resConfigPath)
	if err != nil {
		return err
	}

	if decryptionTransformer != nil {
		var encryptedBytes []byte
		if err := json.Unmarshal(resBytes, &encryptedBytes); err != nil {
			return err
		}
		decrypted, _, err := decryptionTransformer.TransformFromStorage(encryptedBytes, value.DefaultContext(aad))
		if err != nil {
			return err
		}
		resBytes = decrypted
	}

	fileMap := make(map[string]interface{})
	err = json.Unmarshal(resBytes, &fileMap)
	if err != nil {
		return err
	}

	metadata, metadataFound := fileMap[metadataMapKey].(map[string]interface{})
	if !metadataFound {
		return nil
	}

	// add to adjacency list
	name, _ := metadata["name"].(string)
	namespace, isNamespaced := metadata["namespace"].(string)
	currRestoreObj := restoreObj{
		Name:               name,
		ResourceConfigPath: resConfigPath,
		GVR:                gvr,
		Data:               &unstructured.Unstructured{Object: fileMap},
	}
	if isNamespaced {
		currRestoreObj.Namespace = namespace
	}

	ownerRefs, ownerRefsFound := metadata[ownerRefsMapKey].([]interface{})
	if !ownerRefsFound {
		// has no dependents, so no need to add to adjacency list, add to restoreResources list
		*toRestore = append(*toRestore, currRestoreObj)
		return nil
	}
	numOwners := 0
	for _, owner := range ownerRefs {
		numOwners++
		ownerRefData, ok := owner.(map[string]interface{})
		if !ok {
			logrus.Errorf("invalid ownerRef")
			continue
		}

		groupVersion := ownerRefData["apiVersion"].(string)
		gv, err := schema.ParseGroupVersion(groupVersion)
		if err != nil {
			logrus.Errorf(" err %v parsing ownerRef apiVersion", err)
			continue
		}
		kind := ownerRefData["kind"].(string)
		gvk := gv.WithKind(kind)
		ownerGVR, isNamespaced, err := h.sharedClientFactory.ResourceForGVK(gvk)
		if err != nil {
			return fmt.Errorf("Error getting resource for gvk %v: %v", gvk, err)
		}

		var apiGroup, version string
		split := strings.SplitN(groupVersion, "/", 2)
		if len(split) == 1 {
			// resources under v1 version
			version = split[0]
		} else {
			apiGroup = split[0]
			version = split[1]
		}
		// TODO: check if this object creation is needed
		// kind + "." + apigroup + "#" + version
		ownerDirPath := fmt.Sprintf("%s.%s#%s", ownerGVR.Resource, apiGroup, version)
		ownerName := ownerRefData["name"].(string)
		// Store resourceConfigPath of owner Ref because that's what we check for in "Created" map
		ownerObj := restoreObj{
			Name:               ownerName,
			ResourceConfigPath: filepath.Join(backupPath, ownerDirPath, ownerName+".json"),
			GVR:                ownerGVR,
		}
		if isNamespaced {
			// if owning object is namespaced, then it has to be the same ns as the current dependent object
			ownerObj.Namespace = currRestoreObj.Namespace
		}
		ownerObjDependents, ok := ownerToDependentsList[ownerObj.ResourceConfigPath]
		if !ok {
			ownerToDependentsList[ownerObj.ResourceConfigPath] = []restoreObj{currRestoreObj}
		} else {
			ownerToDependentsList[ownerObj.ResourceConfigPath] = append(ownerObjDependents, currRestoreObj)
		}
	}

	numOwnerReferences[currRestoreObj.ResourceConfigPath] = numOwners
	return nil
}

func (h *handler) createFromDependencyGraph(ownerToDependentsList map[string][]restoreObj, created map[string]bool,
	numOwnerReferences map[string]int, toRestore []restoreObj) error {
	numTotalDependents := 0
	for _, dependents := range ownerToDependentsList {
		numTotalDependents += len(dependents)
	}
	countRestored := 0
	var errList []error
	for len(toRestore) > 0 {
		curr := toRestore[0]
		if len(toRestore) == 1 {
			toRestore = []restoreObj{}
		} else {
			toRestore = toRestore[1:]
		}
		if created[curr.ResourceConfigPath] {
			logrus.Infof("Resource %v is already created", curr.ResourceConfigPath)
			continue
		}
		if err := h.restoreResource(curr, curr.GVR); err != nil {
			errList = append(errList, err)
			continue
		}
		for _, dependent := range ownerToDependentsList[curr.ResourceConfigPath] {
			// example, curr = catTemplate, dependent=catTempVer
			if numOwnerReferences[dependent.ResourceConfigPath] > 0 {
				numOwnerReferences[dependent.ResourceConfigPath]--
			}
			if numOwnerReferences[dependent.ResourceConfigPath] == 0 {
				logrus.Infof("dependent %v is now ready to create", dependent.Name)
				toRestore = append(toRestore, dependent)
			}
		}
		created[curr.ResourceConfigPath] = true
		countRestored++
	}
	fmt.Printf("\nTotal restored resources final: %v\n", countRestored)
	return util.ErrList(errList)
}

func (h *handler) updateOwnerRefs(ownerReferences []interface{}, namespace string) error {
	for ind, ownerRef := range ownerReferences {
		reference := ownerRef.(map[string]interface{})
		apiversion, _ := reference["apiVersion"].(string)
		kind, _ := reference["kind"].(string)
		if apiversion == "" || kind == "" {
			continue
		}
		ownerGV, err := schema.ParseGroupVersion(apiversion)
		if err != nil {
			return fmt.Errorf("err %v parsing apiversion %v", err, apiversion)
		}
		ownerGVK := ownerGV.WithKind(kind)
		name, _ := reference["name"].(string)

		ownerGVR, isNamespaced, err := h.sharedClientFactory.ResourceForGVK(ownerGVK)
		if err != nil {
			return fmt.Errorf("error getting resource for gvk %v: %v", ownerGVK, err)
		}
		ownerObj := &restoreObj{
			Name: name,
			GVR:  ownerGVR,
		}
		// if owner object is namespaced, it has to be within same namespace, since per definition
		/*
			// OwnerReference contains enough information to let you identify an owning
			// object. An owning object must be in the same namespace as the dependent, or
			// be cluster-scoped, so there is no namespace field.*/
		if isNamespaced {
			ownerObj.Namespace = namespace
		}

		logrus.Infof("Getting new UID for %v ", ownerObj.Name)
		ownerObjNewUID, err := h.getOwnerNewUID(ownerObj)
		if err != nil {
			return fmt.Errorf("error obtaining new UID for %v: %v", ownerObj.Name, err)
		}
		reference["uid"] = ownerObjNewUID
		ownerReferences[ind] = reference
	}
	return nil
}

func (h *handler) restoreResource(currRestoreObj restoreObj, gvr schema.GroupVersionResource) error {
	logrus.Infof("Restoring %v", currRestoreObj.Name)

	fileMap := currRestoreObj.Data.Object
	obj := currRestoreObj.Data

	fileMapMetadata := fileMap[metadataMapKey].(map[string]interface{})
	name := fileMapMetadata["name"].(string)
	namespace, _ := fileMapMetadata["namespace"].(string)
	var dr dynamic.ResourceInterface
	dr = h.dynamicClient.Resource(gvr)
	if namespace != "" {
		dr = h.dynamicClient.Resource(gvr).Namespace(namespace)
	}
	ownerReferences, _ := fileMapMetadata[ownerRefsMapKey].([]interface{})
	if ownerReferences != nil {
		if err := h.updateOwnerRefs(ownerReferences, namespace); err != nil {
			return err
		}
	}
	res, err := dr.Get(h.ctx, name, k8sv1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("restoreResource: err getting resource %v", err)
		}
		// create and return
		_, err := dr.Create(h.ctx, obj, k8sv1.CreateOptions{})
		if err != nil {
			return err
		}
		return nil
	}
	//fmt.Printf("res: %#v\n", res.Object)
	resMetadata := res.Object[metadataMapKey].(map[string]interface{})
	resourceVersion := resMetadata["resourceVersion"].(string)
	obj.Object[metadataMapKey].(map[string]interface{})["resourceVersion"] = resourceVersion
	_, err = dr.Update(h.ctx, obj, k8sv1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("restoreResource: err updating resource %v", err)
	}
	//h.discoveryClient.ServerResourcesForGroupVersion()
	//h.discoveryClient.
	//_, hasStatusSubresource := res.Object["status"]
	//if hasStatusSubresource {
	//	_, err := dr.UpdateStatus(h.ctx, obj, k8sv1.UpdateOptions{})
	//	if err != nil {
	//		logrus.Errorf("NOOO error in update status: %v", err)
	//		return fmt.Errorf("restoreResource: err updating status resource %v", err)
	//	}
	//}

	fmt.Printf("\nSuccessfully restored %v\n", name)
	return nil
}

func (h *handler) getOwnerNewUID(owner *restoreObj) (string, error) {
	var ownerDyn dynamic.ResourceInterface
	ownerDyn = h.dynamicClient.Resource(owner.GVR)

	if owner.Namespace != "" {
		ownerDyn = h.dynamicClient.Resource(owner.GVR).Namespace(owner.Namespace)
	}
	ownerObj, err := ownerDyn.Get(h.ctx, owner.Name, k8sv1.GetOptions{})
	if err != nil {
		return "", err
	}
	ownerObjMetadata := ownerObj.Object[metadataMapKey].(map[string]interface{})
	ownerObjUID := ownerObjMetadata["uid"].(string)
	return ownerObjUID, nil
}

func getGVR(resourceGVK string) schema.GroupVersionResource {
	gvkParts := strings.Split(resourceGVK, "#")
	version := gvkParts[1]
	resourceGroup := strings.SplitN(gvkParts[0], ".", 2)
	resource := strings.TrimSuffix(resourceGroup[0], ".")
	var group string
	if len(resourceGroup) > 1 {
		group = resourceGroup[1]
	}
	gr := schema.ParseGroupResource(resource + "." + group)
	gvr := gr.WithVersion(version)
	return gvr
}

func (h *handler) prune(backupName, backupPath string, pruneTimeout int, transformerMap map[schema.GroupResource]value.Transformer) error {
	// prune
	fmt.Printf("\nin prune!!\n")
	filtersBytes, err := ioutil.ReadFile(filepath.Join(backupPath, "filters", "filters.json"))
	if err != nil {
		return fmt.Errorf("error reading backup fitlers file: %v", err)
	}
	var backupFilters []v1.BackupFilter
	if err := json.Unmarshal(filtersBytes, &backupFilters); err != nil {
		return fmt.Errorf("error unmarshaling backup filters file: %v", err)
	}
	rh := util.ResourceHandler{
		DiscoveryClient: h.discoveryClient,
		DynamicClient:   h.dynamicClient,
	}
	pruneDirPath, err := ioutil.TempDir("", fmt.Sprintf("prune-%s", backupName))
	if err != nil {
		return err
	}
	logrus.Infof("Prune dir path is %s", pruneDirPath)

	if err := rh.GatherResources(h.ctx, backupFilters, pruneDirPath, transformerMap); err != nil {
		return err
	}
	logrus.Infof("Comparing prune and backup dirs")
	// compare pruneDirPath and backupPath contents, to find any extra files in pruneDirPath, and mark them for deletion
	namespacedResourcesToDelete := make(map[string]schema.GroupVersionResource)
	resourcesToDelete := make(map[string]schema.GroupVersionResource)
	walkFunc := func(currPath string, info os.FileInfo, err error) error {
		if info.IsDir() {
			return nil
		}
		// check if this file exists in backupPath or not
		// for example, for /var/tmp/authconfigs.management.cattle.io#v3/adfs.json,
		// containingDirFullPath = /var/tmp/authconfigs.management.cattle.io#v3
		containingDirFullPath := path.Dir(currPath)
		// containingDirBasePath = authconfigs.management.cattle.io#v3
		containingDirBasePath := filepath.Base(containingDirFullPath)
		// currFileName = authconfigs.management.cattle.io#v3/adfs.json => removes the path upto the dir for groupversion
		currFileName := filepath.Join(containingDirBasePath, filepath.Base(currPath))
		// if this file does not exist in the backup, it was created after taking backup, so delete it
		if _, err := os.Stat(filepath.Join(backupPath, currFileName)); os.IsNotExist(err) {
			gvr := getGVR(containingDirBasePath)
			isNamespaced, err := lasso.IsNamespaced(gvr, h.restmapper)
			if err != nil {
				logrus.Errorf("Error finding if %v is namespaced: %v", currFileName, err)
			}
			if isNamespaced {
				// use entire path as key, as we need to read this file to get the namespace
				namespacedResourcesToDelete[currPath] = gvr
			} else {
				// use only the filename without json ext as we can delete this resource without reading the file
				resourcesToDelete[strings.TrimSuffix(filepath.Base(currPath), ".json")] = gvr
			}

		}
		return nil
	}
	err = filepath.Walk(pruneDirPath, walkFunc)
	if err != nil {
		return err
	}
	logrus.Infof("Now Need to delete namespaced %v", namespacedResourcesToDelete)
	logrus.Infof("Now Need to delete clusterscoped %v", resourcesToDelete)

	for resourceName, gvr := range resourcesToDelete {
		dr := h.dynamicClient.Resource(gvr)
		if err := dr.Delete(h.ctx, resourceName, k8sv1.DeleteOptions{}); err != nil {
			return err
		}
	}

	for resourceFile, gvr := range namespacedResourcesToDelete {
		resourceBytes, err := ioutil.ReadFile(resourceFile)
		if err != nil {
			return err
		}
		var resourceContents map[string]interface{}
		if err := json.Unmarshal(resourceBytes, &resourceContents); err != nil {
			return err
		}
		metadata := resourceContents[metadataMapKey].(map[string]interface{})
		resourceName, nameFound := metadata["name"].(string)
		namespace, nsFound := metadata["namespace"].(string)
		if !nameFound || !nsFound {
			return fmt.Errorf("cannot delete resource as namespace not found")
		}
		dr := h.dynamicClient.Resource(gvr).Namespace(namespace)
		if err := dr.Delete(h.ctx, resourceName, k8sv1.DeleteOptions{}); err != nil {
			return err
		}
	}

	err = os.RemoveAll(pruneDirPath)
	return err
}
